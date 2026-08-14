package mail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	stdmail "net/mail"
	"os"
	"sort"
	"strings"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"
	imapclientv2 "github.com/emersion/go-imap/v2/imapclient"

	"icloud-api/internal/domain"
)

type archiveCandidate struct {
	uid          uint32
	internalDate time.Time
	rawSize      int64
	upstreamSeen bool
	aliasIDs     []int64
	parsed       parsedMessage
}

func (f *Fetcher) fetchArchiveIncremental(
	ctx context.Context,
	account domain.Account,
	password string,
	aliases []domain.Alias,
	previous *domain.IMAPSyncState,
	snapshotPositions map[int64]domain.MailboxSnapshotPosition,
	settings fetchSettings,
) (domain.MailboxSyncResult, error) {
	failure := domain.MailboxSyncResult{}
	if previous != nil {
		failure.State = *previous
	}
	if err := ctx.Err(); err != nil {
		return failure, err
	}
	if err := validateIMAPAccount(account, password); err != nil {
		return failure, err
	}
	aliasAddresses, err := prepareAliases(account, aliases, settings.maxAliases)
	if err != nil {
		return failure, err
	}
	if err := validateLegacySnapshotPositions(aliases, snapshotPositions); err != nil {
		return failure, err
	}
	host, address, username, err := accountEndpoint(account)
	if err != nil {
		return failure, err
	}

	domain.ReportMailboxSyncProgress(ctx, domain.MailboxSyncPhaseConnecting, 5)
	client, err := dialArchiveIMAP(ctx, address, host, settings)
	if err != nil {
		return failure, fmt.Errorf("connect IMAP %s: %w", address, err)
	}
	stopCancellation := make(chan struct{})
	cancellationStopped := make(chan struct{})
	go func() {
		defer close(cancellationStopped)
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-stopCancellation:
		}
	}()
	defer func() {
		close(stopCancellation)
		<-cancellationStopped
		_ = client.Close()
	}()

	domain.ReportMailboxSyncProgress(ctx, domain.MailboxSyncPhaseAuthenticating, 10)
	if err := client.Login(username, password).Wait(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return failure, ctxErr
		}
		return failure, fmt.Errorf("login IMAP account: %w", err)
	}
	domain.ReportMailboxSyncProgress(ctx, domain.MailboxSyncPhaseScanning, 15)
	mailbox, err := client.Select("INBOX", &imapv2.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return failure, ctxErr
		}
		return failure, fmt.Errorf("select INBOX read-only: %w", err)
	}
	if mailbox == nil || mailbox.UIDValidity == 0 || mailbox.UIDNext == 0 {
		return failure, errors.New("select INBOX read-only: invalid mailbox UID state")
	}

	syncedAt := settings.now().UTC()
	upperUID := uint32(mailbox.UIDNext - 1)
	reset := previous == nil || previous.AccountID != account.ID || previous.UIDValidity != mailbox.UIDValidity
	result := domain.MailboxSyncResult{
		State: domain.IMAPSyncState{
			AccountID:   account.ID,
			UIDValidity: mailbox.UIDValidity,
			LastUID:     upperUID,
			UpdatedAt:   syncedAt,
		},
		Reset:     reset,
		TargetUID: upperUID,
	}
	publish := func() (domain.MailboxSyncResult, error) {
		domain.ReportMailboxSyncProgress(ctx, domain.MailboxSyncPhaseValidating, 25)
		updates, err := reconcileLegacySnapshotPositions(
			ctx, client, snapshotPositions, mailbox.UIDValidity,
		)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return failure, ctxErr
			}
			return failure, fmt.Errorf("validate legacy mailbox snapshots: %w", err)
		}
		result.LegacySnapshotUpdates = updates
		return result, nil
	}
	// Accounts without enabled aliases do not need an archive cursor window;
	// advance directly to the observed upper bound while still reconciling any
	// legacy snapshots.
	if len(aliasAddresses) == 0 {
		domain.ReportMailboxSyncProgress(ctx, domain.MailboxSyncPhaseReading, 20)
		result.State.LastUID = upperUID
		return publish()
	}
	// A new installation or UIDVALIDITY generation establishes a bounded recent
	// window. The following incremental batch examines every UID after this
	// boundary, preserving the no-unbounded-backfill behavior while retaining
	// both upstream-read and upstream-unread messages in the v2 archive.
	if reset {
		lastUID, hasMore, err := establishArchiveRecentCursor(
			ctx, client, mailbox.NumMessages, upperUID, settings.maxCandidates,
		)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return failure, ctxErr
			}
			return failure, fmt.Errorf("establish recent mailbox cursor: %w", err)
		}
		result.State.LastUID = lastUID
		result.HasMore = hasMore
		domain.ReportMailboxSyncProgress(ctx, domain.MailboxSyncPhaseReading, 20)
		return publish()
	}
	if previous.LastUID > upperUID {
		return failure, fmt.Errorf("stored IMAP cursor UID %d exceeds mailbox upper UID %d", previous.LastUID, upperUID)
	}
	result.State.LastUID = previous.LastUID
	if previous.LastUID == upperUID {
		return publish()
	}

	uids, hasMore, processedThrough, err := discoverArchiveUIDs(client, previous.LastUID, upperUID, settings.maxIncrementalCandidates)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return failure, ctxErr
		}
		return failure, fmt.Errorf("discover new mailbox UIDs: %w", err)
	}
	result.State.LastUID = processedThrough
	result.HasMore = hasMore
	domain.ReportMailboxSyncProgress(ctx, domain.MailboxSyncPhaseReading, 20)
	if len(uids) == 0 {
		return publish()
	}

	candidates, err := fetchArchiveCandidateHeaders(
		client, uids, aliasAddresses, account.Email, settings,
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return failure, ctxErr
		}
		return failure, err
	}
	keepStagedMessages := false
	defer func() {
		if keepStagedMessages {
			return
		}
		for _, message := range result.ArchivedMessages {
			if message.RawMIMEPath != "" {
				_ = os.Remove(message.RawMIMEPath)
			}
		}
	}()
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return failure, err
		}
		archived, err := fetchArchivedMessage(client, candidate, account.ID, mailbox.UIDValidity, syncedAt, settings)
		if err != nil {
			return failure, err
		}
		result.ArchivedMessages = append(result.ArchivedMessages, archived)
	}
	published, err := publish()
	if err != nil {
		return failure, err
	}
	keepStagedMessages = true
	return published, nil
}

func validateLegacySnapshotPositions(
	aliases []domain.Alias,
	positions map[int64]domain.MailboxSnapshotPosition,
) error {
	enabled := make(map[int64]struct{}, len(aliases))
	for _, alias := range aliases {
		if alias.Enabled {
			enabled[alias.ID] = struct{}{}
		}
	}
	for aliasID, position := range positions {
		if _, ok := enabled[aliasID]; !ok {
			return fmt.Errorf("invalid legacy mailbox snapshot position: alias %d is not enabled", aliasID)
		}
		if position.AliasID != aliasID || position.UIDValidity == 0 || position.UID == 0 {
			return fmt.Errorf("invalid legacy mailbox snapshot position for alias %d", aliasID)
		}
	}
	return nil
}

// reconcileLegacySnapshotPositions validates every persisted legacy position
// in one UID FETCH. A missing UID is authoritative evidence of EXPUNGE; an
// older UIDVALIDITY is stale by definition. No headers or bodies are fetched,
// and the archive cursor is left untouched.
func reconcileLegacySnapshotPositions(
	ctx context.Context,
	client *imapclientv2.Client,
	positions map[int64]domain.MailboxSnapshotPosition,
	uidValidity uint32,
) (map[int64]domain.LatestMessage, error) {
	requested := make(map[uint32]struct{}, len(positions))
	set := imapv2.UIDSet{}
	for _, position := range positions {
		if position.UIDValidity != uidValidity {
			continue
		}
		if _, exists := requested[position.UID]; !exists {
			requested[position.UID] = struct{}{}
			set.AddNum(imapv2.UID(position.UID))
		}
	}
	if len(requested) == 0 {
		return legacySnapshotUpdatesForFoundUIDs(positions, uidValidity, nil), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	found := make(map[uint32]struct{}, len(requested))
	command := client.Fetch(set, &imapv2.FetchOptions{UID: true})
	defer command.Close()
	for message := command.Next(); message != nil; message = command.Next() {
		var uid uint32
		for item := message.Next(); item != nil; item = message.Next() {
			if item, ok := item.(imapclientv2.FetchItemDataUID); ok {
				uid = uint32(item.UID)
			}
		}
		if _, ok := requested[uid]; !ok || uid == 0 {
			return nil, fmt.Errorf("snapshot validation returned unexpected UID %d", uid)
		}
		if _, duplicate := found[uid]; duplicate {
			return nil, fmt.Errorf("snapshot validation returned duplicate UID %d", uid)
		}
		found[uid] = struct{}{}
	}
	if err := command.Close(); err != nil {
		return nil, fmt.Errorf("snapshot UID FETCH: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return legacySnapshotUpdatesForFoundUIDs(positions, uidValidity, found), nil
}

func legacySnapshotUpdatesForFoundUIDs(
	positions map[int64]domain.MailboxSnapshotPosition,
	uidValidity uint32,
	found map[uint32]struct{},
) map[int64]domain.LatestMessage {
	updates := make(map[int64]domain.LatestMessage)
	for aliasID, position := range positions {
		_, exists := found[position.UID]
		if position.UIDValidity != uidValidity || !exists {
			updates[aliasID] = emptyLegacySnapshot(aliasID, uidValidity)
		}
	}
	return updates
}

func emptyLegacySnapshot(aliasID int64, uidValidity uint32) domain.LatestMessage {
	return domain.LatestMessage{
		AliasID:       aliasID,
		UIDValidity:   uidValidity,
		SnapshotState: domain.SnapshotEmpty,
	}
}

func dialArchiveIMAP(ctx context.Context, address, serverName string, settings fetchSettings) (*imapclientv2.Client, error) {
	dialer := &net.Dialer{Timeout: settings.timeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	tlsConnection := tls.Client(connection, tlsConfig)
	handshakeContext, cancel := context.WithTimeout(ctx, settings.timeout)
	defer cancel()
	if err := tlsConnection.HandshakeContext(handshakeContext); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return imapclientv2.New(tlsConnection, &imapclientv2.Options{WordDecoder: headerWordDecoder}), nil
}

// discoverArchiveUIDs examines a bounded oldest sequence window after the
// committed UID. It returns every UID in that window, including messages that
// are already \Seen upstream. Sequence probes keep the response bounded for
// sparse UID spaces; the leading/trailing sentinels detect EXPUNGE races.
func discoverArchiveUIDs(client *imapclientv2.Client, lastUID, upperUID uint32, limit int) ([]uint32, bool, uint32, error) {
	if limit < 1 {
		return nil, false, lastUID, errors.New("incremental UID limit must be positive")
	}
	if lastUID > upperUID {
		return nil, false, lastUID, fmt.Errorf("stored cursor UID %d exceeds mailbox upper UID %d", lastUID, upperUID)
	}
	if lastUID == upperUID {
		return nil, false, upperUID, nil
	}

	mailbox := client.Mailbox()
	if mailbox == nil || mailbox.NumMessages == 0 {
		return nil, false, upperUID, nil
	}
	numMessages := mailbox.NumMessages
	// A small numeric interval can be searched directly. SEARCH is only used
	// for range discovery here; unlike the legacy path, it deliberately has no
	// \Seen predicate because v2 archives both read and unread mail.
	if uint64(upperUID)-uint64(lastUID) <= uint64(limit) {
		set := imapv2.UIDSet{}
		set.AddRange(imapv2.UID(lastUID+1), imapv2.UID(upperUID))
		data, err := client.UIDSearch(&imapv2.SearchCriteria{UID: []imapv2.UIDSet{set}}, nil).Wait()
		if err != nil {
			return nil, false, lastUID, err
		}
		uids, err := validateArchiveUIDSearch(data.AllUIDs(), lastUID, upperUID)
		if err != nil {
			return nil, false, lastUID, err
		}
		return uids, false, upperUID, nil
	}

	firstSequence, err := findArchiveFirstSequenceAfterUID(client, numMessages, lastUID, upperUID)
	if err != nil {
		return nil, false, lastUID, err
	}
	if firstSequence > uint64(numMessages) {
		return nil, false, upperUID, nil
	}
	first := uint32(firstSequence)
	rangeFirst := first
	if rangeFirst > 1 {
		rangeFirst--
	}
	rangeLast := uint64(numMessages)
	remaining := uint64(numMessages) - firstSequence + 1
	if uint64(limit) < remaining {
		rangeLast = firstSequence + uint64(limit) - 1
	}

	var trailingUID uint32
	trailingSequence := uint32(0)
	if rangeLast < uint64(numMessages) {
		trailingSequence = uint32(rangeLast + 1)
		trailing, fetchErr := fetchArchiveSequenceUIDs(client, trailingSequence, trailingSequence)
		if fetchErr != nil {
			return nil, false, lastUID, fmt.Errorf("read incremental trailing sequence sentinel: %w", fetchErr)
		}
		trailingUID = trailing[0]
	}
	discovered, err := fetchArchiveSequenceUIDFlags(client, rangeFirst, uint32(rangeLast))
	if err != nil {
		return nil, false, lastUID, err
	}
	if trailingSequence != 0 {
		trailing, fetchErr := fetchArchiveSequenceUIDs(client, trailingSequence, trailingSequence)
		if fetchErr != nil {
			return nil, false, lastUID, fmt.Errorf("recheck incremental trailing sequence sentinel: %w", fetchErr)
		}
		if trailing[0] != trailingUID {
			return nil, false, lastUID, fmt.Errorf(
				"mailbox trailing sequence sentinel changed from UID %d to UID %d",
				trailingUID, trailing[0],
			)
		}
	}
	if len(discovered) == 0 {
		return nil, false, lastUID, errors.New("incremental sequence range returned no messages")
	}
	leadingUID := discovered[0].uid
	if first > 1 && leadingUID > lastUID {
		return nil, false, lastUID, errors.New("mailbox sequence boundary changed before batch fetch")
	}
	leading, fetchErr := fetchArchiveSequenceUIDs(client, rangeFirst, rangeFirst)
	if fetchErr != nil {
		return nil, false, lastUID, fmt.Errorf("recheck mailbox leading sequence: %w", fetchErr)
	}
	if len(leading) != 1 || leading[0] != leadingUID {
		return nil, false, lastUID, errors.New("mailbox sequence boundary changed after batch fetch")
	}
	if first > 1 {
		// The first response is a sentinel at the committed boundary; it is not
		// part of the new UID batch.
		discovered = discovered[1:]
	}
	if len(discovered) == 0 {
		return nil, false, lastUID, errors.New("incremental sequence range contained only boundary sentinel")
	}
	for _, message := range discovered {
		if message.uid <= lastUID || message.uid > upperUID {
			return nil, false, lastUID, fmt.Errorf("incremental sequence batch returned unexpected UID %d", message.uid)
		}
	}
	hasMore := rangeLast < uint64(numMessages)
	processedThrough := upperUID
	if hasMore {
		processedThrough = discovered[len(discovered)-1].uid
	}
	uids := make([]uint32, len(discovered))
	for index, message := range discovered {
		uids[index] = message.uid
	}
	return uids, hasMore, processedThrough, nil
}

func validateArchiveUIDSearch(discovered []imapv2.UID, lastUID, upperUID uint32) ([]uint32, error) {
	uids := make([]uint32, 0, len(discovered))
	seen := make(map[uint32]struct{}, len(discovered))
	for _, raw := range discovered {
		uid := uint32(raw)
		if uid <= lastUID || uid > upperUID {
			return nil, fmt.Errorf("server returned unexpected UID %d", uid)
		}
		if _, duplicate := seen[uid]; duplicate {
			return nil, fmt.Errorf("server returned duplicate UID %d", uid)
		}
		seen[uid] = struct{}{}
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	return uids, nil
}

type archiveSequenceMessage struct {
	sequence uint32
	uid      uint32
	seen     bool
}

func findArchiveFirstSequenceAfterUID(client *imapclientv2.Client, numMessages uint32, lastUID, upperUID uint32) (uint64, error) {
	if numMessages == 0 {
		return 1, nil
	}
	low, high := uint64(1), uint64(numMessages)+1
	probe := func(sequence uint64) (uint32, error) {
		uids, err := fetchArchiveSequenceUIDs(client, uint32(sequence), uint32(sequence))
		if err != nil {
			return 0, err
		}
		if len(uids) != 1 {
			return 0, fmt.Errorf("sequence %d returned %d UIDs", sequence, len(uids))
		}
		uid := uids[0]
		if uid > upperUID {
			return 0, fmt.Errorf("sequence %d returned UID %d beyond selected upper UID %d", sequence, uid, upperUID)
		}
		return uid, nil
	}
	guess := uint64(lastUID) + 1
	if guess > uint64(numMessages) {
		guess = uint64(numMessages)
	}
	if guess >= 1 {
		uid, err := probe(guess)
		if err != nil {
			return 0, err
		}
		if uid == lastUID+1 {
			return guess, nil
		}
		if uid <= lastUID {
			low = guess + 1
		} else {
			high = guess
		}
	}
	for low < high {
		middle := low + (high-low)/2
		uid, err := probe(middle)
		if err != nil {
			return 0, err
		}
		if uid <= lastUID {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return low, nil
}

func fetchArchiveSequenceUIDs(client *imapclientv2.Client, first, last uint32) ([]uint32, error) {
	messages, err := fetchArchiveSequenceRange(client, first, last, false)
	if err != nil {
		return nil, err
	}
	uids := make([]uint32, len(messages))
	for index, message := range messages {
		uids[index] = message.uid
	}
	return uids, nil
}

func fetchArchiveSequenceUIDFlags(client *imapclientv2.Client, first, last uint32) ([]archiveSequenceMessage, error) {
	return fetchArchiveSequenceRange(client, first, last, true)
}

func fetchArchiveSequenceRange(client *imapclientv2.Client, first, last uint32, includeFlags bool) ([]archiveSequenceMessage, error) {
	if first == 0 || last < first {
		return nil, fmt.Errorf("invalid mailbox sequence range %d:%d", first, last)
	}
	want := uint64(last) - uint64(first) + 1
	if want > uint64(maxSequenceFetchMessages) {
		return nil, fmt.Errorf("fetch sequence range %d:%d exceeds bounded response limit %d", first, last, maxSequenceFetchMessages)
	}
	options := &imapv2.FetchOptions{UID: true, Flags: includeFlags}
	sequenceSet := imapv2.SeqSet{}
	sequenceSet.AddRange(first, last)
	command := client.Fetch(sequenceSet, options)
	defer command.Close()
	messages := make([]archiveSequenceMessage, 0, int(want))
	seenSequences := make(map[uint32]struct{}, int(want))
	seenUIDs := make(map[uint32]struct{}, int(want))
	for message := command.Next(); message != nil; message = command.Next() {
		item := archiveSequenceMessage{sequence: message.SeqNum}
		for data := message.Next(); data != nil; data = message.Next() {
			switch data := data.(type) {
			case imapclientv2.FetchItemDataUID:
				item.uid = uint32(data.UID)
			case imapclientv2.FetchItemDataFlags:
				for _, flag := range data.Flags {
					if flag == imapv2.FlagSeen {
						item.seen = true
						break
					}
				}
			}
		}
		if item.sequence < first || item.sequence > last || item.uid == 0 {
			return nil, fmt.Errorf("fetch sequence range %d:%d returned invalid message", first, last)
		}
		if _, duplicate := seenSequences[item.sequence]; duplicate {
			return nil, fmt.Errorf("fetch sequence range %d:%d returned duplicate sequence %d", first, last, item.sequence)
		}
		if _, duplicate := seenUIDs[item.uid]; duplicate {
			return nil, fmt.Errorf("fetch sequence range %d:%d returned duplicate UID %d", first, last, item.uid)
		}
		seenSequences[item.sequence] = struct{}{}
		seenUIDs[item.uid] = struct{}{}
		messages = append(messages, item)
	}
	if err := command.Close(); err != nil {
		return nil, fmt.Errorf("fetch sequence range %d:%d: %w", first, last, err)
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].sequence < messages[j].sequence })
	if uint64(len(messages)) != want {
		return nil, fmt.Errorf("fetch sequence range %d:%d returned an unstable mailbox view", first, last)
	}
	for index, message := range messages {
		if message.sequence != first+uint32(index) || (index > 0 && message.uid <= messages[index-1].uid) {
			return nil, fmt.Errorf("fetch sequence range %d:%d returned an unstable mailbox view", first, last)
		}
	}
	return messages, nil
}

// establishArchiveRecentCursor returns the UID immediately before a bounded
// recent sequence window. It intentionally advances only the account cursor;
// the next incremental batch examines all UIDs and fetches content. This
// mirrors the legacy fetcher's reset boundary and avoids backfilling an entire
// existing mailbox when an alias is newly created or re-enabled.
func establishArchiveRecentCursor(
	ctx context.Context,
	client *imapclientv2.Client,
	numMessages uint32,
	upperUID uint32,
	limit int,
) (uint32, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if limit < 1 {
		return 0, false, errors.New("incremental UID limit must be positive")
	}
	if numMessages == 0 {
		return upperUID, false, nil
	}
	if uint64(numMessages) <= uint64(limit) {
		return 0, true, nil
	}
	// Anchor the tail before and after locating the boundary. A stable tail is
	// needed because sequence numbers can shift when an EXPUNGE interleaves with
	// the boundary FETCH; on any mismatch the caller must retry without moving
	// the persisted cursor.
	tailSequence := numMessages
	tailBefore, err := fetchArchiveSequenceUIDs(client, tailSequence, tailSequence)
	if err != nil {
		return 0, false, fmt.Errorf("anchor recent mailbox window: %w", err)
	}
	if len(tailBefore) != 1 || tailBefore[0] == 0 || tailBefore[0] > upperUID {
		return 0, false, errors.New("anchor recent mailbox window: invalid trailing UID")
	}
	boundarySequence := numMessages - uint32(limit)
	boundary, err := fetchArchiveSequenceUIDs(client, boundarySequence, boundarySequence)
	if err != nil {
		return 0, false, fmt.Errorf("locate recent mailbox window: %w", err)
	}
	if len(boundary) != 1 || boundary[0] == 0 || boundary[0] >= tailBefore[0] {
		return 0, false, fmt.Errorf(
			"recent mailbox boundary sequence %d returned invalid UID %d (tail UID %d, upper UID %d)",
			boundarySequence, func() uint32 {
				if len(boundary) == 1 {
					return boundary[0]
				}
				return 0
			}(), tailBefore[0], upperUID,
		)
	}
	tailAfter, err := fetchArchiveSequenceUIDs(client, tailSequence, tailSequence)
	if err != nil {
		return 0, false, fmt.Errorf("recheck recent mailbox window anchor: %w", err)
	}
	if len(tailAfter) != 1 || tailAfter[0] != tailBefore[0] {
		return 0, false, errors.New("recent mailbox window changed while establishing cursor")
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	return boundary[0], true, nil
}

func fetchArchiveCandidateHeaders(
	client *imapclientv2.Client,
	uids []uint32,
	aliases map[string][]int64,
	accountEmail string,
	settings fetchSettings,
) ([]archiveCandidate, error) {
	fields := recipientHeaderFieldsForFetch()
	fields = append(fields, "Message-ID", "Date", "From", "Subject")
	fields = uniqueHeaderFields(fields)
	section := &imapv2.FetchItemBodySection{
		Specifier:    imapv2.PartSpecifierHeader,
		HeaderFields: fields,
		Partial:      &imapv2.SectionPartial{Offset: 0, Size: int64(settings.maxHeaderBytes) + 1},
		Peek:         true,
	}
	set := imapv2.UIDSet{}
	for _, uid := range uids {
		set.AddNum(imapv2.UID(uid))
	}
	command := client.Fetch(set, &imapv2.FetchOptions{
		UID:          true,
		Flags:        true,
		InternalDate: true,
		RFC822Size:   true,
		BodySection:  []*imapv2.FetchItemBodySection{section},
	})
	defer command.Close()
	requested := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		requested[uid] = struct{}{}
	}
	seen := make(map[uint32]struct{}, len(uids))
	var candidates []archiveCandidate
	for message := command.Next(); message != nil; message = command.Next() {
		var candidate archiveCandidate
		var header []byte
		headerTruncated := false
		for item := message.Next(); item != nil; item = message.Next() {
			switch item := item.(type) {
			case imapclientv2.FetchItemDataUID:
				candidate.uid = uint32(item.UID)
			case imapclientv2.FetchItemDataInternalDate:
				candidate.internalDate = item.Time
			case imapclientv2.FetchItemDataRFC822Size:
				candidate.rawSize = item.Size
			case imapclientv2.FetchItemDataFlags:
				for _, flag := range item.Flags {
					if flag == imapv2.FlagSeen {
						candidate.upstreamSeen = true
						break
					}
				}
			case imapclientv2.FetchItemDataBodySection:
				if item.Literal == nil {
					continue
				}
				if item.Literal.Size() > int64(settings.maxHeaderBytes) {
					headerTruncated = true
				}
				var readErr error
				header, readErr = io.ReadAll(io.LimitReader(item.Literal, int64(settings.maxHeaderBytes)+1))
				if readErr != nil {
					return nil, fmt.Errorf("read candidate header: %w", readErr)
				}
				_, _ = io.Copy(io.Discard, item.Literal)
				if len(header) > settings.maxHeaderBytes {
					header = header[:settings.maxHeaderBytes]
					headerTruncated = true
				}
			}
		}
		if _, ok := requested[candidate.uid]; !ok || candidate.uid == 0 {
			return nil, fmt.Errorf("fetch candidate headers returned unexpected UID %d", candidate.uid)
		}
		if _, duplicate := seen[candidate.uid]; duplicate {
			return nil, fmt.Errorf("fetch candidate headers returned duplicate UID %d", candidate.uid)
		}
		seen[candidate.uid] = struct{}{}
		if headerTruncated || len(header) == 0 {
			continue
		}
		parsedHeader, err := stdmail.ReadMessage(bytes.NewReader(header))
		if err != nil {
			continue
		}
		candidate.aliasIDs, _ = classifyArchiveRecipientAliases(parsedHeader.Header, aliases, accountEmail, settings.allowWeakRecipientHeaders)
		if len(candidate.aliasIDs) == 0 {
			continue
		}
		candidate.parsed, err = parseMIMEMessageWithOptions(
			header,
			defaultMIMELimits(0, defaultMetadataResultBytes),
			true,
		)
		if err != nil {
			continue
		}
		if candidate.internalDate.IsZero() {
			candidate.internalDate = time.Now().UTC()
		}
		candidates = append(candidates, candidate)
	}
	if err := command.Close(); err != nil {
		return nil, fmt.Errorf("fetch candidate headers: %w", err)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].uid < candidates[j].uid })
	return candidates, nil
}

func classifyArchiveRecipientAliases(
	header stdmail.Header,
	aliases map[string][]int64,
	accountEmail string,
	allowWeak bool,
) ([]int64, bool) {
	if _, present, _ := parseICloudHMERoute(header); present {
		aliasID, determinate := classifyRecipientAlias(header, aliases, accountEmail, allowWeak)
		if !determinate || aliasID == 0 {
			return nil, determinate
		}
		return []int64{aliasID}, true
	}
	fields := strongRecipientHeaderFields
	if !hasAnyHeader(header, fields) {
		if !allowWeak {
			return nil, true
		}
		fields = weakRecipientHeaderFields
	}
	addresses, valid := addressesFromHeaders(header, fields)
	if !valid {
		return nil, false
	}
	seen := make(map[int64]struct{})
	for _, address := range addresses {
		for _, aliasID := range aliases[address] {
			if aliasID > 0 {
				seen[aliasID] = struct{}{}
			}
		}
	}
	result := make([]int64, 0, len(seen))
	for aliasID := range seen {
		result = append(result, aliasID)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, true
}

func fetchArchivedMessage(
	client *imapclientv2.Client,
	candidate archiveCandidate,
	accountID int64,
	uidValidity uint32,
	syncedAt time.Time,
	settings fetchSettings,
) (domain.ArchivedMessage, error) {
	archived := domain.ArchivedMessage{
		AccountID:    accountID,
		UIDValidity:  uidValidity,
		UID:          candidate.uid,
		MessageID:    candidate.parsed.messageID,
		InternalDate: candidate.internalDate,
		HeaderDate:   candidate.parsed.headerDate,
		From:         candidate.parsed.from,
		To:           candidate.parsed.to,
		CC:           candidate.parsed.cc,
		Subject:      candidate.parsed.subject,
		RawSize:      candidate.rawSize,
		UpstreamSeen: candidate.upstreamSeen,
		AliasIDs:     append([]int64(nil), candidate.aliasIDs...),
		SyncedAt:     syncedAt,
	}
	if messageExceedsArchiveLimit(candidate.rawSize, settings.maxMessageBytes) {
		archived.ContentState = domain.ArchiveContentOversized
		archived.OTP = ExtractOTP(archived.Subject, "", "")
		return archived, nil
	}
	if settings.archiveTempDir == "" {
		return domain.ArchivedMessage{}, errors.New("mail archive temporary directory is not configured")
	}
	if err := os.MkdirAll(settings.archiveTempDir, 0o700); err != nil {
		return domain.ArchivedMessage{}, fmt.Errorf("create mail archive temporary directory: %w", err)
	}

	section := &imapv2.FetchItemBodySection{Peek: true}
	set := imapv2.UIDSetNum(imapv2.UID(candidate.uid))
	command := client.Fetch(set, &imapv2.FetchOptions{
		UID:          true,
		InternalDate: true,
		RFC822Size:   true,
		BodySection:  []*imapv2.FetchItemBodySection{section},
	})
	defer command.Close()
	var found bool
	var literalSize int64
	var temporaryPath string
	var digestText string
	for message := command.Next(); message != nil; message = command.Next() {
		var uid uint32
		for item := message.Next(); item != nil; item = message.Next() {
			switch item := item.(type) {
			case imapclientv2.FetchItemDataUID:
				uid = uint32(item.UID)
			case imapclientv2.FetchItemDataInternalDate:
				if !item.Time.IsZero() {
					archived.InternalDate = item.Time
				}
			case imapclientv2.FetchItemDataRFC822Size:
				archived.RawSize = item.Size
			case imapclientv2.FetchItemDataBodySection:
				if item.Literal == nil {
					continue
				}
				found = true
				literalSize = item.Literal.Size()
				if messageExceedsArchiveLimit(literalSize, settings.maxMessageBytes) {
					_, _ = io.Copy(io.Discard, item.Literal)
					archived.ContentState = domain.ArchiveContentOversized
					continue
				}
				path, digest, writeErr := writeArchiveLiteral(settings.archiveTempDir, item.Literal, literalSize)
				if writeErr != nil {
					return domain.ArchivedMessage{}, writeErr
				}
				temporaryPath, digestText = path, digest
			}
		}
		if uid != candidate.uid {
			if temporaryPath != "" {
				_ = os.Remove(temporaryPath)
			}
			return domain.ArchivedMessage{}, fmt.Errorf("fetch archived message returned unexpected UID %d", uid)
		}
	}
	if err := command.Close(); err != nil {
		if temporaryPath != "" {
			_ = os.Remove(temporaryPath)
		}
		return domain.ArchivedMessage{}, fmt.Errorf("fetch archived message UID %d: %w", candidate.uid, err)
	}
	if !found {
		return domain.ArchivedMessage{}, fmt.Errorf("fetch archived message UID %d returned no literal", candidate.uid)
	}
	if archived.ContentState == domain.ArchiveContentOversized {
		if temporaryPath != "" {
			_ = os.Remove(temporaryPath)
		}
		archived.RawSize = max(archived.RawSize, literalSize)
		archived.OTP = ExtractOTP(archived.Subject, "", "")
		return archived, nil
	}
	archived.RawMIMEPath = temporaryPath
	archived.RawSize = literalSize
	archived.RawSHA256 = digestText
	archived.ContentState = domain.ArchiveContentAvailable
	file, err := os.Open(temporaryPath)
	if err != nil {
		_ = os.Remove(temporaryPath)
		return domain.ArchivedMessage{}, fmt.Errorf("open staged MIME for parsing: %w", err)
	}
	limits := defaultMIMELimits(int64(settings.maxBodyBytes), int64(settings.maxParsedMessageBytes))
	parsed, parseErr := parseMIMEMessageReader(file, literalSize, limits)
	closeErr := file.Close()
	if parseErr != nil {
		_ = os.Remove(temporaryPath)
		return domain.ArchivedMessage{}, parseErr
	}
	if closeErr != nil {
		_ = os.Remove(temporaryPath)
		return domain.ArchivedMessage{}, closeErr
	}
	projectParsedArchiveContent(&archived, parsed)
	archived.OTP = ExtractOTP(parsed.subject, parsed.textBody, parsed.htmlBody)
	return archived, nil
}

func projectParsedArchiveContent(archived *domain.ArchivedMessage, parsed parsedMessage) {
	archived.MessageID = parsed.messageID
	archived.HeaderDate = parsed.headerDate
	archived.From = parsed.from
	archived.To = parsed.to
	archived.CC = parsed.cc
	archived.Subject = parsed.subject
	archived.TextBody = parsed.textBody
	archived.HTMLBody = parsed.htmlBody
	archived.Attachments = append([]domain.Attachment(nil), parsed.attachments...)
	archived.BodyTruncated = parsed.bodyTruncated
}

func messageExceedsArchiveLimit(size int64, limit int) bool {
	return size > int64(limit)
}

func writeArchiveLiteral(directory string, literal imapv2.LiteralReader, expected int64) (string, string, error) {
	file, err := os.CreateTemp(directory, "sync-*.eml.tmp")
	if err != nil {
		return "", "", fmt.Errorf("create staged MIME: %w", err)
	}
	path := file.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", "", fmt.Errorf("protect staged MIME: %w", err)
	}
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, digest), literal)
	if err == nil && written != expected {
		err = fmt.Errorf("MIME literal size changed: wrote %d of %d bytes", written, expected)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", "", fmt.Errorf("write staged MIME: %w", err)
	}
	keep = true
	return path, hex.EncodeToString(digest.Sum(nil)), nil
}

func uniqueHeaderFields(fields []string) []string {
	seen := make(map[string]struct{}, len(fields))
	result := make([]string, 0, len(fields))
	for _, field := range fields {
		key := strings.ToLower(strings.TrimSpace(field))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, field)
	}
	return result
}

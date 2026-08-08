package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	stdmail "net/mail"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"

	"icloud-api/internal/domain"
)

const (
	defaultIMAPHost            = "imap.mail.me.com"
	defaultIMAPPort            = 993
	defaultIMAPTimeout         = 25 * time.Second
	defaultMaxAliases          = domain.MaxEnabledAliasesPerAccount
	defaultMaxCandidates       = 1024
	defaultMaxHeaderBytes      = 128 << 10
	defaultMaxMessageBytes     = 10 << 20
	defaultMaxBodyBytes        = 1 << 20
	defaultMaxFetchResultBytes = 64 << 20
	candidateHeaderFetchBatch  = 64
	messageFetchBatch          = 8
)

var (
	ErrAccountDisabled      = errors.New("mail account is disabled")
	ErrAliasAccountMismatch = errors.New("alias does not belong to account")
	ErrInvalidAlias         = errors.New("invalid alias")
	ErrInvalidIMAPConfig    = errors.New("invalid IMAP configuration")
	ErrTooManyAliases       = errors.New("too many aliases")
)

type imapSession interface {
	Login(username, password string) error
	Select(name string, readOnly bool) (*imap.MailboxStatus, error)
	Fetch(seqset *imap.SeqSet, items []imap.FetchItem, ch chan *imap.Message) error
	UidSearch(criteria *imap.SearchCriteria) ([]uint32, error)
	UidFetch(seqset *imap.SeqSet, items []imap.FetchItem, ch chan *imap.Message) error
	Logout() error
	Terminate() error
}

type dialSessionFunc func(ctx context.Context, address, serverName string, timeout time.Duration) (imapSession, error)

// Fetcher retrieves the latest message delivered to each alias through one
// account-level IMAP connection. All connections use implicit TLS.
type Fetcher struct {
	IMAPTimeout               time.Duration
	MaxAliases                int
	MaxCandidates             int
	MaxHeaderBytes            int
	MaxMessageBytes           int
	MaxBodyBytes              int
	MaxParsedMessageBytes     int
	MaxFetchResultBytes       int
	AllowWeakRecipientHeaders bool

	dial dialSessionFunc
	now  func() time.Time
}

// NewFetcher returns a fetcher with production defaults. Its limits can be
// overwritten directly, for example from the application configuration.
func NewFetcher() *Fetcher {
	return &Fetcher{
		IMAPTimeout:     defaultIMAPTimeout,
		MaxAliases:      defaultMaxAliases,
		MaxCandidates:   defaultMaxCandidates,
		MaxHeaderBytes:  defaultMaxHeaderBytes,
		MaxMessageBytes: defaultMaxMessageBytes,
		MaxBodyBytes:    defaultMaxBodyBytes,
		dial:            dialIMAPTLS,
		now:             time.Now,
	}
}

// FetchLatest performs an authoritative bounded snapshot without a persisted
// cursor. It remains as a compatibility wrapper around FetchIncremental.
func (f *Fetcher) FetchLatest(ctx context.Context, account domain.Account, password string, aliases []domain.Alias) (map[int64]domain.LatestMessage, error) {
	result, err := f.FetchIncremental(ctx, account, password, aliases, nil, nil)
	if err != nil {
		return nil, err
	}
	return result.Messages, nil
}

// FetchIncremental reads one bounded account-level UID window and classifies
// its recipients locally. It never performs one IMAP query per alias.
func (f *Fetcher) FetchIncremental(
	ctx context.Context,
	account domain.Account,
	password string,
	aliases []domain.Alias,
	previous *domain.IMAPSyncState,
	snapshotPositions map[int64]domain.MailboxSnapshotPosition,
) (domain.MailboxSyncResult, error) {
	settings := f.settings()
	failure := domain.MailboxSyncResult{}
	if previous != nil {
		failure.State = *previous
	}
	if err := ctx.Err(); err != nil {
		return failure, err
	}
	if !account.Enabled {
		return failure, ErrAccountDisabled
	}
	if password == "" {
		return failure, fmt.Errorf("%w: empty password", ErrInvalidIMAPConfig)
	}

	aliasAddresses, err := prepareAliases(account, aliases, settings.maxAliases)
	if err != nil {
		return failure, err
	}
	if err := validateSnapshotPositions(aliases, snapshotPositions); err != nil {
		return failure, err
	}

	host, address, username, err := accountEndpoint(account)
	if err != nil {
		return failure, err
	}
	session, err := settings.dial(ctx, address, host, settings.timeout)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return failure, ctxErr
		}
		return failure, fmt.Errorf("connect IMAP %s: %w", address, err)
	}

	stopCancellation := make(chan struct{})
	cancellationStopped := make(chan struct{})
	go func() {
		defer close(cancellationStopped)
		select {
		case <-ctx.Done():
			_ = session.Terminate()
		case <-stopCancellation:
		}
	}()
	defer func() {
		close(stopCancellation)
		<-cancellationStopped
		_ = session.Terminate()
	}()

	if err := session.Login(username, password); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return failure, ctxErr
		}
		return failure, fmt.Errorf("login IMAP account: %w", err)
	}
	mailbox, err := session.Select("INBOX", true)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return failure, ctxErr
		}
		return failure, fmt.Errorf("select INBOX read-only: %w", err)
	}
	if mailbox == nil {
		return failure, errors.New("select INBOX read-only: empty mailbox status")
	}
	uidValidity := mailbox.UidValidity
	if uidValidity == 0 {
		return failure, errors.New("select INBOX read-only: UIDVALIDITY is zero")
	}
	if mailbox.UidNext == 0 {
		return failure, errors.New("select INBOX read-only: UIDNEXT is zero")
	}

	syncedAt := settings.now().UTC()
	upperUID := mailbox.UidNext - 1
	reset := previous == nil || previous.AccountID != account.ID || previous.UIDValidity != uidValidity
	if !reset {
		if previous.LastUID > upperUID {
			return failure, fmt.Errorf(
				"stored IMAP cursor UID %d exceeds mailbox upper UID %d",
				previous.LastUID,
				upperUID,
			)
		}
	}

	result := domain.MailboxSyncResult{
		Messages: make(map[int64]domain.LatestMessage),
		State: domain.IMAPSyncState{
			AccountID:   account.ID,
			UIDValidity: uidValidity,
			LastUID:     upperUID,
			UpdatedAt:   syncedAt,
		},
		Reset: reset,
	}
	if !reset {
		result.State.LastUID = previous.LastUID
	}
	preserveExistingSnapshots := !reset || previous == nil || previous.UIDValidity == uidValidity
	publish := func() (domain.MailboxSyncResult, error) {
		if err := validatePublishUIDs(
			ctx,
			session,
			snapshotPositions,
			result.Messages,
			uidValidity,
			preserveExistingSnapshots,
		); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return failure, ctxErr
			}
			return failure, fmt.Errorf("validate mailbox snapshots before publish: %w", err)
		}
		return result, nil
	}
	aliasIDs := flattenedAliasIDs(aliasAddresses)
	if len(aliasIDs) == 0 {
		result.State.LastUID = upperUID
		return publish()
	}

	var (
		candidateUIDs      []uint32
		authoritativeEmpty map[int64]struct{}
	)
	if reset {
		// A new baseline intentionally keeps only the newest actual messages.
		// Sequence numbers make the limit message-based even when UIDs are sparse.
		candidateUIDs, err = fetchRecentMailboxUIDs(ctx, session, mailbox.Messages, settings.maxCandidates)
		if mailbox.Messages <= uint32(settings.maxCandidates) {
			authoritativeEmpty = idSet(aliasIDs)
		}
		if err == nil && mailbox.Messages > uint32(settings.maxCandidates) {
			currentPositions := make(map[int64]domain.MailboxSnapshotPosition, len(snapshotPositions))
			for aliasID, position := range snapshotPositions {
				if position.UIDValidity == uidValidity {
					currentPositions[aliasID] = position
				}
			}
			var missing map[int64]struct{}
			missing, err = findMissingSnapshotAliases(ctx, session, currentPositions, uidValidity)
			if len(missing) > 0 && authoritativeEmpty == nil {
				authoritativeEmpty = make(map[int64]struct{}, len(missing))
			}
			for aliasID := range missing {
				authoritativeEmpty[aliasID] = struct{}{}
			}
		}
	} else {
		if previous.LastUID < upperUID {
			// Incremental runs process the oldest outstanding actual messages first.
			// Sequence lookup bounds both the command response and local allocation.
			candidateUIDs, result.State.LastUID, result.HasMore, err = fetchIncrementalMailboxUIDs(
				ctx,
				session,
				mailbox.Messages,
				previous.LastUID,
				upperUID,
				settings.maxCandidates,
			)
		}
		if err == nil {
			var missing map[int64]struct{}
			missing, err = findMissingSnapshotAliases(ctx, session, snapshotPositions, uidValidity)
			if err == nil && len(missing) > 0 {
				authoritativeEmpty = missing
				if !result.HasMore {
					// Once caught up, one shared recent-window scan supplies a fallback
					// for every expunged alias snapshot. During a backlog, only the
					// current cursor-bounded batch may replace a missing snapshot; the
					// empty result keeps the cursor moving so later batches remain visible.
					candidateUIDs, err = fetchRecentMailboxUIDs(ctx, session, mailbox.Messages, settings.maxCandidates)
				}
			}
		}
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return failure, ctxErr
		}
		return failure, fmt.Errorf("discover candidate message UIDs: %w", err)
	}
	for _, uid := range candidateUIDs {
		if uid == 0 || uid > upperUID {
			return failure, fmt.Errorf("discover candidate message UIDs: UID %d is outside mailbox upper UID %d", uid, upperUID)
		}
	}
	winners := make(map[int64]uint32)
	if len(candidateUIDs) > 0 {
		winners, err = fetchCandidateWinners(
			ctx,
			session,
			candidateUIDs,
			aliasAddresses,
			account.Email,
			settings.maxHeaderBytes,
			settings.allowWeakRecipientHeaders,
		)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return failure, ctxErr
			}
			return failure, err
		}
	}
	for aliasID := range authoritativeEmpty {
		if _, found := winners[aliasID]; found {
			continue
		}
		result.Messages[aliasID] = domain.LatestMessage{
			AliasID:       aliasID,
			UIDValidity:   uidValidity,
			SyncedAt:      syncedAt,
			SnapshotState: domain.SnapshotEmpty,
		}
	}
	if len(winners) == 0 {
		return publish()
	}

	uidToAliases := make(map[uint32][]int64)
	for aliasID, uid := range winners {
		uidToAliases[uid] = append(uidToAliases[uid], aliasID)
	}
	resultMessageCount := len(result.Messages)
	if resultMessageCount < len(winners) {
		resultMessageCount = len(winners)
	}
	resultBaseBytes := int64(resultMessageCount * parsedMessageBaseBytes)
	resultDynamicBudget := int64(settings.maxFetchResultBytes) - resultBaseBytes
	if resultDynamicBudget < 0 {
		resultDynamicBudget = 0
	}
	foundAliasCount := len(winners)
	fairDynamicBytes := resultDynamicBudget / int64(foundAliasCount)
	fairParsedBytes := int64(parsedMessageBaseBytes) + fairDynamicBytes
	if fairParsedBytes > int64(settings.maxParsedMessageBytes) {
		fairParsedBytes = int64(settings.maxParsedMessageBytes)
	}
	maxParsedDynamicBytes := fairParsedBytes - parsedMessageBaseBytes
	fairBodyBytes := min(int64(settings.maxBodyBytes), fairDynamicBytes, maxParsedDynamicBytes)
	mimeLimits := defaultMIMELimits(fairBodyBytes, fairParsedBytes)
	messages, err := fetchMessages(
		session,
		mapKeys(uidToAliases),
		settings.maxMessageBytes,
		mimeLimits,
		uidToAliases,
		resultDynamicBudget,
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return failure, ctxErr
		}
		return failure, err
	}
	for uid := range uidToAliases {
		if _, found := messages[uid]; !found {
			return failure, fmt.Errorf("fetch latest messages: missing or invalid winner UID %d", uid)
		}
	}

	for uid, fetched := range messages {
		parsed := fetched.parsed
		for _, aliasID := range uidToAliases[uid] {
			result.Messages[aliasID] = domain.LatestMessage{
				AliasID:       aliasID,
				UIDValidity:   uidValidity,
				UID:           uid,
				MessageID:     parsed.messageID,
				InternalDate:  fetched.internalDate,
				HeaderDate:    parsed.headerDate,
				From:          parsed.from,
				To:            parsed.to,
				CC:            parsed.cc,
				Subject:       parsed.subject,
				TextBody:      parsed.textBody,
				HTMLBody:      parsed.htmlBody,
				Attachments:   parsed.attachments,
				BodyTruncated: fetched.truncated || parsed.bodyTruncated,
				SyncedAt:      syncedAt,
				SnapshotState: domain.SnapshotFound,
			}
		}
	}
	return publish()
}

// fetchCandidateWinners scans candidate headers newest-first in shared batches.
// A successful UID FETCH may omit expunged UID gaps. Individual malformed or
// ambiguous messages are skipped because they cannot be routed safely; command
// failures and protocol invariant violations still fail the whole batch.
func fetchCandidateWinners(
	ctx context.Context,
	session imapSession,
	candidateUIDs []uint32,
	aliases map[string][]int64,
	accountEmail string,
	maxHeaderBytes int,
	allowWeak bool,
) (map[int64]uint32, error) {
	section := &imap.BodySectionName{
		Peek: true,
		BodyPartName: imap.BodyPartName{
			Specifier: imap.HeaderSpecifier,
			Fields:    recipientHeaderFieldsForFetch(),
		},
		Partial: []int{0, maxHeaderBytes + 1},
	}
	winners := make(map[int64]uint32)
	for start := 0; start < len(candidateUIDs); start += candidateHeaderFetchBatch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+candidateHeaderFetchBatch, len(candidateUIDs))
		uids := candidateUIDs[start:end]
		requested := make(map[uint32]struct{}, len(uids))
		for _, uid := range uids {
			requested[uid] = struct{}{}
		}
		batch := make(map[uint32]int64, len(uids))
		seen := make(map[uint32]struct{}, len(uids))
		err := uidFetchEach(session, uids, []imap.FetchItem{imap.FetchUid, section.FetchItem()}, func(message *imap.Message) error {
			if _, ok := requested[message.Uid]; !ok {
				return fmt.Errorf("unexpected candidate UID %d", message.Uid)
			}
			if _, duplicate := seen[message.Uid]; duplicate {
				return fmt.Errorf("duplicate candidate header UID %d", message.Uid)
			}
			seen[message.Uid] = struct{}{}
			body := message.GetBody(section)
			if body == nil {
				return nil
			}
			raw, truncated, readErr := readLiteral(body, maxHeaderBytes)
			if readErr != nil {
				return fmt.Errorf("read candidate UID %d header: %w", message.Uid, readErr)
			}
			if truncated {
				return nil
			}
			parsed, parseErr := stdmail.ReadMessage(bytes.NewReader(raw))
			if parseErr != nil {
				return nil
			}
			aliasID, determinate := classifyScannedRecipientAlias(parsed.Header, aliases, accountEmail, allowWeak)
			if !determinate {
				return nil
			}
			batch[message.Uid] = aliasID
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("fetch candidate headers: %w", err)
		}
		for _, uid := range uids {
			aliasID, found := batch[uid]
			if !found || aliasID == 0 {
				continue
			}
			if _, alreadyFound := winners[aliasID]; !alreadyFound {
				winners[aliasID] = uid
			}
		}
	}
	return winners, nil
}

// classifyScannedRecipientAlias treats mail without any trusted routing
// signal as ordinary account mail. Once a relevant signal is present, the
// stricter classifier still fails closed on malformed or conflicting values.
func classifyScannedRecipientAlias(
	header stdmail.Header,
	aliases map[string][]int64,
	accountEmail string,
	allowWeak bool,
) (int64, bool) {
	aliasID, determinate := classifyRecipientAlias(header, aliases, accountEmail, allowWeak)
	if determinate {
		return aliasID, true
	}
	candidateFields := append([]string{icloudHMEHeaderField}, strongRecipientHeaderFields...)
	if allowWeak {
		candidateFields = append(candidateFields, weakRecipientHeaderFields...)
	}
	if !hasAnyHeader(header, candidateFields) {
		return 0, true
	}
	return 0, false
}

type fetchSettings struct {
	timeout                   time.Duration
	maxAliases                int
	maxCandidates             int
	maxHeaderBytes            int
	maxMessageBytes           int
	maxBodyBytes              int
	maxParsedMessageBytes     int
	maxFetchResultBytes       int
	allowWeakRecipientHeaders bool
	dial                      dialSessionFunc
	now                       func() time.Time
}

func (f *Fetcher) settings() fetchSettings {
	settings := fetchSettings{
		timeout:             defaultIMAPTimeout,
		maxAliases:          defaultMaxAliases,
		maxCandidates:       defaultMaxCandidates,
		maxHeaderBytes:      defaultMaxHeaderBytes,
		maxMessageBytes:     defaultMaxMessageBytes,
		maxBodyBytes:        defaultMaxBodyBytes,
		maxFetchResultBytes: defaultMaxFetchResultBytes,
		dial:                dialIMAPTLS,
		now:                 time.Now,
	}
	if f == nil {
		return settings
	}
	if f.IMAPTimeout > 0 {
		settings.timeout = f.IMAPTimeout
	}
	if f.MaxAliases > 0 {
		settings.maxAliases = f.MaxAliases
	}
	if f.MaxCandidates > 0 {
		settings.maxCandidates = f.MaxCandidates
	}
	if f.MaxHeaderBytes > 0 {
		settings.maxHeaderBytes = f.MaxHeaderBytes
	}
	if f.MaxMessageBytes > 0 {
		settings.maxMessageBytes = f.MaxMessageBytes
	}
	if f.MaxBodyBytes > 0 {
		settings.maxBodyBytes = f.MaxBodyBytes
	}
	settings.maxParsedMessageBytes = settings.maxBodyBytes + defaultMetadataResultBytes
	if f.MaxParsedMessageBytes > 0 {
		settings.maxParsedMessageBytes = f.MaxParsedMessageBytes
	}
	if settings.maxParsedMessageBytes < parsedMessageBaseBytes {
		settings.maxParsedMessageBytes = parsedMessageBaseBytes
	}
	if f.MaxFetchResultBytes > 0 {
		settings.maxFetchResultBytes = f.MaxFetchResultBytes
	}
	settings.allowWeakRecipientHeaders = f.AllowWeakRecipientHeaders
	if f.dial != nil {
		settings.dial = f.dial
	}
	if f.now != nil {
		settings.now = f.now
	}
	return settings
}

func prepareAliases(account domain.Account, aliases []domain.Alias, maxAliases int) (map[string][]int64, error) {
	byAddress := make(map[string][]int64)
	seenIDs := make(map[int64]struct{})
	for _, alias := range aliases {
		if !alias.Enabled {
			continue
		}
		if alias.AccountID != account.ID {
			return nil, fmt.Errorf("%w: alias %d", ErrAliasAccountMismatch, alias.ID)
		}
		if alias.ID <= 0 {
			return nil, fmt.Errorf("%w: missing ID", ErrInvalidAlias)
		}
		if _, exists := seenIDs[alias.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate ID %d", ErrInvalidAlias, alias.ID)
		}
		if len(seenIDs) == maxAliases {
			return nil, ErrTooManyAliases
		}
		seenIDs[alias.ID] = struct{}{}
		address, ok := normalizeAliasAddress(alias.Address)
		if !ok {
			return nil, fmt.Errorf("%w: alias %d address", ErrInvalidAlias, alias.ID)
		}
		byAddress[address] = append(byAddress[address], alias.ID)
	}
	if len(seenIDs) > maxAliases {
		return nil, ErrTooManyAliases
	}
	return byAddress, nil
}

func validateSnapshotPositions(aliases []domain.Alias, positions map[int64]domain.MailboxSnapshotPosition) error {
	enabled := make(map[int64]struct{}, len(aliases))
	for _, alias := range aliases {
		if alias.Enabled {
			enabled[alias.ID] = struct{}{}
		}
	}
	for aliasID, position := range positions {
		if _, ok := enabled[aliasID]; !ok {
			return fmt.Errorf("invalid mailbox snapshot position: alias %d is not enabled", aliasID)
		}
		if position.AliasID != aliasID || position.UIDValidity == 0 || position.UID == 0 {
			return fmt.Errorf("invalid mailbox snapshot position for alias %d", aliasID)
		}
	}
	return nil
}

func flattenedAliasIDs(aliases map[string][]int64) []int64 {
	result := make([]int64, 0)
	for _, ids := range aliases {
		result = append(result, ids...)
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result
}

func idSet(ids []int64) map[int64]struct{} {
	result := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		result[id] = struct{}{}
	}
	return result
}

// findMissingSnapshotAliases validates all same-generation snapshot UIDs in
// one account-level UID FETCH. Positions from an older generation are already
// stale and are reconciled without sending them to the current mailbox.
func findMissingSnapshotAliases(
	ctx context.Context,
	session imapSession,
	positions map[int64]domain.MailboxSnapshotPosition,
	uidValidity uint32,
) (map[int64]struct{}, error) {
	missing := make(map[int64]struct{})
	uids := make([]uint32, 0, len(positions))
	requested := make(map[uint32]struct{}, len(positions))
	for aliasID, position := range positions {
		if position.UIDValidity != uidValidity {
			missing[aliasID] = struct{}{}
			continue
		}
		if _, exists := requested[position.UID]; !exists {
			requested[position.UID] = struct{}{}
			uids = append(uids, position.UID)
		}
	}
	if len(uids) == 0 {
		return missing, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	found := make(map[uint32]struct{}, len(uids))
	err := uidFetchEach(session, uids, []imap.FetchItem{imap.FetchUid}, func(message *imap.Message) error {
		if _, ok := requested[message.Uid]; !ok {
			return fmt.Errorf("snapshot validation returned unexpected UID %d", message.Uid)
		}
		if _, duplicate := found[message.Uid]; duplicate {
			return fmt.Errorf("snapshot validation returned duplicate UID %d", message.Uid)
		}
		found[message.Uid] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("validate mailbox snapshots: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for aliasID, position := range positions {
		if position.UIDValidity != uidValidity {
			continue
		}
		if _, exists := found[position.UID]; !exists {
			missing[aliasID] = struct{}{}
		}
	}
	return missing, nil
}

// validatePublishUIDs is the final account-level mailbox check before a result
// can be published. It covers both newly fetched winners and same-generation
// snapshots that the store will retain because this result does not replace
// them. A UID omitted by this one shared command was expunged during the scan,
// so the whole result must be retried without advancing its cursor.
func validatePublishUIDs(
	ctx context.Context,
	session imapSession,
	positions map[int64]domain.MailboxSnapshotPosition,
	messages map[int64]domain.LatestMessage,
	uidValidity uint32,
	preserveExisting bool,
) error {
	requested := make(map[uint32]struct{}, len(positions)+len(messages))
	if preserveExisting {
		for aliasID, position := range positions {
			if position.UIDValidity != uidValidity {
				continue
			}
			if _, replaced := messages[aliasID]; replaced {
				continue
			}
			requested[position.UID] = struct{}{}
		}
	}
	for aliasID, message := range messages {
		switch message.SnapshotState {
		case domain.SnapshotFound:
			if message.AliasID != aliasID || message.UIDValidity != uidValidity || message.UID == 0 {
				return fmt.Errorf("invalid found snapshot for alias %d", aliasID)
			}
			requested[message.UID] = struct{}{}
		case domain.SnapshotEmpty:
		case domain.SnapshotUnknown:
			return fmt.Errorf("indeterminate snapshot for alias %d", aliasID)
		default:
			return fmt.Errorf("invalid snapshot state %q for alias %d", message.SnapshotState, aliasID)
		}
	}
	if len(requested) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	uids := make([]uint32, 0, len(requested))
	for uid := range requested {
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(left, right int) bool { return uids[left] < uids[right] })
	found := make(map[uint32]struct{}, len(uids))
	err := uidFetchEach(session, uids, []imap.FetchItem{imap.FetchUid}, func(message *imap.Message) error {
		if _, ok := requested[message.Uid]; !ok {
			return fmt.Errorf("final validation returned unexpected UID %d", message.Uid)
		}
		if _, duplicate := found[message.Uid]; duplicate {
			return fmt.Errorf("final validation returned duplicate UID %d", message.Uid)
		}
		found[message.Uid] = struct{}{}
		return nil
	})
	if err != nil {
		return fmt.Errorf("final UID FETCH: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, uid := range uids {
		if _, exists := found[uid]; !exists {
			return fmt.Errorf("UID %d was expunged before publish", uid)
		}
	}
	return nil
}

func accountEndpoint(account domain.Account) (host, address, username string, err error) {
	host = strings.TrimSpace(account.IMAPHost)
	if host == "" {
		host = defaultIMAPHost
	}
	if !strings.EqualFold(strings.TrimSuffix(host, "."), defaultIMAPHost) {
		return "", "", "", fmt.Errorf("%w: IMAP host must be %s", ErrInvalidIMAPConfig, defaultIMAPHost)
	}
	port := account.IMAPPort
	if port == 0 {
		port = defaultIMAPPort
	}
	if port != defaultIMAPPort {
		return "", "", "", fmt.Errorf("%w: IMAP port must be %d", ErrInvalidIMAPConfig, defaultIMAPPort)
	}
	username = strings.TrimSpace(account.IMAPUsername)
	if username == "" {
		username = strings.TrimSpace(account.Email)
	}
	if username == "" {
		return "", "", "", fmt.Errorf("%w: empty username", ErrInvalidIMAPConfig)
	}
	return defaultIMAPHost, "imap.mail.me.com:993", username, nil
}

// fetchRecentMailboxUIDs returns the newest actual message UIDs in descending
// order. Sequence numbers are used for discovery because UIDs can be sparse
// after messages are expunged; the caller then uses UID FETCH for headers and
// bodies. A short or malformed response is an error so the caller never
// advances its cursor past an uncertain mailbox view.
func fetchRecentMailboxUIDs(
	ctx context.Context,
	session imapSession,
	messagesCount uint32,
	limit int,
) ([]uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, errors.New("candidate limit must be positive")
	}
	if messagesCount == 0 {
		return nil, nil
	}

	window := uint64(limit)
	if window > uint64(messagesCount) {
		window = uint64(messagesCount)
	}
	first := messagesCount - uint32(window) + 1
	last := messagesCount
	uids, err := fetchMailboxUIDSequenceRange(ctx, session, first, last)
	if err != nil {
		return nil, err
	}
	for left, right := 0, len(uids)-1; left < right; left, right = left+1, right-1 {
		uids[left], uids[right] = uids[right], uids[left]
	}
	return uids, nil
}

// fetchIncrementalMailboxUIDs returns at most limit oldest outstanding actual
// UIDs in descending order. Small numeric ranges use one bounded UID SEARCH;
// large ranges locate the first sequence after lastUID with bounded binary
// probes, then fetch one contiguous sequence batch plus a sentinel. Response
// size is O(limit), even for very large or sparse UID sets.
func fetchIncrementalMailboxUIDs(
	ctx context.Context,
	session imapSession,
	messagesCount uint32,
	lastUID uint32,
	upperUID uint32,
	limit int,
) (uids []uint32, processedThrough uint32, hasMore bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, lastUID, false, err
	}
	if limit <= 0 {
		return nil, lastUID, false, errors.New("candidate limit must be positive")
	}
	if lastUID >= upperUID {
		return nil, upperUID, false, nil
	}
	if uint64(upperUID)-uint64(lastUID) <= uint64(limit) {
		set := new(imap.SeqSet)
		set.AddRange(lastUID+1, upperUID)
		criteria := imap.NewSearchCriteria()
		criteria.Uid = set
		discovered, searchErr := session.UidSearch(criteria)
		if searchErr != nil {
			return nil, lastUID, false, fmt.Errorf("search UID range %d:%d: %w", lastUID+1, upperUID, searchErr)
		}
		if err := ctx.Err(); err != nil {
			return nil, lastUID, false, err
		}
		seen := make(map[uint32]struct{}, len(discovered))
		for _, uid := range discovered {
			if uid <= lastUID || uid > upperUID {
				return nil, lastUID, false, fmt.Errorf("search UID range %d:%d returned unexpected UID %d", lastUID+1, upperUID, uid)
			}
			if _, duplicate := seen[uid]; duplicate {
				return nil, lastUID, false, fmt.Errorf("search UID range %d:%d returned duplicate UID %d", lastUID+1, upperUID, uid)
			}
			seen[uid] = struct{}{}
		}
		sort.Slice(discovered, func(left, right int) bool { return discovered[left] > discovered[right] })
		return discovered, upperUID, false, nil
	}
	if messagesCount == 0 {
		return nil, upperUID, false, nil
	}

	firstSequence, err := findFirstSequenceAfterUID(ctx, session, messagesCount, lastUID, upperUID)
	if err != nil {
		return nil, lastUID, false, err
	}
	if firstSequence > uint64(messagesCount) {
		return nil, upperUID, false, nil
	}

	first := uint32(firstSequence)
	rangeFirst := first
	if first > 1 {
		rangeFirst--
	}
	rangeLast := firstSequence + uint64(limit)
	if rangeLast > uint64(messagesCount) {
		rangeLast = uint64(messagesCount)
	}
	discovered, err := fetchMailboxUIDSequenceRange(ctx, session, rangeFirst, uint32(rangeLast))
	if err != nil {
		return nil, lastUID, false, err
	}
	if first > 1 {
		if discovered[0] > lastUID {
			return nil, lastUID, false, fmt.Errorf("mailbox sequence boundary changed before batch fetch")
		}
		discovered = discovered[1:]
	}
	for _, uid := range discovered {
		if uid <= lastUID || uid > upperUID {
			return nil, lastUID, false, fmt.Errorf("incremental sequence batch returned unexpected UID %d", uid)
		}
	}
	hasMore = len(discovered) > limit
	if hasMore {
		discovered = discovered[:limit]
		processedThrough = discovered[len(discovered)-1]
	} else {
		processedThrough = upperUID
	}
	for left, right := 0, len(discovered)-1; left < right; left, right = left+1, right-1 {
		discovered[left], discovered[right] = discovered[right], discovered[left]
	}
	return discovered, processedThrough, hasMore, nil
}

func findFirstSequenceAfterUID(
	ctx context.Context,
	session imapSession,
	messagesCount uint32,
	lastUID uint32,
	upperUID uint32,
) (uint64, error) {
	low, high := uint64(1), uint64(messagesCount)+1
	for low < high {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		middle := low + (high-low)/2
		uids, err := fetchMailboxUIDSequenceRange(ctx, session, uint32(middle), uint32(middle))
		if err != nil {
			return 0, err
		}
		uid := uids[0]
		if uid > upperUID {
			return 0, fmt.Errorf("sequence %d returned UID %d beyond selected upper UID %d", middle, uid, upperUID)
		}
		if uid <= lastUID {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return low, nil
}

func fetchMailboxUIDSequenceRange(
	ctx context.Context,
	session imapSession,
	first uint32,
	last uint32,
) ([]uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if first == 0 || last < first {
		return nil, fmt.Errorf("invalid mailbox sequence range %d:%d", first, last)
	}
	set := new(imap.SeqSet)
	set.AddRange(first, last)
	messages := make(chan *imap.Message)
	done := make(chan error, 1)
	go func() {
		done <- session.Fetch(set, []imap.FetchItem{imap.FetchUid}, messages)
	}()

	type sequenceUID struct {
		sequence uint32
		uid      uint32
	}
	want := uint64(last) - uint64(first) + 1
	discovered := make([]sequenceUID, 0, int(want))
	seenSequences := make(map[uint32]struct{}, int(want))
	seenUIDs := make(map[uint32]struct{}, int(want))
	malformed := false
	for message := range messages {
		if message == nil || message.SeqNum < first || message.SeqNum > last || message.Uid == 0 {
			malformed = true
			continue
		}
		if _, duplicate := seenSequences[message.SeqNum]; duplicate {
			malformed = true
			continue
		}
		if _, duplicate := seenUIDs[message.Uid]; duplicate {
			malformed = true
			continue
		}
		seenSequences[message.SeqNum] = struct{}{}
		seenUIDs[message.Uid] = struct{}{}
		discovered = append(discovered, sequenceUID{sequence: message.SeqNum, uid: message.Uid})
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("fetch sequence range %d:%d: %w", first, last, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(discovered, func(left, right int) bool {
		return discovered[left].sequence < discovered[right].sequence
	})
	if uint64(len(discovered)) != want {
		malformed = true
	}
	for index, item := range discovered {
		if item.sequence != first+uint32(index) || (index > 0 && item.uid <= discovered[index-1].uid) {
			malformed = true
			break
		}
	}
	if malformed {
		return nil, fmt.Errorf("fetch sequence range %d:%d returned an unstable mailbox view", first, last)
	}
	uids := make([]uint32, len(discovered))
	for index, item := range discovered {
		uids[index] = item.uid
	}
	return uids, nil
}

type fetchedMessage struct {
	parsed       parsedMessage
	internalDate time.Time
	truncated    bool
}

func fetchMessages(
	session imapSession,
	uids []uint32,
	maxMessageBytes int,
	limits mimeLimits,
	aliasesByUID map[uint32][]int64,
	remainingResultBytes int64,
) (map[uint32]fetchedMessage, error) {
	if remainingResultBytes < 0 {
		remainingResultBytes = 0
	}
	section := &imap.BodySectionName{Peek: true, Partial: []int{0, maxMessageBytes + 1}}
	result := make(map[uint32]fetchedMessage, len(uids))
	for start := 0; start < len(uids); start += messageFetchBatch {
		end := min(start+messageFetchBatch, len(uids))
		batchUIDs := make(map[uint32]struct{}, end-start)
		batchResult := make(map[uint32]fetchedMessage, end-start)
		seenUIDs := make(map[uint32]struct{}, end-start)
		invalidUIDs := make(map[uint32]struct{})
		for _, uid := range uids[start:end] {
			batchUIDs[uid] = struct{}{}
		}
		err := uidFetchEach(session, uids[start:end], []imap.FetchItem{
			imap.FetchUid,
			imap.FetchInternalDate,
			imap.FetchRFC822Size,
			section.FetchItem(),
		}, func(message *imap.Message) error {
			if _, requested := batchUIDs[message.Uid]; !requested {
				return nil
			}
			if _, invalid := invalidUIDs[message.Uid]; invalid {
				return nil
			}
			if _, duplicate := seenUIDs[message.Uid]; duplicate {
				delete(batchResult, message.Uid)
				invalidUIDs[message.Uid] = struct{}{}
				return nil
			}
			seenUIDs[message.Uid] = struct{}{}
			body := message.GetBody(section)
			if body == nil || message.Uid == 0 {
				return nil
			}
			raw, truncated, readErr := readLiteral(body, maxMessageBytes)
			if readErr != nil {
				return nil
			}
			truncated = truncated || uint64(message.Size) > uint64(maxMessageBytes)
			parsed, parseErr := parseMIMEMessageWithOptions(raw, limits, truncated)
			if parseErr != nil {
				// Recipient routing was already established from the bounded header
				// fetch. Preserve the mailbox position with an explicit truncated
				// placeholder instead of letting one malformed MIME body block the
				// account cursor forever.
				parsed = parsedMessage{bodyTruncated: true}
				truncated = true
			}
			batchResult[message.Uid] = fetchedMessage{
				parsed:       parsed,
				internalDate: message.InternalDate,
				truncated:    truncated,
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("fetch latest messages: %w", err)
		}
		for _, uid := range uids[start:end] {
			fetched, found := batchResult[uid]
			if !found {
				continue
			}
			aliasCount := len(aliasesByUID[uid])
			if aliasCount == 0 {
				aliasCount = 1
			}
			allowedPerAlias := remainingResultBytes / int64(aliasCount)
			dynamicBytes := parsedMessageDynamicBytes(fetched.parsed)
			if dynamicBytes > allowedPerAlias {
				trimParsedMessageToBytes(&fetched.parsed, parsedMessageBaseBytes+allowedPerAlias)
				dynamicBytes = parsedMessageDynamicBytes(fetched.parsed)
			}
			cost := dynamicBytes * int64(aliasCount)
			if cost > remainingResultBytes {
				cost = remainingResultBytes
			}
			remainingResultBytes -= cost
			result[uid] = fetched
		}
	}
	return result, nil
}

func uidFetchEach(session imapSession, uids []uint32, items []imap.FetchItem, visit func(*imap.Message) error) error {
	if len(uids) == 0 {
		return nil
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	messages := make(chan *imap.Message)
	done := make(chan error, 1)
	go func() {
		done <- session.UidFetch(set, items, messages)
	}()
	var visitErr error
	for message := range messages {
		if message != nil && visitErr == nil {
			visitErr = visit(message)
		}
	}
	if err := <-done; err != nil {
		return err
	}
	return visitErr
}

func readLiteral(literal imap.Literal, limit int) ([]byte, bool, error) {
	if literal == nil {
		return nil, false, errors.New("nil IMAP literal")
	}
	if bytesValue, ok := literal.(interface{ Bytes() []byte }); ok {
		content := bytesValue.Bytes()
		if len(content) > limit {
			return content[:limit], true, nil
		}
		return content, false, nil
	}
	content, err := io.ReadAll(io.LimitReader(literal, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(content) > limit {
		return content[:limit], true, nil
	}
	return content, false, nil
}

func mapKeys(values map[uint32][]int64) []uint32 {
	keys := make([]uint32, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func dialIMAPTLS(ctx context.Context, address, serverName string, timeout time.Duration) (imapSession, error) {
	dialer := &net.Dialer{Timeout: timeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}

	stopCancellation := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stopCancellation:
		}
	}()
	defer close(stopCancellation)

	tlsConnection := tls.Client(connection, &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	})
	handshakeContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := tlsConnection.HandshakeContext(handshakeContext); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if err := tlsConnection.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = connection.Close()
		return nil, err
	}
	session, err := imapclient.New(tlsConnection)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	if err := tlsConnection.SetDeadline(time.Time{}); err != nil {
		_ = session.Terminate()
		return nil, err
	}
	session.Timeout = timeout
	return session, nil
}

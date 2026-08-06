package mail

import (
	"bytes"
	"container/heap"
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
	defaultIMAPHost              = "imap.mail.me.com"
	defaultIMAPPort              = 993
	defaultIMAPTimeout           = 25 * time.Second
	defaultMaxAliases            = domain.MaxEnabledAliasesPerAccount
	defaultMaxCandidates         = 1024
	defaultMaxCandidatesPerAlias = 24
	defaultMaxHeaderBytes        = 128 << 10
	defaultMaxMessageBytes       = 10 << 20
	defaultMaxBodyBytes          = 1 << 20
	defaultMaxFetchResultBytes   = 64 << 20
	candidateHeaderFetchBatch    = 64
	messageFetchBatch            = 8
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
	MaxCandidatesPerAlias     int
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
		IMAPTimeout:           defaultIMAPTimeout,
		MaxAliases:            defaultMaxAliases,
		MaxCandidates:         defaultMaxCandidates,
		MaxCandidatesPerAlias: defaultMaxCandidatesPerAlias,
		MaxHeaderBytes:        defaultMaxHeaderBytes,
		MaxMessageBytes:       defaultMaxMessageBytes,
		MaxBodyBytes:          defaultMaxBodyBytes,
		dial:                  dialIMAPTLS,
		now:                   time.Now,
	}
}

// FetchLatest returns one snapshot state per enabled alias. Empty is
// authoritative, Unknown preserves the prior snapshot, and Found includes the
// latest parsed message.
func (f *Fetcher) FetchLatest(ctx context.Context, account domain.Account, password string, aliases []domain.Alias) (map[int64]domain.LatestMessage, error) {
	settings := f.settings()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !account.Enabled {
		return nil, ErrAccountDisabled
	}
	if password == "" {
		return nil, fmt.Errorf("%w: empty password", ErrInvalidIMAPConfig)
	}

	aliasAddresses, searchAddresses, err := prepareAliases(account, aliases, settings.maxAliases, settings.maxCandidates)
	if err != nil {
		return nil, err
	}

	host, address, username, err := accountEndpoint(account)
	if err != nil {
		return nil, err
	}
	session, err := settings.dial(ctx, address, host, settings.timeout)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("connect IMAP %s: %w", address, err)
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
			return nil, ctxErr
		}
		return nil, fmt.Errorf("login IMAP account: %w", err)
	}
	mailbox, err := session.Select("INBOX", true)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("select INBOX read-only: %w", err)
	}
	if mailbox == nil {
		return nil, errors.New("select INBOX read-only: empty mailbox status")
	}
	if mailbox.UidValidity == 0 {
		return nil, errors.New("select INBOX read-only: UIDVALIDITY is zero")
	}
	if len(aliasAddresses) == 0 {
		return map[int64]domain.LatestMessage{}, nil
	}

	syncedAt := settings.now().UTC()
	resultCapacity := 0
	for _, aliasIDs := range aliasAddresses {
		resultCapacity += len(aliasIDs)
	}
	result := make(map[int64]domain.LatestMessage, resultCapacity)
	for _, aliasIDs := range aliasAddresses {
		for _, aliasID := range aliasIDs {
			result[aliasID] = domain.LatestMessage{
				AliasID:       aliasID,
				UIDValidity:   mailbox.UidValidity,
				SyncedAt:      syncedAt,
				SnapshotState: domain.SnapshotUnknown,
			}
		}
	}

	uidLists := make([][]uint32, 0, len(searchAddresses))
	aliasCandidateUIDs := make(map[int64][]uint32, len(result))
	aliasCandidatesTruncated := make(map[int64]bool, len(result))
	for _, aliasAddress := range searchAddresses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		uids, searchErr := session.UidSearch(recipientSearchCriteria(aliasAddress, settings.allowWeakRecipientHeaders))
		if searchErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("search alias recipients: %w", searchErr)
		}
		newest, candidatesTruncated := newestUIDs(uids, settings.maxCandidatesPerAlias)
		uidLists = append(uidLists, newest)
		for _, aliasID := range aliasAddresses[aliasAddress] {
			aliasCandidateUIDs[aliasID] = newest
			aliasCandidatesTruncated[aliasID] = candidatesTruncated
		}
		if len(uids) == 0 {
			for _, aliasID := range aliasAddresses[aliasAddress] {
				empty := result[aliasID]
				empty.SnapshotState = domain.SnapshotEmpty
				result[aliasID] = empty
			}
		}
	}

	candidateUIDs := fairCandidateUIDs(uidLists, settings.maxCandidates)
	if len(candidateUIDs) == 0 {
		return result, nil
	}

	winners, falsePositiveEmpty, err := findAliasWinners(
		session,
		candidateUIDs,
		aliasCandidateUIDs,
		aliasCandidatesTruncated,
		aliasAddresses,
		settings.maxHeaderBytes,
		settings.allowWeakRecipientHeaders,
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	for aliasID := range falsePositiveEmpty {
		empty := result[aliasID]
		empty.SnapshotState = domain.SnapshotEmpty
		result[aliasID] = empty
	}
	if len(winners) == 0 {
		return result, nil
	}

	uidToAliases := make(map[uint32][]int64)
	for aliasID, uid := range winners {
		uidToAliases[uid] = append(uidToAliases[uid], aliasID)
	}
	resultBaseBytes := int64(len(result) * parsedMessageBaseBytes)
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
			return nil, ctxErr
		}
		return nil, err
	}

	for uid, fetched := range messages {
		parsed := fetched.parsed
		for _, aliasID := range uidToAliases[uid] {
			result[aliasID] = domain.LatestMessage{
				AliasID:       aliasID,
				UIDValidity:   mailbox.UidValidity,
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
	return result, nil
}

type fetchSettings struct {
	timeout                   time.Duration
	maxAliases                int
	maxCandidates             int
	maxCandidatesPerAlias     int
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
		timeout:               defaultIMAPTimeout,
		maxAliases:            defaultMaxAliases,
		maxCandidates:         defaultMaxCandidates,
		maxCandidatesPerAlias: defaultMaxCandidatesPerAlias,
		maxHeaderBytes:        defaultMaxHeaderBytes,
		maxMessageBytes:       defaultMaxMessageBytes,
		maxBodyBytes:          defaultMaxBodyBytes,
		maxFetchResultBytes:   defaultMaxFetchResultBytes,
		dial:                  dialIMAPTLS,
		now:                   time.Now,
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
	if f.MaxCandidatesPerAlias > 0 {
		settings.maxCandidatesPerAlias = f.MaxCandidatesPerAlias
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

func prepareAliases(account domain.Account, aliases []domain.Alias, maxAliases, maxCandidates int) (map[string][]int64, []string, error) {
	byAddress := make(map[string][]int64)
	seenIDs := make(map[int64]struct{})
	for _, alias := range aliases {
		if !alias.Enabled {
			continue
		}
		if alias.AccountID != account.ID {
			return nil, nil, fmt.Errorf("%w: alias %d", ErrAliasAccountMismatch, alias.ID)
		}
		if alias.ID <= 0 {
			return nil, nil, fmt.Errorf("%w: missing ID", ErrInvalidAlias)
		}
		if _, exists := seenIDs[alias.ID]; exists {
			return nil, nil, fmt.Errorf("%w: duplicate ID %d", ErrInvalidAlias, alias.ID)
		}
		if len(seenIDs) == maxAliases {
			return nil, nil, ErrTooManyAliases
		}
		seenIDs[alias.ID] = struct{}{}
		address, ok := normalizeAliasAddress(alias.Address)
		if !ok {
			return nil, nil, fmt.Errorf("%w: alias %d address", ErrInvalidAlias, alias.ID)
		}
		if _, exists := byAddress[address]; !exists && len(byAddress) == maxCandidates {
			return nil, nil, ErrTooManyAliases
		}
		byAddress[address] = append(byAddress[address], alias.ID)
	}
	if len(seenIDs) > maxAliases || len(byAddress) > maxCandidates {
		return nil, nil, ErrTooManyAliases
	}
	addresses := make([]string, 0, len(byAddress))
	for address := range byAddress {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	return byAddress, addresses, nil
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

func recipientSearchCriteria(address string, allowWeak bool) *imap.SearchCriteria {
	headerFields := recipientSearchHeaderFields(allowWeak)
	leaves := make([]*imap.SearchCriteria, 0, len(headerFields))
	for _, field := range headerFields {
		criteria := imap.NewSearchCriteria()
		criteria.Header.Set(field, address)
		leaves = append(leaves, criteria)
	}
	for len(leaves) > 1 {
		next := make([]*imap.SearchCriteria, 0, (len(leaves)+1)/2)
		for i := 0; i < len(leaves); i += 2 {
			if i+1 == len(leaves) {
				next = append(next, leaves[i])
				continue
			}
			next = append(next, &imap.SearchCriteria{Or: [][2]*imap.SearchCriteria{{leaves[i], leaves[i+1]}}})
		}
		leaves = next
	}
	return leaves[0]
}

type uidMinHeap []uint32

func (h uidMinHeap) Len() int           { return len(h) }
func (h uidMinHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h uidMinHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *uidMinHeap) Push(value any)    { *h = append(*h, value.(uint32)) }
func (h *uidMinHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func newestUIDs(uids []uint32, limit int) ([]uint32, bool) {
	if limit <= 0 {
		if len(uids) > 0 {
			return nil, true
		}
		return nil, false
	}

	selected := make(uidMinHeap, 0, min(limit, len(uids)))
	selectedSet := make(map[uint32]struct{}, min(limit, len(uids)))
	truncated := false
	for _, uid := range uids {
		if uid == 0 {
			truncated = true
			continue
		}
		if _, exists := selectedSet[uid]; exists {
			continue
		}
		if len(selected) < limit {
			heap.Push(&selected, uid)
			selectedSet[uid] = struct{}{}
			continue
		}
		truncated = true
		if uid <= selected[0] {
			continue
		}
		removed := heap.Pop(&selected).(uint32)
		delete(selectedSet, removed)
		heap.Push(&selected, uid)
		selectedSet[uid] = struct{}{}
	}
	result := append([]uint32(nil), selected...)
	sort.Slice(result, func(i, j int) bool { return result[i] > result[j] })
	return result, truncated
}

func fairCandidateUIDs(lists [][]uint32, limit int) []uint32 {
	seen := make(map[uint32]struct{})
	result := make([]uint32, 0, limit)
	for depth := 0; len(result) < limit; depth++ {
		found := false
		for _, list := range lists {
			if depth >= len(list) {
				continue
			}
			found = true
			uid := list[depth]
			if _, exists := seen[uid]; exists {
				continue
			}
			seen[uid] = struct{}{}
			result = append(result, uid)
			if len(result) == limit {
				break
			}
		}
		if !found {
			break
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] > result[j] })
	return result
}

type candidateHeaderResolution struct {
	aliasID     int64
	determinate bool
}

func findAliasWinners(
	session imapSession,
	uids []uint32,
	aliasCandidates map[int64][]uint32,
	aliasCandidatesTruncated map[int64]bool,
	aliases map[string][]int64,
	maxHeaderBytes int,
	allowWeak bool,
) (map[int64]uint32, map[int64]struct{}, error) {
	requestedUIDs := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		requestedUIDs[uid] = struct{}{}
	}
	section := &imap.BodySectionName{
		Peek: true,
		BodyPartName: imap.BodyPartName{
			Specifier: imap.HeaderSpecifier,
			Fields:    recipientSearchHeaderFields(allowWeak),
		},
		Partial: []int{0, maxHeaderBytes + 1},
	}
	resolutions := make(map[uint32]candidateHeaderResolution, len(uids))
	seenResponses := make(map[uint32]struct{}, len(uids))
	for start := 0; start < len(uids); start += candidateHeaderFetchBatch {
		end := min(start+candidateHeaderFetchBatch, len(uids))
		batchUIDs := make(map[uint32]struct{}, end-start)
		for _, uid := range uids[start:end] {
			batchUIDs[uid] = struct{}{}
		}
		err := uidFetchEach(session, uids[start:end], []imap.FetchItem{imap.FetchUid, section.FetchItem()}, func(message *imap.Message) error {
			if _, requested := batchUIDs[message.Uid]; !requested {
				return nil
			}
			if _, duplicate := seenResponses[message.Uid]; duplicate {
				resolutions[message.Uid] = candidateHeaderResolution{}
				return nil
			}
			seenResponses[message.Uid] = struct{}{}
			body := message.GetBody(section)
			if body == nil {
				return nil
			}
			raw, truncated, readErr := readLiteral(body, maxHeaderBytes)
			if readErr != nil || truncated {
				return nil
			}
			parsed, parseErr := stdmail.ReadMessage(bytes.NewReader(raw))
			if parseErr != nil {
				return nil
			}
			aliasID, determinate := classifyRecipientAlias(parsed.Header, aliases, allowWeak)
			resolutions[message.Uid] = candidateHeaderResolution{aliasID: aliasID, determinate: determinate}
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("fetch candidate headers: %w", err)
		}
	}

	winners := make(map[int64]uint32)
	empty := make(map[int64]struct{})
	for aliasID, candidates := range aliasCandidates {
		fullyResolved := len(candidates) > 0 && !aliasCandidatesTruncated[aliasID]
		for _, uid := range candidates {
			if _, requested := requestedUIDs[uid]; !requested {
				fullyResolved = false
				break
			}
			resolution, fetched := resolutions[uid]
			if !fetched || !resolution.determinate {
				fullyResolved = false
				break
			}
			if resolution.aliasID == aliasID {
				winners[aliasID] = uid
				break
			}
		}
		if winners[aliasID] == 0 && fullyResolved {
			empty[aliasID] = struct{}{}
		}
	}
	return winners, empty, nil
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
				if !truncated {
					return nil
				}
				parsed = parsedMessage{bodyTruncated: true}
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

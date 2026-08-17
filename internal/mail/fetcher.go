package mail

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

const (
	defaultIMAPTimeout              = 8 * time.Second
	defaultMaxAliases               = domain.MaxEnabledAliasesPerAccount
	defaultMaxCandidates            = 1024
	defaultMaxIncrementalCandidates = 256
	defaultMaxHeaderBytes           = 128 << 10
	defaultMaxMessageBytes          = 100 << 20
	defaultMaxBodyBytes             = 512 << 10
	// Sequence discovery includes one leading/trailing sentinel around a batch.
	maxSequenceFetchMessages = defaultMaxCandidates + 1
)

var (
	ErrAccountDisabled      = errors.New("mail account is disabled")
	ErrAliasAccountMismatch = errors.New("alias does not belong to account")
	ErrInvalidAlias         = errors.New("invalid alias")
	ErrInvalidIMAPConfig    = errors.New("invalid IMAP configuration")
	ErrTooManyAliases       = errors.New("too many aliases")
)

// Fetcher incrementally archives every message after the committed account
// cursor. It never changes upstream flags and never backfills the mailbox when
// a cursor is first created or UIDVALIDITY changes.
type Fetcher struct {
	IMAPTimeout time.Duration
	MaxAliases  int
	// MaxCandidates bounds the recent UID window established on a reset.
	MaxCandidates             int
	MaxIncrementalCandidates  int
	MaxHeaderBytes            int
	MaxMessageBytes           int
	MaxBodyBytes              int
	MaxParsedMessageBytes     int
	AllowWeakRecipientHeaders bool
	// ArchiveTempDir receives complete MIME literals before the store publishes
	// them atomically into the archive tree.
	ArchiveTempDir string

	now func() time.Time
}

// NewFetcher returns a fetcher with production defaults. Configuration may
// override the exported limits before the first synchronization.
func NewFetcher() *Fetcher {
	return &Fetcher{
		IMAPTimeout:              defaultIMAPTimeout,
		MaxAliases:               defaultMaxAliases,
		MaxCandidates:            defaultMaxCandidates,
		MaxIncrementalCandidates: defaultMaxIncrementalCandidates,
		MaxHeaderBytes:           defaultMaxHeaderBytes,
		MaxMessageBytes:          defaultMaxMessageBytes,
		MaxBodyBytes:             defaultMaxBodyBytes,
		now:                      time.Now,
	}
}

// FetchIncremental reads one bounded account-level UID window and routes each
// message locally to every matching alias.
func (f *Fetcher) FetchIncremental(
	ctx context.Context,
	account domain.Account,
	password string,
	aliases []domain.Alias,
	previous *domain.IMAPSyncState,
	snapshotPositions map[int64]domain.MailboxSnapshotPosition,
) (domain.MailboxSyncResult, error) {
	return f.fetchArchiveIncremental(
		ctx, account, password, aliases, previous, snapshotPositions, f.settings(),
	)
}

type fetchSettings struct {
	timeout                   time.Duration
	maxAliases                int
	maxCandidates             int
	maxIncrementalCandidates  int
	maxHeaderBytes            int
	maxMessageBytes           int
	maxBodyBytes              int
	maxParsedMessageBytes     int
	allowWeakRecipientHeaders bool
	now                       func() time.Time
	archiveTempDir            string
}

func (f *Fetcher) settings() fetchSettings {
	settings := fetchSettings{
		timeout:                  defaultIMAPTimeout,
		maxAliases:               defaultMaxAliases,
		maxCandidates:            defaultMaxCandidates,
		maxIncrementalCandidates: defaultMaxIncrementalCandidates,
		maxHeaderBytes:           defaultMaxHeaderBytes,
		maxMessageBytes:          defaultMaxMessageBytes,
		maxBodyBytes:             defaultMaxBodyBytes,
		now:                      time.Now,
	}
	if f == nil {
		settings.maxParsedMessageBytes = settings.maxBodyBytes + defaultMetadataResultBytes
		return settings
	}
	if f.IMAPTimeout > 0 {
		settings.timeout = f.IMAPTimeout
	}
	if f.MaxAliases > 0 {
		settings.maxAliases = min(f.MaxAliases, domain.MaxEnabledAliasesPerAccount)
	}
	if f.MaxCandidates > 0 {
		settings.maxCandidates = min(f.MaxCandidates, defaultMaxCandidates)
	}
	if f.MaxIncrementalCandidates > 0 {
		settings.maxIncrementalCandidates = min(f.MaxIncrementalCandidates, defaultMaxIncrementalCandidates)
	}
	if f.MaxHeaderBytes > 0 {
		settings.maxHeaderBytes = f.MaxHeaderBytes
	}
	if f.MaxMessageBytes > 0 {
		settings.maxMessageBytes = min(f.MaxMessageBytes, defaultMaxMessageBytes)
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
	settings.allowWeakRecipientHeaders = f.AllowWeakRecipientHeaders
	settings.archiveTempDir = strings.TrimSpace(f.ArchiveTempDir)
	if f.now != nil {
		settings.now = f.now
	}
	return settings
}

func prepareAliases(account domain.Account, aliases []domain.Alias, maxAliases int) (map[string][]int64, error) {
	byAddress := make(map[string][]int64)
	seenIDs := make(map[int64]struct{})
	// The fixed capacity belongs to the iCloud Hide My Email workflow. Custom
	// mailboxes deliberately have no cumulative alias limit, so their complete
	// enabled set must remain routable after it grows beyond that capacity.
	limitEnabledAliases := domain.NormalizeMailboxType(account.MailboxType) != domain.MailboxTypeCustom
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
		if limitEnabledAliases && maxAliases > 0 && len(seenIDs) >= maxAliases {
			return nil, ErrTooManyAliases
		}
		seenIDs[alias.ID] = struct{}{}
		address, ok := normalizeAliasAddress(alias.Address)
		if !ok {
			return nil, fmt.Errorf("%w: alias %d address", ErrInvalidAlias, alias.ID)
		}
		byAddress[address] = append(byAddress[address], alias.ID)
	}
	return byAddress, nil
}

func accountEndpoint(account domain.Account) (host, address, username string, err error) {
	if strings.TrimSpace(account.IMAPHost) == "" {
		account.IMAPHost = domain.DefaultIMAPHost
	}
	if account.IMAPPort == 0 {
		account.IMAPPort = domain.DefaultIMAPPort
	}
	var port int
	host, port, err = domain.NormalizeIMAPEndpoint(account.IMAPHost, account.IMAPPort)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: %v", ErrInvalidIMAPConfig, err)
	}
	username = strings.TrimSpace(account.IMAPUsername)
	if username == "" {
		username = strings.TrimSpace(account.Email)
	}
	if username == "" {
		return "", "", "", fmt.Errorf("%w: empty username", ErrInvalidIMAPConfig)
	}
	return host, net.JoinHostPort(host, strconv.Itoa(port)), username, nil
}

func validateIMAPAccount(account domain.Account, password string) error {
	if !account.Enabled {
		return ErrAccountDisabled
	}
	if password == "" {
		return fmt.Errorf("%w: empty password", ErrInvalidIMAPConfig)
	}
	return nil
}

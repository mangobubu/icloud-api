package domain

import "time"

const (
	SyncStatusPending = "pending"
	SyncStatusOK      = "ok"
	SyncStatusError   = "error"

	AppleAliasConfirmationPending = "APPLE_ALIAS_CONFIRMATION_PENDING"

	// Alias credential modes identify which public contract an alias uses.
	// Legacy aliases retain their original API key and v1 mailbox state, while
	// v2 aliases use the versioned credential bundle and archive APIs.
	AliasCredentialModeLegacy = "legacy"
	AliasCredentialModeV2     = "v2"

	MaxEnabledAliasesPerAccount = 1000
)

type Admin struct {
	ID              int64
	Username        string
	PasswordHash    string
	PasswordVersion int64
	CreatedAt       time.Time
}

type Session struct {
	AdminID         int64
	Username        string
	PasswordVersion int64
	CSRF            string
	ExpiresAt       time.Time
}

type Account struct {
	ID                 int64
	Name               string
	Email              string
	IMAPHost           string
	IMAPPort           int
	IMAPUsername       string
	PasswordCiphertext string
	Enabled            bool
	LastSyncStatus     string
	LastSyncError      string
	LastSyncedAt       *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	AliasCount         int
}

// AppleWebSession stores the encrypted Apple web session associated with an
// account. Ciphertext is opaque to the persistence layer.
type AppleWebSession struct {
	AccountID       int64
	Ciphertext      string
	AppleID         string
	Region          string
	Authenticated   bool
	LastValidatedAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// AliasCreationSchedule is the persisted per-account plan used by the
// Hide My Email auto-creation worker. PlannedAt contains only future attempts
// and is stored as absolute UTC times so process restarts do not reshuffle a
// live cycle.
type AliasCreationSchedule struct {
	AccountID        int64
	Enabled          bool
	PlannedAt        []time.Time
	NextRunAt        *time.Time
	LastAttemptedAt  *time.Time
	LastCreatedAt    *time.Time
	LastAliasAddress string
	LastError        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// PendingAliasAPIKey is an automatically created alias whose one-time raw
// API key is still waiting for an administrator to retrieve and acknowledge.
// APIKeyCiphertext remains encrypted and is only handled by trusted service
// code.
type PendingAliasAPIKey struct {
	// Alias is embedded so both the original pending.Alias.Address shape and
	// callers that treated the pending value as an Alias (pending.Address) keep
	// compiling during the credential transition.
	Alias
	APIKeyCiphertext string
	CreatedAt        time.Time
}

type Alias struct {
	ID                   int64
	AccountID            int64
	AccountEmail         string
	Address              string
	Label                string
	APIKeyHash           []byte
	APIKeyPrefix         string
	CredentialMode       string
	CredentialCiphertext string
	IMAPPasswordHash     []byte
	OAuthClientID        string
	RefreshTokenHash     []byte
	CredentialVersion    int64
	MailboxUIDValidity   uint32
	MailboxUIDNext       uint32
	Enabled              bool
	LastSyncStatus       string
	LastSyncError        string
	LastSyncedAt         *time.Time
	LastAccessedAt       *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
	LatestReceivedAt     *time.Time
}

// AliasCredentials is the complete administrator-visible credential bundle
// issued for one privacy address. It is encrypted as one versioned record at
// rest; the individual hashes on Alias are used only for authentication.
type AliasCredentials struct {
	APIKey       string `json:"api_key"`
	IMAPPassword string `json:"imap_password"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
}

// AliasCredentialMaterial contains the encrypted bundle and its lookup hashes.
// Plaintext credentials never enter SQL arguments through this type.
type AliasCredentialMaterial struct {
	Ciphertext string
	APIKeyHash []byte
	// APIKeyPrefix is the administrator-visible non-secret prefix of the
	// API key. It is persisted alongside the lookup hash so list/detail DTOs
	// remain useful after the plaintext bundle is discarded from memory.
	APIKeyPrefix     string
	IMAPPasswordHash []byte
	OAuthClientID    string
	RefreshTokenHash []byte
	Version          int64
}

// AliasImportCandidate is a remotely discovered alias. APIKeyHash and
// APIKeyPrefix remain part of the original import contract: legacy callers
// generate the key before calling ImportAliases and expect those values to be
// persisted unchanged. The richer v2 import path may leave them empty because
// the store issues and returns its credential bundle transactionally.
type AliasImportCandidate struct {
	Address      string
	Label        string
	APIKeyHash   []byte
	APIKeyPrefix string
	Active       bool
}

type AliasImportConflict struct {
	Address              string
	ExistingAliasID      int64
	ExistingAccountID    int64
	ExistingAccountEmail string
}

type AliasImportResult struct {
	Created               []Alias
	Existing              []Alias
	Conflicts             []AliasImportConflict
	ImportedDisabledCount int
}

// AliasImportCredential is transient metadata for the trusted service that
// initiated an import. It preserves the existing one-time API-key response
// without introducing another plaintext database field.
type AliasImportCredential struct {
	Alias  Alias
	APIKey string
}

type MailAddress struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

type ArchivedMessage struct {
	AccountID    int64
	UIDValidity  uint32
	UID          uint32
	MessageID    string
	InternalDate time.Time
	HeaderDate   *time.Time
	From         []MailAddress
	To           []MailAddress
	CC           []MailAddress
	Subject      string
	TextBody     string
	HTMLBody     string
	Attachments  []Attachment
	// RawMIME is retained for small in-process fixtures. Production sync writes
	// the literal to RawMIMEPath and never buffers a large message in memory.
	RawMIME       []byte
	RawMIMEPath   string
	RawSize       int64
	RawSHA256     string
	ContentState  string
	OTP           string
	AliasIDs      []int64
	SyncedAt      time.Time
	BodyTruncated bool
	// UpstreamSeen is an in-process observation from the IMAP FETCH FLAGS
	// response. It is deliberately not persisted: v2 retains every archived
	// message, while the legacy latest-message projection only accepts messages
	// that were unread upstream when they were discovered.
	UpstreamSeen bool
}

const (
	ArchiveContentAvailable = "available"
	ArchiveContentEvicted   = "evicted"
	ArchiveContentMetadata  = "metadata_only"
	ArchiveContentOversized = "oversized"
	ArchiveContentMissing   = "missing"
)

type ArchivedMailboxMessage struct {
	ID            int64
	AliasID       int64
	MailboxUID    uint32
	UIDValidity   uint32
	MessageID     string
	InternalDate  time.Time
	HeaderDate    *time.Time
	From          []MailAddress
	To            []MailAddress
	CC            []MailAddress
	Subject       string
	ContentPath   string
	ContentBytes  int64
	ContentSHA256 string
	ContentState  string
	OTP           string
	BodyTruncated bool
	ArchivedAt    time.Time
}

type OTPRecord struct {
	OTP  string
	Time time.Time
}

// SnapshotState describes the confidence of a legacy latest-mail snapshot.
// The v1 API only exposes snapshots that are found; the other states are kept
// for callers that need to distinguish an authoritative empty mailbox from an
// unavailable one.
type SnapshotState string

const (
	SnapshotFound   SnapshotState = "found"
	SnapshotEmpty   SnapshotState = "empty"
	SnapshotUnknown SnapshotState = "unknown"
)

// LatestMessage is the compact, one-row mailbox view retained for the legacy
// mail endpoints. It intentionally mirrors the fields needed by the old
// response contract; the archive tables remain the source of truth for v2.
type LatestMessage struct {
	AliasID       int64
	UIDValidity   uint32
	UID           uint32
	MessageID     string
	InternalDate  time.Time
	HeaderDate    *time.Time
	From          []MailAddress
	To            []MailAddress
	CC            []MailAddress
	Subject       string
	TextBody      string
	HTMLBody      string
	Attachments   []Attachment
	BodyTruncated bool
	SyncedAt      time.Time
	SnapshotState SnapshotState `json:"-"`
}

// IMAPSyncState is the account-level cursor for one selected mailbox
// generation. LastUID is the examined high-water mark. A reset establishes the
// no-backfill boundary at the current upstream UID; subsequent runs archive
// every new UID regardless of its Seen flag. UpdatedAt is when this mailbox
// position was observed, not when it committed.
type IMAPSyncState struct {
	AccountID   int64
	UIDValidity uint32
	LastUID     uint32
	UpdatedAt   time.Time
}

// SeenTask is a durable request to mark one message as seen in its account's
// selected mailbox. UIDVALIDITY scopes UID to one mailbox generation.
type SeenTask struct {
	AccountID   int64
	UIDValidity uint32
	UID         uint32
	CreatedAt   time.Time
}

// MailboxSnapshotPosition identifies one persisted v1 latest-mail projection.
// Sync validates these positions in one shared IMAP command so an expunged
// message cannot remain visible through the compatibility endpoints.
type MailboxSnapshotPosition struct {
	AliasID     int64
	UIDValidity uint32
	UID         uint32
}

// MailboxSyncResult contains the newly archived messages and committed cursor
// for one bounded account-level sync batch.
type MailboxSyncResult struct {
	ArchivedMessages      []ArchivedMessage
	LegacySnapshotUpdates map[int64]LatestMessage
	State                 IMAPSyncState
	Reset                 bool
	// HasMore means mailbox UIDs beyond State.LastUID still need inspection. It
	// can be true even when all remaining messages are already read, and for the
	// first Reset batch; later batches continue as ordinary increments.
	HasMore bool
	// TargetUID is the selected mailbox upper bound observed for this batch.
	// It is transient progress metadata and is not persisted with the cursor.
	TargetUID uint32
}

type MailboxBinding struct {
	Alias   Alias
	Account Account
	Message *LatestMessage
}

type AuditLog struct {
	ID           int64
	AdminID      *int64
	Username     string
	Action       string
	ResourceType string
	ResourceID   string
	Result       string
	IP           string
	RequestID    string
	Detail       string
	CreatedAt    time.Time
}

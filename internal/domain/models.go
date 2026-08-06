package domain

import "time"

const (
	SyncStatusPending = "pending"
	SyncStatusOK      = "ok"
	SyncStatusError   = "error"

	MaxEnabledAliasesPerAccount = 256
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

type Alias struct {
	ID               int64
	AccountID        int64
	AccountEmail     string
	Address          string
	Label            string
	APIKeyHash       []byte
	APIKeyPrefix     string
	Enabled          bool
	LastSyncStatus   string
	LastSyncError    string
	LastSyncedAt     *time.Time
	LastAccessedAt   *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	LatestReceivedAt *time.Time
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

type SnapshotState string

const (
	SnapshotFound   SnapshotState = "found"
	SnapshotEmpty   SnapshotState = "empty"
	SnapshotUnknown SnapshotState = "unknown"
)

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

package hmesync

import (
	"context"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

const (
	StatusLoginRequired        = "login_required"
	StatusVerificationRequired = "verification_required"
	StatusAuthenticated        = "authenticated"
	StatusExpired              = "expired"

	RegionGlobal = "global"
	RegionChina  = "cn"
)

// Repository is the persistence surface needed by the Apple directory sync.
// Apple credentials and verification codes are deliberately absent.
type Repository interface {
	GetAccount(context.Context, int64) (domain.Account, error)
	GetAppleWebSession(context.Context, int64) (domain.AppleWebSession, error)
	UpsertAppleWebSession(context.Context, domain.AppleWebSession) (domain.AppleWebSession, error)
	DeleteAppleWebSession(context.Context, int64) error
	ImportAliases(context.Context, int64, []domain.AliasImportCandidate) (domain.AliasImportResult, error)
}

type SessionCipher interface {
	EncryptAppleSession(string) (string, error)
	DecryptAppleSession(string) (string, error)
}

type AppleClient interface {
	SignIn(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error)
	VerifyCode(context.Context, apple.Session, string) (apple.Session, error)
	Validate(context.Context, apple.Session) (apple.Session, error)
	ListAliases(context.Context, apple.Session) (apple.ListResult, apple.Session, error)
}

// AccountLocker is shared with IMAP synchronization. Apple network requests
// complete before this lock is acquired.
type AccountLocker interface {
	WithAccountLock(context.Context, int64, func() error) error
}

type SessionInfo struct {
	Status          string
	AppleID         string
	Region          string
	AuthenticatedAt *time.Time
	ExpiresAt       *time.Time
}

type AuthResult struct {
	Status      string
	ChallengeID string
	Session     SessionInfo
}

type SyncSummary struct {
	Total                 int
	CreatedCount          int
	ExistingCount         int
	InactiveCount         int
	ImportedDisabledCount int
	ConflictCount         int
	FilteredOutCount      int
}

type CreatedAlias struct {
	Alias  domain.Alias
	APIKey string
}

type SyncResult struct {
	Summary SyncSummary
	Created []CreatedAlias
	Session SessionInfo
}

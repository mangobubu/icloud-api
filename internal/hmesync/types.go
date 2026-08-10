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

// AutoCreateRepository is the additional persistence surface used only by the
// background creator. Keeping it separate preserves source compatibility for
// callers that only implement interactive Apple directory sync.
type AutoCreateRepository interface {
	Repository
	CountEnabledAliasesByAccount(context.Context, int64) (int, error)
	CreateAliasWithPendingAPIKey(context.Context, domain.AppleWebSession, domain.Alias, string) (domain.Alias, domain.AppleWebSession, error)
	GetPendingAutoAliasConfirmation(context.Context, int64) (domain.PendingAliasAPIKey, error)
	ConfirmPendingAutoAlias(context.Context, domain.AppleWebSession, int64) (domain.Alias, domain.AppleWebSession, error)
}

// AliasDeletionRepository is the additional persistence surface used by
// synchronized Apple and local alias deletion.
type AliasDeletionRepository interface {
	GetAlias(context.Context, int64) (domain.Alias, error)
	DeleteAlias(context.Context, int64) error
}

type SessionCipher interface {
	EncryptAppleSession(string) (string, error)
	DecryptAppleSession(string) (string, error)
}

type PendingAPIKeyCipher interface {
	EncryptPendingAliasAPIKey(string) (string, error)
}

type AppleClient interface {
	SignIn(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error)
	VerifyCode(context.Context, apple.Session, string) (apple.Session, error)
	Validate(context.Context, apple.Session) (apple.Session, error)
	ListAliases(context.Context, apple.Session) (apple.ListResult, apple.Session, error)
}

type AutoAliasClient interface {
	CreateAlias(context.Context, apple.Session, string, string) (apple.Alias, apple.Session, error)
}

// ForwardingTargetUpdater is optional so existing AppleClient implementations
// can still create aliases when Apple already exposes selectedForwardTo.
type ForwardingTargetUpdater interface {
	UpdateForwardTo(context.Context, apple.Session, string) (apple.Session, error)
}

// AliasDeletionClient exposes Apple's two-step permanent deletion flow.
// Active aliases must be deactivated before they can be deleted.
type AliasDeletionClient interface {
	DeactivateAlias(context.Context, apple.Session, string) (apple.Session, error)
	DeleteAlias(context.Context, apple.Session, string) (apple.Session, error)
}

// AccountLocker is shared with IMAP synchronization. Interactive Apple flows
// do their network work before acquiring this publication lock.
type AccountLocker interface {
	WithAccountLock(context.Context, int64, func() error) error
}

// AccountLockAcquirer is an optional stronger boundary used by operations that
// have an irreversible remote side effect followed by local publication. The
// production sync manager implements it; keeping it separate preserves source
// compatibility for embedders that only provide AccountLocker.
type AccountLockAcquirer interface {
	AcquireAccountLock(context.Context, int64) (func(), error)
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

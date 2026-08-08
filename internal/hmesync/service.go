package hmesync

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"sync"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

const (
	defaultChallengeTTL         = 10 * time.Minute
	defaultVerificationAttempts = 5
	autoCreateLabel             = "自动创建"
	autoCreateNote              = "icloud-api 自动创建"
)

type Option func(*Service)

func WithChallengeTTL(ttl time.Duration) Option {
	return func(service *Service) {
		if ttl > 0 {
			service.challengeTTL = ttl
		}
	}
}

func WithClock(now func() time.Time) Option {
	return func(service *Service) {
		if now != nil {
			service.now = now
		}
	}
}

type challenge struct {
	id           string
	ownerAdminID int64
	accountID    int64
	identity     accountIdentity
	appleID      string
	region       apple.Region
	session      apple.Session
	expiresAt    time.Time
	attempts     int
}

type accountIdentity struct {
	email        string
	imapUsername string
}

type operationLock struct {
	token chan struct{}
	refs  int
}

type Service struct {
	repo   Repository
	cipher SessionCipher
	client AppleClient
	locker AccountLocker
	now    func() time.Time

	challengeTTL  time.Duration
	maxAttempts   int
	challengeMu   sync.Mutex
	challenges    map[string]challenge
	accountFlows  map[int64]string
	operationMu   sync.Mutex
	operationLock map[int64]*operationLock
}

func New(repo Repository, cipher SessionCipher, client AppleClient, locker AccountLocker, options ...Option) (*Service, error) {
	if repo == nil {
		return nil, errors.New("HME sync repository is required")
	}
	if cipher == nil {
		return nil, errors.New("HME sync session cipher is required")
	}
	if client == nil {
		return nil, errors.New("Apple client is required")
	}
	if locker == nil {
		return nil, errors.New("account locker is required")
	}
	service := &Service{
		repo:          repo,
		cipher:        cipher,
		client:        client,
		locker:        locker,
		now:           func() time.Time { return time.Now().UTC() },
		challengeTTL:  defaultChallengeTTL,
		maxAttempts:   defaultVerificationAttempts,
		challenges:    make(map[string]challenge),
		accountFlows:  make(map[int64]string),
		operationLock: make(map[int64]*operationLock),
	}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service, nil
}

func (s *Service) StartAuth(
	ctx context.Context,
	ownerAdminID, accountID int64,
	appleID, password string,
	region apple.Region,
) (AuthResult, error) {
	if ownerAdminID < 1 || accountID < 1 {
		return AuthResult{}, errors.New("owner and account IDs must be positive")
	}
	appleID = strings.TrimSpace(appleID)
	if appleID == "" || password == "" {
		return AuthResult{}, errors.New("Apple ID and password are required")
	}
	region, err := normalizeRegion(region)
	if err != nil {
		return AuthResult{}, err
	}
	release, err := s.acquireOperation(ctx, accountID)
	if err != nil {
		return AuthResult{}, err
	}
	defer release()
	defer func() { password = "" }()

	account, err := s.repo.GetAccount(ctx, accountID)
	if err != nil {
		return AuthResult{}, err
	}
	s.deleteAccountChallenge(accountID)
	previous := s.previousSession(ctx, accountID, appleID, region)
	session, needsVerification, err := s.client.SignIn(ctx, appleID, password, region, previous)
	password = ""
	if err != nil {
		return AuthResult{}, mapAppleError(err, false)
	}
	normalizeSession(&session, appleID, region, s.now())

	if needsVerification {
		if err := s.removeSessionForIdentity(ctx, accountID, identityOf(account)); err != nil {
			return AuthResult{}, err
		}
		flow, err := s.newChallenge(ownerAdminID, account, appleID, region, session)
		if err != nil {
			return AuthResult{}, fmt.Errorf("create Apple verification challenge: %w", err)
		}
		info := sessionInfoFromSession(session, StatusVerificationRequired, nil, &flow.expiresAt)
		return AuthResult{Status: StatusVerificationRequired, ChallengeID: flow.id, Session: info}, nil
	}

	record, err := s.persistSession(ctx, accountID, identityOf(account), session)
	if err != nil {
		return AuthResult{}, err
	}
	info := sessionInfoFromRecord(record, session, StatusAuthenticated)
	return AuthResult{Status: StatusAuthenticated, Session: info}, nil
}

func (s *Service) VerifyAuth(
	ctx context.Context,
	ownerAdminID, accountID int64,
	challengeID, code string,
) (AuthResult, error) {
	if ownerAdminID < 1 || accountID < 1 {
		return AuthResult{}, errors.New("owner and account IDs must be positive")
	}
	challengeID = strings.TrimSpace(challengeID)
	code = strings.TrimSpace(code)
	if challengeID == "" || code == "" {
		return AuthResult{}, wrapError(CodeFlowExpired, ErrFlowExpired, nil)
	}
	release, err := s.acquireOperation(ctx, accountID)
	if err != nil {
		return AuthResult{}, err
	}
	defer release()
	defer func() { code = "" }()

	flow, ok := s.readChallenge(challengeID, ownerAdminID, accountID)
	if !ok {
		return AuthResult{}, wrapError(CodeFlowExpired, ErrFlowExpired, nil)
	}
	account, err := s.repo.GetAccount(ctx, accountID)
	if err != nil {
		s.deleteChallenge(challengeID)
		return AuthResult{}, err
	}
	if !sameIdentity(identityOf(account), flow.identity) {
		s.deleteChallenge(challengeID)
		return AuthResult{}, wrapError(CodeAccountChanged, ErrAccountChanged, nil)
	}

	session, err := s.client.VerifyCode(ctx, flow.session, code)
	code = ""
	if err != nil {
		mapped := mapAppleError(err, true)
		if errors.Is(mapped, ErrVerificationInvalid) {
			s.recordFailedAttempt(flow)
		} else {
			s.deleteChallenge(challengeID)
		}
		return AuthResult{}, mapped
	}
	normalizeSession(&session, flow.appleID, flow.region, s.now())
	record, err := s.persistSession(ctx, accountID, flow.identity, session)
	if err != nil {
		s.deleteChallenge(challengeID)
		return AuthResult{}, err
	}
	s.deleteChallenge(challengeID)
	info := sessionInfoFromRecord(record, session, StatusAuthenticated)
	return AuthResult{Status: StatusAuthenticated, Session: info}, nil
}

func (s *Service) GetSession(ctx context.Context, accountID int64) (SessionInfo, error) {
	if accountID < 1 {
		return SessionInfo{}, errors.New("account ID must be positive")
	}
	record, err := s.repo.GetAppleWebSession(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		return SessionInfo{Status: StatusLoginRequired}, nil
	}
	if err != nil {
		return SessionInfo{}, err
	}
	session, err := s.decryptSession(record)
	if err != nil || !record.Authenticated {
		return SessionInfo{
			Status:  StatusExpired,
			AppleID: record.AppleID,
			Region:  publicRegion(record.Region),
		}, nil
	}
	info := sessionInfoFromRecord(record, session, StatusAuthenticated)
	if info.ExpiresAt != nil && !info.ExpiresAt.After(s.now()) {
		info.Status = StatusExpired
	}
	return info, nil
}

func (s *Service) ClearAuth(ctx context.Context, accountID int64) error {
	if accountID < 1 {
		return errors.New("account ID must be positive")
	}
	release, err := s.acquireOperation(ctx, accountID)
	if err != nil {
		return err
	}
	defer release()
	s.deleteAccountChallenge(accountID)
	return s.locker.WithAccountLock(ctx, accountID, func() error {
		err := s.repo.DeleteAppleWebSession(ctx, accountID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
}

func (s *Service) SyncAliases(ctx context.Context, accountID int64) (SyncResult, error) {
	if accountID < 1 {
		return SyncResult{}, errors.New("account ID must be positive")
	}
	release, err := s.acquireOperation(ctx, accountID)
	if err != nil {
		return SyncResult{}, err
	}
	defer release()

	account, err := s.repo.GetAccount(ctx, accountID)
	if err != nil {
		return SyncResult{}, err
	}
	record, session, err := s.loadSession(ctx, accountID)
	if err != nil {
		if errors.Is(err, ErrSessionExpired) {
			s.expireSession(ctx, accountID)
		}
		return SyncResult{}, err
	}
	validated, err := s.client.Validate(ctx, session)
	if err != nil {
		mapped := mapAppleError(err, false)
		if errors.Is(mapped, ErrSessionExpired) {
			s.expireSession(ctx, accountID)
		}
		return SyncResult{}, mapped
	}
	normalizeSession(&validated, record.AppleID, session.Region, s.now())
	list, updated, err := s.client.ListAliases(ctx, validated)
	if err != nil {
		mapped := mapAppleError(err, false)
		if errors.Is(mapped, ErrSessionExpired) {
			s.expireSession(ctx, accountID)
		}
		return SyncResult{}, mapped
	}
	normalizeSession(&updated, record.AppleID, validated.Region, s.now())

	filtered, inactive, err := filterAliases(list, account.Email)
	if err != nil {
		return SyncResult{}, err
	}
	candidates := make([]domain.AliasImportCandidate, 0, len(filtered))
	rawKeys := make(map[string]string, len(filtered))
	for _, remote := range filtered {
		raw, hash, prefix, err := secure.NewAPIKey()
		if err != nil {
			return SyncResult{}, fmt.Errorf("generate imported alias API key: %w", err)
		}
		address := domain.NormalizeEmail(remote.HME)
		rawKeys[address] = raw
		candidates = append(candidates, domain.AliasImportCandidate{
			Address:      address,
			Label:        strings.TrimSpace(remote.Label),
			APIKeyHash:   hash,
			APIKeyPrefix: prefix,
			Active:       remote.IsActive,
		})
	}

	var imported domain.AliasImportResult
	var saved domain.AppleWebSession
	err = s.locker.WithAccountLock(ctx, accountID, func() error {
		current, err := s.repo.GetAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if !sameIdentity(identityOf(current), identityOf(account)) {
			return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
		}
		saved, err = s.saveSession(ctx, accountID, updated)
		if err != nil {
			return err
		}
		imported, err = s.repo.ImportAliases(ctx, accountID, candidates)
		if errors.Is(err, store.ErrAliasOwnershipConflict) {
			return wrapError(CodeAliasOwnershipConflict, ErrAliasOwnershipConflict, err)
		}
		return err
	})
	if err != nil {
		return SyncResult{}, err
	}

	created := make([]CreatedAlias, 0, len(imported.Created))
	for _, alias := range imported.Created {
		created = append(created, CreatedAlias{Alias: alias, APIKey: rawKeys[domain.NormalizeEmail(alias.Address)]})
	}
	return SyncResult{
		Summary: SyncSummary{
			Total:                 len(filtered),
			CreatedCount:          len(imported.Created),
			ExistingCount:         len(imported.Existing),
			InactiveCount:         inactive,
			ImportedDisabledCount: imported.ImportedDisabledCount,
			ConflictCount:         len(imported.Conflicts),
			FilteredOutCount:      len(list.Aliases) - len(filtered),
		},
		Created: created,
		Session: sessionInfoFromRecord(saved, updated, StatusAuthenticated),
	}, nil
}

// CreateAutoAlias reserves exactly one Hide My Email address and publishes it
// locally with a one-time API key sealed for administrator retrieval. The
// method intentionally performs no retry: reserve is a remote side effect and
// repeating it after an ambiguous response could create duplicates.
func (s *Service) CreateAutoAlias(ctx context.Context, accountID int64) (domain.Alias, error) {
	if accountID < 1 {
		return domain.Alias{}, errors.New("account ID must be positive")
	}
	autoRepo, ok := s.repo.(AutoCreateRepository)
	if !ok {
		return domain.Alias{}, errors.New("automatic alias creation persistence is unavailable")
	}
	autoClient, ok := s.client.(AutoAliasClient)
	if !ok {
		return domain.Alias{}, errors.New("automatic alias creation client is unavailable")
	}
	keyCipher, ok := s.cipher.(PendingAPIKeyCipher)
	if !ok {
		return domain.Alias{}, errors.New("automatic alias key encryption is unavailable")
	}
	release, err := s.acquireOperation(ctx, accountID)
	if err != nil {
		return domain.Alias{}, err
	}
	defer release()

	// A successful Apple reserve cannot be rolled back. The production sync
	// manager therefore holds its keyed account lock across the capacity check,
	// remote request, and local publication so account deletion, disabling, or
	// another local alias write cannot consume the last slot in the meantime.
	var releaseAccount func()
	if acquirer, ok := s.locker.(AccountLockAcquirer); ok {
		releaseAccount, err = acquirer.AcquireAccountLock(ctx, accountID)
		if err != nil {
			return domain.Alias{}, err
		}
		defer releaseAccount()
	}
	expireAutoSession := func(sessionErr error) error {
		var deleteErr error
		if releaseAccount != nil {
			// The production locker is deliberately non-reentrant. The account
			// lock already protects this deletion across the remote operation.
			deleteErr = s.repo.DeleteAppleWebSession(ctx, accountID)
		} else {
			deleteErr = s.locker.WithAccountLock(ctx, accountID, func() error {
				err := s.repo.DeleteAppleWebSession(ctx, accountID)
				if errors.Is(err, store.ErrNotFound) {
					return nil
				}
				return err
			})
		}
		if errors.Is(deleteErr, store.ErrNotFound) {
			deleteErr = nil
		}
		if deleteErr == nil {
			return sessionErr
		}
		return wrapError(CodeSessionExpired, ErrSessionExpired, errors.Join(
			sessionErr,
			fmt.Errorf("delete expired Apple session: %w", deleteErr),
		))
	}

	account, err := s.repo.GetAccount(ctx, accountID)
	if err != nil {
		return domain.Alias{}, err
	}
	if !account.Enabled {
		return domain.Alias{}, errors.New("主号已停用")
	}
	count, err := autoRepo.CountEnabledAliasesByAccount(ctx, accountID)
	if err != nil {
		return domain.Alias{}, err
	}
	if count >= domain.MaxEnabledAliasesPerAccount {
		return domain.Alias{}, store.ErrAliasLimit
	}

	record, session, err := s.loadSession(ctx, accountID)
	if err != nil {
		if errors.Is(err, ErrSessionExpired) {
			err = expireAutoSession(err)
		}
		return domain.Alias{}, err
	}
	validated, err := s.client.Validate(ctx, session)
	if err != nil {
		mapped := mapAppleError(err, false)
		if errors.Is(mapped, ErrSessionExpired) {
			mapped = expireAutoSession(mapped)
		}
		return domain.Alias{}, mapped
	}
	if strings.TrimSpace(validated.AppleID) != "" &&
		!strings.EqualFold(strings.TrimSpace(validated.AppleID), strings.TrimSpace(record.AppleID)) {
		return domain.Alias{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	normalizeSession(&validated, record.AppleID, session.Region, s.now())

	// Apple reserve uses the account-wide selectedForwardTo setting and does
	// not accept a per-request forwarding address. Confirm that target before
	// the irreversible reserve call. The reserve response may omit
	// forwardToEmail, so checking only after creation both misclassifies a valid
	// response and can leave an untracked remote alias.
	settings, listedSession, err := s.client.ListAliases(ctx, validated)
	if err != nil {
		mapped := mapAppleError(err, false)
		if errors.Is(mapped, ErrSessionExpired) {
			mapped = expireAutoSession(mapped)
		}
		return domain.Alias{}, mapped
	}
	if strings.TrimSpace(listedSession.AppleID) != "" &&
		!strings.EqualFold(strings.TrimSpace(listedSession.AppleID), strings.TrimSpace(record.AppleID)) {
		return domain.Alias{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	normalizeSession(&listedSession, record.AppleID, validated.Region, s.now())
	listedRegion, err := normalizeRegion(listedSession.Region)
	if err != nil {
		return domain.Alias{}, err
	}
	listedSession.Region = listedRegion
	checkpointSession := func(session apple.Session) error {
		if releaseAccount != nil {
			_, err := s.saveSession(ctx, accountID, session)
			return err
		}
		_, err := s.persistSession(ctx, accountID, identityOf(account), session)
		return err
	}
	checkpointReturnedSession := func(returned *apple.Session, fallbackRegion apple.Region) error {
		if strings.TrimSpace(returned.AppleID) != "" &&
			!strings.EqualFold(strings.TrimSpace(returned.AppleID), strings.TrimSpace(record.AppleID)) {
			return wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
		}
		normalizeSession(returned, record.AppleID, fallbackRegion, s.now())
		region, err := normalizeRegion(returned.Region)
		if err != nil {
			return err
		}
		returned.Region = region
		return checkpointSession(*returned)
	}
	// ListAliases may rotate cookies even when the forwarding preflight rejects
	// the operation. Preserve that valid checkpoint so a corrected setting does
	// not force an otherwise unnecessary Apple login.
	if err := checkpointSession(listedSession); err != nil {
		return domain.Alias{}, err
	}
	if strings.TrimSpace(settings.SelectedForwardTo) == "" {
		return domain.Alias{}, wrapError(CodeUpstreamError, ErrUpstream,
			errors.New("Apple response omitted the selected forwarding address"))
	}
	if !sameEmail(settings.SelectedForwardTo, account.Email) {
		return domain.Alias{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}

	// Prepare every locally fallible key operation before reserve so a local key
	// failure cannot strand an address after Apple's irreversible side effect.
	rawKey, hash, prefix, err := secure.NewAPIKey()
	if err != nil {
		return domain.Alias{}, fmt.Errorf("generate automatic alias API key: %w", err)
	}
	ciphertext, err := keyCipher.EncryptPendingAliasAPIKey(rawKey)
	rawKey = ""
	if err != nil {
		return domain.Alias{}, fmt.Errorf("encrypt automatic alias API key: %w", err)
	}

	created, updated, err := autoClient.CreateAlias(ctx, listedSession, autoCreateLabel, autoCreateNote)
	if err != nil {
		mapped := mapAppleError(err, false)
		if errors.Is(mapped, ErrSessionExpired) {
			return domain.Alias{}, expireAutoSession(mapped)
		}
		if !errors.Is(mapped, context.Canceled) && !errors.Is(mapped, context.DeadlineExceeded) &&
			hasAppleSessionState(updated) {
			if checkpointErr := checkpointReturnedSession(&updated, listedSession.Region); checkpointErr != nil {
				return domain.Alias{}, checkpointErr
			}
		}
		return domain.Alias{}, mapped
	}
	if err := checkpointReturnedSession(&updated, listedSession.Region); err != nil {
		return domain.Alias{}, err
	}

	if strings.TrimSpace(created.ForwardToEmail) == "" {
		// A minimal reserve response does not identify the forwarding mailbox.
		// Re-read the new HME entry rather than treating the pre-reserve setting
		// as proof; it can be changed concurrently from another Apple session.
		confirmed, confirmedSession, listErr := s.client.ListAliases(ctx, updated)
		if listErr != nil {
			mapped := mapAppleError(listErr, false)
			if errors.Is(mapped, ErrSessionExpired) {
				mapped = expireAutoSession(mapped)
			}
			return domain.Alias{}, mapped
		}
		if err := checkpointReturnedSession(&confirmedSession, updated.Region); err != nil {
			return domain.Alias{}, err
		}
		confirmedAlias, found := findAppleAlias(confirmed.Aliases, created.HME)
		if !found {
			return domain.Alias{}, wrapError(CodeUpstreamError, ErrUpstream,
				errors.New("Apple list omitted the newly reserved alias"))
		}
		created = confirmedAlias
		updated = confirmedSession
	}

	address := domain.NormalizeEmail(created.HME)
	parsed, parseErr := mail.ParseAddress(address)
	if address == "" || parseErr != nil || parsed.Name != "" || domain.NormalizeEmail(parsed.Address) != address {
		return domain.Alias{}, wrapError(CodeUpstreamError, ErrUpstream, errors.New("Apple returned an invalid alias address"))
	}
	if !created.IsActive {
		return domain.Alias{}, wrapError(CodeUpstreamError, ErrUpstream, errors.New("Apple returned an inactive alias"))
	}
	if !sameEmail(created.ForwardToEmail, account.Email) {
		return domain.Alias{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	updatedRegion, err := normalizeRegion(updated.Region)
	if err != nil {
		return domain.Alias{}, err
	}
	updated.Region = updatedRegion
	if strings.TrimSpace(updated.AppleID) != "" &&
		!strings.EqualFold(strings.TrimSpace(updated.AppleID), strings.TrimSpace(record.AppleID)) {
		return domain.Alias{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	normalizeSession(&updated, record.AppleID, session.Region, s.now())
	updatedPayload, err := json.Marshal(updated)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("encode rotated Apple session: %w", err)
	}
	sessionCiphertext, err := s.cipher.EncryptAppleSession(string(updatedPayload))
	if err != nil {
		return domain.Alias{}, fmt.Errorf("encrypt rotated Apple session: %w", err)
	}
	alias := domain.Alias{
		AccountID:      accountID,
		AccountEmail:   account.Email,
		Address:        address,
		Label:          strings.TrimSpace(created.Label),
		APIKeyHash:     hash,
		APIKeyPrefix:   prefix,
		Enabled:        true,
		LastSyncStatus: domain.SyncStatusPending,
	}
	sessionRecord := domain.AppleWebSession{
		AccountID:       accountID,
		Ciphertext:      sessionCiphertext,
		AppleID:         strings.TrimSpace(updated.AppleID),
		Region:          publicRegion(string(updated.Region)),
		Authenticated:   true,
		LastValidatedAt: &updated.ValidatedAt,
	}
	if sessionRecord.LastValidatedAt.IsZero() {
		validatedAt := s.now().UTC()
		sessionRecord.LastValidatedAt = &validatedAt
	}
	var saved domain.Alias
	publish := func() error {
		current, err := s.repo.GetAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if !current.Enabled {
			return errors.New("主号已停用")
		}
		if !sameIdentity(identityOf(current), identityOf(account)) {
			return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
		}
		var saveErr error
		saved, _, saveErr = autoRepo.CreateAliasWithPendingAPIKey(ctx, sessionRecord, alias, ciphertext)
		return saveErr
	}
	if releaseAccount != nil {
		err = publish()
	} else {
		err = s.locker.WithAccountLock(ctx, accountID, publish)
	}
	if errors.Is(err, store.ErrAliasOwnershipConflict) {
		return domain.Alias{}, wrapError(CodeAliasOwnershipConflict, ErrAliasOwnershipConflict, err)
	}
	if err != nil {
		return domain.Alias{}, err
	}
	return saved, nil
}

func findAppleAlias(aliases []apple.Alias, address string) (apple.Alias, bool) {
	wanted := domain.NormalizeEmail(address)
	for _, alias := range aliases {
		if domain.NormalizeEmail(alias.HME) == wanted {
			return alias, true
		}
	}
	return apple.Alias{}, false
}

func hasAppleSessionState(session apple.Session) bool {
	return strings.TrimSpace(session.AppleID) != "" ||
		strings.TrimSpace(session.DSID) != "" ||
		strings.TrimSpace(session.PremiumMailSettingsURL) != "" ||
		strings.TrimSpace(session.SessionToken) != "" ||
		len(session.Cookies) > 0
}

// CreateAlias is a convenience alias for callers that treat one background
// slot as a normal single-alias operation.
func (s *Service) CreateAlias(ctx context.Context, accountID int64) (domain.Alias, error) {
	return s.CreateAutoAlias(ctx, accountID)
}

func (s *Service) persistSession(ctx context.Context, accountID int64, expected accountIdentity, session apple.Session) (domain.AppleWebSession, error) {
	var saved domain.AppleWebSession
	err := s.locker.WithAccountLock(ctx, accountID, func() error {
		current, err := s.repo.GetAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if !sameIdentity(identityOf(current), expected) {
			return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
		}
		saved, err = s.saveSession(ctx, accountID, session)
		return err
	})
	return saved, err
}

func (s *Service) removeSessionForIdentity(ctx context.Context, accountID int64, expected accountIdentity) error {
	return s.locker.WithAccountLock(ctx, accountID, func() error {
		current, err := s.repo.GetAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if !sameIdentity(identityOf(current), expected) {
			return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
		}
		err = s.repo.DeleteAppleWebSession(ctx, accountID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
}

func (s *Service) saveSession(ctx context.Context, accountID int64, session apple.Session) (domain.AppleWebSession, error) {
	payload, err := json.Marshal(session)
	if err != nil {
		return domain.AppleWebSession{}, fmt.Errorf("encode Apple session: %w", err)
	}
	ciphertext, err := s.cipher.EncryptAppleSession(string(payload))
	if err != nil {
		return domain.AppleWebSession{}, fmt.Errorf("encrypt Apple session: %w", err)
	}
	validatedAt := session.ValidatedAt
	if validatedAt.IsZero() {
		validatedAt = s.now().UTC()
	}
	return s.repo.UpsertAppleWebSession(ctx, domain.AppleWebSession{
		AccountID:       accountID,
		Ciphertext:      ciphertext,
		AppleID:         strings.TrimSpace(session.AppleID),
		Region:          publicRegion(string(session.Region)),
		Authenticated:   true,
		LastValidatedAt: &validatedAt,
	})
}

func (s *Service) loadSession(ctx context.Context, accountID int64) (domain.AppleWebSession, apple.Session, error) {
	record, err := s.repo.GetAppleWebSession(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.AppleWebSession{}, apple.Session{}, wrapError(CodeLoginRequired, ErrLoginRequired, err)
	}
	if err != nil {
		return domain.AppleWebSession{}, apple.Session{}, err
	}
	if !record.Authenticated {
		return record, apple.Session{}, wrapError(CodeLoginRequired, ErrLoginRequired, nil)
	}
	session, err := s.decryptSession(record)
	if err != nil {
		return record, apple.Session{}, wrapError(CodeSessionExpired, ErrSessionExpired, err)
	}
	return record, session, nil
}

func (s *Service) decryptSession(record domain.AppleWebSession) (apple.Session, error) {
	payload, err := s.cipher.DecryptAppleSession(record.Ciphertext)
	if err != nil {
		return apple.Session{}, fmt.Errorf("decrypt Apple session: %w", err)
	}
	var session apple.Session
	if err := json.Unmarshal([]byte(payload), &session); err != nil {
		return apple.Session{}, fmt.Errorf("decode Apple session: %w", err)
	}
	region, err := normalizeRegion(session.Region)
	recordRegion, recordRegionErr := normalizeRegion(apple.Region(record.Region))
	if err != nil || recordRegionErr != nil ||
		!strings.EqualFold(strings.TrimSpace(record.AppleID), strings.TrimSpace(session.AppleID)) ||
		recordRegion != region {
		return apple.Session{}, errors.New("stored Apple session metadata mismatch")
	}
	session.Region = region
	return session, nil
}

func (s *Service) previousSession(ctx context.Context, accountID int64, appleID string, region apple.Region) *apple.Session {
	record, err := s.repo.GetAppleWebSession(ctx, accountID)
	if err != nil || !strings.EqualFold(strings.TrimSpace(record.AppleID), appleID) ||
		publicRegion(record.Region) != publicRegion(string(region)) {
		return nil
	}
	session, err := s.decryptSession(record)
	if err != nil {
		return nil
	}
	return &session
}

func (s *Service) expireSession(ctx context.Context, accountID int64) {
	_ = s.locker.WithAccountLock(ctx, accountID, func() error {
		err := s.repo.DeleteAppleWebSession(ctx, accountID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
}

func filterAliases(result apple.ListResult, accountEmail string) ([]apple.Alias, int, error) {
	accountEmail = domain.NormalizeEmail(accountEmail)
	belongs := sameEmail(result.SelectedForwardTo, accountEmail)
	for _, address := range result.ForwardToEmails {
		belongs = belongs || sameEmail(address, accountEmail)
	}
	seenAddresses := make(map[string]struct{}, len(result.Aliases))
	seenRemoteIDs := make(map[string]struct{}, len(result.Aliases))
	filtered := make([]apple.Alias, 0, len(result.Aliases))
	inactive := 0
	for index, remote := range result.Aliases {
		address := domain.NormalizeEmail(remote.HME)
		parsed, err := mail.ParseAddress(address)
		if address == "" || err != nil || domain.NormalizeEmail(parsed.Address) != address {
			return nil, 0, wrapError(CodeUpstreamError, ErrUpstream,
				fmt.Errorf("Apple alias %d has invalid address", index))
		}
		if _, exists := seenAddresses[address]; exists {
			return nil, 0, wrapError(CodeUpstreamError, ErrUpstream,
				fmt.Errorf("Apple directory contains duplicate address"))
		}
		seenAddresses[address] = struct{}{}
		remoteID := strings.TrimSpace(remote.AnonymousID)
		if remoteID != "" {
			if _, exists := seenRemoteIDs[remoteID]; exists {
				return nil, 0, wrapError(CodeUpstreamError, ErrUpstream,
					fmt.Errorf("Apple directory contains duplicate remote ID"))
			}
			seenRemoteIDs[remoteID] = struct{}{}
		}
		if sameEmail(remote.ForwardToEmail, accountEmail) {
			belongs = true
			remote.HME = address
			filtered = append(filtered, remote)
			if !remote.IsActive {
				inactive++
			}
		}
	}
	if !belongs {
		return nil, 0, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	return filtered, inactive, nil
}

func normalizeRegion(region apple.Region) (apple.Region, error) {
	switch strings.ToLower(strings.TrimSpace(string(region))) {
	case "", RegionGlobal:
		return apple.RegionGlobal, nil
	case RegionChina, "china":
		return apple.RegionChina, nil
	default:
		return "", errors.New("unknown Apple service region")
	}
}

func normalizeSession(session *apple.Session, appleID string, region apple.Region, now time.Time) {
	if strings.TrimSpace(session.AppleID) == "" {
		session.AppleID = strings.TrimSpace(appleID)
	}
	if session.Region == "" {
		session.Region = region
	}
	if session.ValidatedAt.IsZero() {
		session.ValidatedAt = now.UTC()
	}
}

func publicRegion(region string) string {
	if strings.EqualFold(strings.TrimSpace(region), string(apple.RegionChina)) ||
		strings.EqualFold(strings.TrimSpace(region), "china") {
		return RegionChina
	}
	return RegionGlobal
}

func sameEmail(left, right string) bool {
	left = domain.NormalizeEmail(left)
	right = domain.NormalizeEmail(right)
	return left != "" && left == right
}

func identityOf(account domain.Account) accountIdentity {
	return accountIdentity{
		email:        domain.NormalizeEmail(account.Email),
		imapUsername: strings.TrimSpace(account.IMAPUsername),
	}
}

func sameIdentity(left, right accountIdentity) bool {
	return left.email != "" && left.email == right.email && left.imapUsername == right.imapUsername
}

func sessionInfoFromSession(session apple.Session, status string, authenticatedAt, expiresAt *time.Time) SessionInfo {
	return SessionInfo{
		Status:          status,
		AppleID:         strings.TrimSpace(session.AppleID),
		Region:          publicRegion(string(session.Region)),
		AuthenticatedAt: cloneTime(authenticatedAt),
		ExpiresAt:       cloneTime(expiresAt),
	}
}

func sessionInfoFromRecord(record domain.AppleWebSession, session apple.Session, status string) SessionInfo {
	authenticatedAt := record.CreatedAt
	var expiresAt *time.Time
	for _, cookie := range session.Cookies {
		if !strings.EqualFold(cookie.Name, "X-APPLE-WEBAUTH-TOKEN") || cookie.Expires.IsZero() {
			continue
		}
		expires := cookie.Expires.UTC()
		if expiresAt == nil || expires.Before(*expiresAt) {
			expiresAt = &expires
		}
	}
	return sessionInfoFromSession(session, status, &authenticatedAt, expiresAt)
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func (s *Service) newChallenge(ownerAdminID int64, account domain.Account, appleID string, region apple.Region, session apple.Session) (challenge, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return challenge{}, err
	}
	flow := challenge{
		id:           base64.RawURLEncoding.EncodeToString(bytes),
		ownerAdminID: ownerAdminID,
		accountID:    account.ID,
		identity:     identityOf(account),
		appleID:      appleID,
		region:       region,
		session:      session,
		expiresAt:    s.now().UTC().Add(s.challengeTTL),
	}
	s.challengeMu.Lock()
	defer s.challengeMu.Unlock()
	s.cleanupChallengesLocked()
	if existing := s.accountFlows[account.ID]; existing != "" {
		delete(s.challenges, existing)
	}
	s.challenges[flow.id] = flow
	s.accountFlows[account.ID] = flow.id
	return flow, nil
}

func (s *Service) readChallenge(id string, ownerAdminID, accountID int64) (challenge, bool) {
	s.challengeMu.Lock()
	defer s.challengeMu.Unlock()
	s.cleanupChallengesLocked()
	flow, ok := s.challenges[id]
	if !ok || flow.ownerAdminID != ownerAdminID || flow.accountID != accountID ||
		flow.attempts >= s.maxAttempts {
		return challenge{}, false
	}
	return flow, true
}

func (s *Service) recordFailedAttempt(flow challenge) {
	s.challengeMu.Lock()
	defer s.challengeMu.Unlock()
	current, ok := s.challenges[flow.id]
	if !ok || current.ownerAdminID != flow.ownerAdminID || current.accountID != flow.accountID {
		return
	}
	current.attempts++
	if current.attempts >= s.maxAttempts {
		delete(s.challenges, current.id)
		if s.accountFlows[current.accountID] == current.id {
			delete(s.accountFlows, current.accountID)
		}
		return
	}
	s.challenges[current.id] = current
}

func (s *Service) deleteChallenge(id string) {
	s.challengeMu.Lock()
	defer s.challengeMu.Unlock()
	flow, ok := s.challenges[id]
	if !ok {
		return
	}
	delete(s.challenges, id)
	if s.accountFlows[flow.accountID] == id {
		delete(s.accountFlows, flow.accountID)
	}
}

func (s *Service) deleteAccountChallenge(accountID int64) {
	s.challengeMu.Lock()
	defer s.challengeMu.Unlock()
	if id := s.accountFlows[accountID]; id != "" {
		delete(s.challenges, id)
		delete(s.accountFlows, accountID)
	}
}

func (s *Service) cleanupChallengesLocked() {
	now := s.now()
	for id, flow := range s.challenges {
		if now.Before(flow.expiresAt) {
			continue
		}
		delete(s.challenges, id)
		if s.accountFlows[flow.accountID] == id {
			delete(s.accountFlows, flow.accountID)
		}
	}
}

func (s *Service) acquireOperation(ctx context.Context, accountID int64) (func(), error) {
	s.operationMu.Lock()
	lock := s.operationLock[accountID]
	if lock == nil {
		lock = &operationLock{token: make(chan struct{}, 1)}
		s.operationLock[accountID] = lock
	}
	lock.refs++
	s.operationMu.Unlock()
	select {
	case lock.token <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-lock.token
				s.operationMu.Lock()
				lock.refs--
				if lock.refs == 0 && s.operationLock[accountID] == lock {
					delete(s.operationLock, accountID)
				}
				s.operationMu.Unlock()
			})
		}, nil
	case <-ctx.Done():
		s.operationMu.Lock()
		lock.refs--
		if lock.refs == 0 && s.operationLock[accountID] == lock {
			delete(s.operationLock, accountID)
		}
		s.operationMu.Unlock()
		return nil, ctx.Err()
	}
}

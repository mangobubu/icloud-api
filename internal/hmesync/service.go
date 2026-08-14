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
	autoCreatePersistTimeout    = 5 * time.Second
	autoCreateRecoveryTimeout   = 10 * time.Second
	aliasDeletePersistTimeout   = 5 * time.Second
)

var defaultAutoCreateConfirmationDelays = [...]time.Duration{
	500 * time.Millisecond,
	1500 * time.Millisecond,
	3 * time.Second,
}

const (
	autoCreatePreparingPercent              = 5
	autoCreateCheckingAccountPercent        = 10
	autoCreateCheckingCapacityPercent       = 15
	autoCreateLoadingSessionPercent         = 25
	autoCreateValidatingSessionPercent      = 35
	autoCreateCheckingForwardingPercent     = 45
	autoCreateInitializingForwardingPercent = 50
	autoCreatePreparingKeyPercent           = 55
	autoCreateReservingPercent              = 65
	autoCreateSavingCandidatePercent        = 75
	autoCreateConfirmingPercent             = 85
	autoCreateReconcilingPercent            = 85
	autoCreateSavingResultPercent           = 95
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

	challengeTTL                 time.Duration
	maxAttempts                  int
	autoCreateConfirmationDelays []time.Duration
	challengeMu                  sync.Mutex
	challenges                   map[string]challenge
	accountFlows                 map[int64]string
	operationMu                  sync.Mutex
	operationLock                map[int64]*operationLock
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
		repo:         repo,
		cipher:       cipher,
		client:       client,
		locker:       locker,
		now:          func() time.Time { return time.Now().UTC() },
		challengeTTL: defaultChallengeTTL,
		maxAttempts:  defaultVerificationAttempts,
		autoCreateConfirmationDelays: append(
			[]time.Duration(nil),
			defaultAutoCreateConfirmationDelays[:]...,
		),
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
	var issued []domain.AliasImportCredential
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
		if credentialRepo, ok := s.repo.(CredentialImportRepository); ok {
			imported, issued, err = credentialRepo.ImportAliasesWithCredentials(ctx, accountID, candidates)
		} else {
			imported, err = s.repo.ImportAliases(ctx, accountID, candidates)
		}
		if errors.Is(err, store.ErrAliasOwnershipConflict) {
			return wrapError(CodeAliasOwnershipConflict, ErrAliasOwnershipConflict, err)
		}
		return err
	})
	if err != nil {
		return SyncResult{}, err
	}

	created := make([]CreatedAlias, 0, len(imported.Created))
	issuedByAddress := make(map[string]string, len(issued))
	for _, credential := range issued {
		issuedByAddress[domain.NormalizeEmail(credential.Alias.Address)] = credential.APIKey
	}
	for _, alias := range imported.Created {
		apiKey := issuedByAddress[domain.NormalizeEmail(alias.Address)]
		if apiKey == "" {
			// Repositories that implement only the original ImportAliases
			// contract persisted this caller-issued key unchanged.
			apiKey = rawKeys[domain.NormalizeEmail(alias.Address)]
		}
		created = append(created, CreatedAlias{
			Alias:  alias,
			APIKey: apiKey,
		})
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

// DeleteAlias permanently removes an Apple Hide My Email address before
// deleting its local record. Ambiguous remote results are reconciled only by
// reading Apple's authoritative directory.
func (s *Service) DeleteAlias(ctx context.Context, aliasID int64) error {
	if aliasID < 1 {
		return errors.New("alias ID must be positive")
	}
	deleteRepo, ok := s.repo.(AliasDeletionRepository)
	if !ok {
		return errors.New("alias deletion persistence is unavailable")
	}
	deleteClient, ok := s.client.(AliasDeletionClient)
	if !ok {
		return errors.New("Apple alias deletion client is unavailable")
	}
	initial, err := deleteRepo.GetAlias(ctx, aliasID)
	if err != nil {
		return err
	}
	if initial.AccountID < 1 {
		return errors.New("alias account ID must be positive")
	}

	releaseOperation, err := s.acquireOperation(ctx, initial.AccountID)
	if err != nil {
		return err
	}
	defer releaseOperation()

	operation := func() error {
		return s.deleteAliasLocked(ctx, deleteRepo, deleteClient, initial.AccountID, aliasID)
	}
	if acquirer, ok := s.locker.(AccountLockAcquirer); ok {
		releaseAccount, err := acquirer.AcquireAccountLock(ctx, initial.AccountID)
		if err != nil {
			return err
		}
		defer releaseAccount()
		return operation()
	}
	return s.locker.WithAccountLock(ctx, initial.AccountID, operation)
}

func (s *Service) deleteAliasLocked(
	ctx context.Context,
	deleteRepo AliasDeletionRepository,
	deleteClient AliasDeletionClient,
	accountID, aliasID int64,
) error {
	alias, err := deleteRepo.GetAlias(ctx, aliasID)
	if err != nil {
		return err
	}
	if alias.AccountID != accountID {
		return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
	}
	if !alias.Enabled && strings.TrimSpace(alias.LastSyncError) == domain.AppleAliasConfirmationPending {
		return wrapError(CodeAliasConfirmationPending, ErrAliasConfirmationPending, store.ErrAliasConfirmationPending)
	}
	account, err := s.repo.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	record, session, err := s.loadSession(ctx, accountID)
	if err != nil {
		if errors.Is(err, ErrSessionExpired) {
			return s.expireAliasDeletionSession(ctx, accountID, err)
		}
		return err
	}

	validated, err := s.client.Validate(ctx, session)
	if err != nil {
		mapped := mapAppleError(err, false)
		if errors.Is(mapped, ErrSessionExpired) {
			return s.expireAliasDeletionSession(ctx, accountID, mapped)
		}
		return mapped
	}
	validated, err = s.prepareAliasDeletionSession(record, validated, session)
	if err != nil {
		return err
	}

	directory, listedSession, err := s.client.ListAliases(ctx, validated)
	if err != nil {
		mapped := mapAppleError(err, false)
		if errors.Is(mapped, ErrSessionExpired) {
			return s.expireAliasDeletionSession(ctx, accountID, mapped)
		}
		listedSession, sessionErr := s.prepareAliasDeletionSession(record, listedSession, validated)
		if sessionErr != nil {
			return errors.Join(sessionErr, mapped)
		}
		if sessionErr = s.checkpointAliasDeletionSession(ctx, accountID, listedSession); sessionErr != nil {
			return errors.Join(mapped, sessionErr)
		}
		return mapped
	}
	listedSession, err = s.prepareAliasDeletionSession(record, listedSession, validated)
	if err != nil {
		return err
	}
	if err := s.checkpointAliasDeletionSession(ctx, accountID, listedSession); err != nil {
		return err
	}
	filtered, _, err := filterAliases(directory, account.Email)
	if err != nil {
		return err
	}
	remote, found, err := findAppleAliasForDeletion(directory.Aliases, filtered, alias.Address)
	if err != nil {
		return err
	}
	if !found {
		return s.deleteLocalAliasAfterApple(ctx, deleteRepo, aliasID)
	}
	remoteID := strings.TrimSpace(remote.AnonymousID)
	if remoteID == "" {
		return wrapError(CodeUpstreamError, ErrUpstream, errors.New("Apple alias omitted its remote ID"))
	}

	currentSession := listedSession
	if remote.IsActive {
		returned, remoteErr := deleteClient.DeactivateAlias(ctx, currentSession, remoteID)
		if remoteErr != nil {
			mapped := mapAppleError(remoteErr, false)
			if errors.Is(mapped, ErrSessionExpired) {
				return s.expireAliasDeletionSession(ctx, accountID, mapped)
			}
			returned, sessionErr := s.prepareAliasDeletionSession(record, returned, currentSession)
			if sessionErr != nil {
				return errors.Join(sessionErr, mapped)
			}
			if sessionErr = s.checkpointAliasDeletionSession(ctx, accountID, returned); sessionErr != nil {
				return errors.Join(mapped, sessionErr)
			}
			reconciled, stillPresent, reconciledSession, reconcileErr := s.reconcileAliasDeletion(
				ctx, account, record, returned, alias.Address,
			)
			if reconcileErr != nil {
				return errors.Join(reconcileErr, mapped)
			}
			if !stillPresent {
				return s.deleteLocalAliasAfterApple(ctx, deleteRepo, aliasID)
			}
			if reconciled.IsActive {
				return mapped
			}
			remoteID = strings.TrimSpace(reconciled.AnonymousID)
			if remoteID == "" {
				return errors.Join(mapped, wrapError(CodeUpstreamError, ErrUpstream,
					errors.New("Apple alias omitted its remote ID")))
			}
			currentSession = reconciledSession
		} else {
			returned, err = s.prepareAliasDeletionSession(record, returned, currentSession)
			if err != nil {
				return err
			}
			if err := s.checkpointAliasDeletionSession(ctx, accountID, returned); err != nil {
				return err
			}
			currentSession = returned
		}
	}

	returned, remoteErr := deleteClient.DeleteAlias(ctx, currentSession, remoteID)
	if remoteErr != nil {
		mapped := mapAppleError(remoteErr, false)
		if errors.Is(mapped, ErrSessionExpired) {
			return s.expireAliasDeletionSession(ctx, accountID, mapped)
		}
		returned, sessionErr := s.prepareAliasDeletionSession(record, returned, currentSession)
		if sessionErr != nil {
			return errors.Join(sessionErr, mapped)
		}
		if sessionErr = s.checkpointAliasDeletionSession(ctx, accountID, returned); sessionErr != nil {
			return errors.Join(mapped, sessionErr)
		}
		_, stillPresent, _, reconcileErr := s.reconcileAliasDeletion(ctx, account, record, returned, alias.Address)
		if reconcileErr != nil {
			return errors.Join(reconcileErr, mapped)
		}
		if stillPresent {
			return mapped
		}
		return s.deleteLocalAliasAfterApple(ctx, deleteRepo, aliasID)
	}
	returned, err = s.prepareAliasDeletionSession(record, returned, currentSession)
	if err != nil {
		return err
	}
	if err := s.checkpointAliasDeletionSession(ctx, accountID, returned); err != nil {
		return err
	}
	return s.deleteLocalAliasAfterApple(ctx, deleteRepo, aliasID)
}

func (s *Service) prepareAliasDeletionSession(
	record domain.AppleWebSession,
	returned, fallback apple.Session,
) (apple.Session, error) {
	if !hasAppleSessionState(returned) {
		returned = fallback
	}
	if strings.TrimSpace(returned.AppleID) != "" &&
		!strings.EqualFold(strings.TrimSpace(returned.AppleID), strings.TrimSpace(record.AppleID)) {
		return apple.Session{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	normalizeSession(&returned, record.AppleID, fallback.Region, s.now())
	region, err := normalizeRegion(returned.Region)
	if err != nil {
		return apple.Session{}, err
	}
	returned.Region = region
	return returned, nil
}

func (s *Service) checkpointAliasDeletionSession(ctx context.Context, accountID int64, session apple.Session) error {
	persistContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), aliasDeletePersistTimeout)
	defer cancel()
	_, err := s.saveSession(persistContext, accountID, session)
	return err
}

func (s *Service) expireAliasDeletionSession(ctx context.Context, accountID int64, sessionErr error) error {
	persistContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), aliasDeletePersistTimeout)
	defer cancel()
	err := s.repo.DeleteAppleWebSession(persistContext, accountID)
	if errors.Is(err, store.ErrNotFound) {
		err = nil
	}
	if err == nil {
		return sessionErr
	}
	return wrapError(CodeSessionExpired, ErrSessionExpired, errors.Join(
		sessionErr,
		fmt.Errorf("delete expired Apple session: %w", err),
	))
}

func (s *Service) reconcileAliasDeletion(
	ctx context.Context,
	account domain.Account,
	record domain.AppleWebSession,
	session apple.Session,
	address string,
) (apple.Alias, bool, apple.Session, error) {
	reconcileContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), aliasDeletePersistTimeout)
	defer cancel()
	directory, returned, err := s.client.ListAliases(reconcileContext, session)
	if err != nil {
		mapped := mapAppleError(err, false)
		if errors.Is(mapped, ErrSessionExpired) {
			return apple.Alias{}, false, session, s.expireAliasDeletionSession(reconcileContext, account.ID, mapped)
		}
		returned, sessionErr := s.prepareAliasDeletionSession(record, returned, session)
		if sessionErr != nil {
			return apple.Alias{}, false, session, errors.Join(mapped, sessionErr)
		}
		if _, sessionErr = s.saveSession(reconcileContext, account.ID, returned); sessionErr != nil {
			return apple.Alias{}, false, returned, errors.Join(mapped, sessionErr)
		}
		return apple.Alias{}, false, returned, mapped
	}
	returned, err = s.prepareAliasDeletionSession(record, returned, session)
	if err != nil {
		return apple.Alias{}, false, session, err
	}
	if _, err := s.saveSession(reconcileContext, account.ID, returned); err != nil {
		return apple.Alias{}, false, returned, err
	}
	filtered, _, err := filterAliases(directory, account.Email)
	if err != nil {
		return apple.Alias{}, false, returned, err
	}
	remote, found, err := findAppleAliasForDeletion(directory.Aliases, filtered, address)
	return remote, found, returned, err
}

func (s *Service) deleteLocalAliasAfterApple(ctx context.Context, repo AliasDeletionRepository, aliasID int64) error {
	persistContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), aliasDeletePersistTimeout)
	defer cancel()
	return repo.DeleteAlias(persistContext, aliasID)
}

func findAppleAliasForDeletion(all, owned []apple.Alias, address string) (apple.Alias, bool, error) {
	remote, found := findAppleAlias(owned, address)
	if found {
		return remote, true, nil
	}
	if _, exists := findAppleAlias(all, address); exists {
		return apple.Alias{}, false, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	return apple.Alias{}, false, nil
}

// CreateAutoAlias reserves exactly one Hide My Email address and publishes it
// locally with a complete persistent credential bundle. The method never
// retries reserve because repeating that remote side effect could create
// duplicates; only the read-only directory confirmation is retried.
func (s *Service) CreateAutoAlias(ctx context.Context, accountID int64) (createdAlias domain.Alias, resultErr error) {
	currentPercent := autoCreatePreparingPercent
	pendingConfirmationTracked := false
	reportProgress := func(phase domain.AliasCreationPhase, percent, attempt int) {
		if percent < 0 {
			percent = 0
		} else if percent > 100 {
			percent = 100
		}
		currentPercent = percent
		domain.ReportAliasCreationProgress(ctx, phase, percent, attempt)
	}
	reportProgress(domain.AliasCreationPhasePreparing, currentPercent, 0)
	defer func() {
		if resultErr != nil && pendingConfirmationTracked {
			resultErr = markPendingConfirmation(resultErr)
		}
		switch {
		case resultErr == nil:
			reportProgress(domain.AliasCreationPhaseCompleted, 100, 0)
		case ctx.Err() != nil && contextOnlyError(resultErr):
			reportProgress(domain.AliasCreationPhaseCancelled, currentPercent, 0)
		default:
			reportProgress(domain.AliasCreationPhaseFailed, currentPercent, 0)
		}
	}()

	if accountID < 1 {
		return domain.Alias{}, errors.New("account ID must be positive")
	}
	// Accept both the original repository contract and the short-lived richer
	// adapter shape used by early v2 integrations. The former preserves pending
	// key delivery; the latter remains usable for embedders that have not yet
	// added that optional field to their persistence layer.
	var countEnabled func(context.Context, int64) (int, error)
	var getPending func(context.Context, int64) (domain.PendingAliasAPIKey, error)
	var createAlias func(context.Context, domain.AppleWebSession, domain.Alias, string) (domain.Alias, domain.AppleWebSession, error)
	var confirmPending func(context.Context, domain.AppleWebSession, int64) (domain.Alias, domain.AppleWebSession, error)
	if autoRepo, ok := s.repo.(AutoCreateRepository); ok {
		countEnabled = autoRepo.CountEnabledAliasesByAccount
		getPending = autoRepo.GetPendingAutoAliasConfirmation
		createAlias = autoRepo.CreateAliasWithPendingAPIKey
		confirmPending = autoRepo.ConfirmPendingAutoAlias
	} else if modernRepo, ok := s.repo.(ModernAutoCreateRepository); ok {
		countEnabled = modernRepo.CountEnabledAliasesByAccount
		getPending = func(ctx context.Context, accountID int64) (domain.PendingAliasAPIKey, error) {
			alias, err := modernRepo.GetPendingAutoAliasConfirmation(ctx, accountID)
			if err != nil {
				return domain.PendingAliasAPIKey{}, err
			}
			return domain.PendingAliasAPIKey{Alias: alias}, nil
		}
		createAlias = func(ctx context.Context, session domain.AppleWebSession, alias domain.Alias, _ string) (domain.Alias, domain.AppleWebSession, error) {
			return modernRepo.CreateAutoAliasCandidate(ctx, session, alias)
		}
		confirmPending = modernRepo.ConfirmPendingAutoAlias
	} else {
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
		cleanupContext, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), autoCreatePersistTimeout)
		defer cancelCleanup()
		var deleteErr error
		if releaseAccount != nil {
			// The production locker is deliberately non-reentrant. The account
			// lock already protects this deletion across the remote operation.
			deleteErr = s.repo.DeleteAppleWebSession(cleanupContext, accountID)
		} else {
			deleteErr = s.locker.WithAccountLock(cleanupContext, accountID, func() error {
				err := s.repo.DeleteAppleWebSession(cleanupContext, accountID)
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

	reportProgress(domain.AliasCreationPhaseCheckingAccount, autoCreateCheckingAccountPercent, 0)
	account, err := s.repo.GetAccount(ctx, accountID)
	if err != nil {
		return domain.Alias{}, err
	}
	if !account.Enabled {
		return domain.Alias{}, wrapError(CodeAccountDisabled, ErrAccountDisabled, nil)
	}
	reportProgress(domain.AliasCreationPhaseCheckingCapacity, autoCreateCheckingCapacityPercent, 0)
	pendingConfirmation, pendingErr := getPending(ctx, accountID)
	hasPendingConfirmation := pendingErr == nil
	pendingConfirmationTracked = hasPendingConfirmation
	if pendingErr != nil && !errors.Is(pendingErr, store.ErrNotFound) {
		return domain.Alias{}, pendingErr
	}
	if !hasPendingConfirmation {
		count, err := countEnabled(ctx, accountID)
		if err != nil {
			return domain.Alias{}, err
		}
		if count >= domain.MaxEnabledAliasesPerAccount {
			return domain.Alias{}, store.ErrAliasLimit
		}
	}

	reportProgress(domain.AliasCreationPhaseLoadingSession, autoCreateLoadingSessionPercent, 0)
	record, session, err := s.loadSession(ctx, accountID)
	if err != nil {
		if errors.Is(err, ErrSessionExpired) {
			err = expireAutoSession(err)
		}
		return domain.Alias{}, err
	}
	trustedDSID := strings.TrimSpace(session.DSID)
	if trustedDSID == "" {
		return domain.Alias{}, expireAutoSession(wrapError(CodeSessionExpired, ErrSessionExpired,
			errors.New("stored Apple session omitted the account identifier")))
	}
	reportProgress(domain.AliasCreationPhaseValidatingSession, autoCreateValidatingSessionPercent, 0)
	validated, err := s.client.Validate(ctx, session)
	if err != nil {
		mapped := mapAppleError(err, false)
		if errors.Is(mapped, ErrSessionExpired) {
			mapped = expireAutoSession(mapped)
		}
		if hasPendingConfirmation &&
			(errors.Is(mapped, ErrUpstream) || errors.Is(mapped, ErrRateLimited)) {
			return domain.Alias{}, wrapError(CodeAliasConfirmationPending, ErrAliasConfirmationPending, mapped)
		}
		return domain.Alias{}, mapped
	}
	if strings.TrimSpace(validated.AppleID) != "" &&
		!strings.EqualFold(strings.TrimSpace(validated.AppleID), strings.TrimSpace(record.AppleID)) {
		return domain.Alias{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	if identityErr := validateSessionDSID(trustedDSID, validated); identityErr != nil {
		if errors.Is(identityErr, ErrSessionExpired) {
			identityErr = expireAutoSession(identityErr)
		}
		return domain.Alias{}, identityErr
	}
	normalizeSession(&validated, record.AppleID, session.Region, s.now())

	// Apple reserve uses the account-wide selectedForwardTo setting and does
	// not accept a per-request forwarding address. Confirm that target before
	// the irreversible reserve call. The reserve response may omit
	// forwardToEmail, so checking only after creation both misclassifies a valid
	// response and can leave an untracked remote alias.
	if hasPendingConfirmation {
		// With a staged alias, this directory read is reconciliation rather
		// than a forwarding preflight. Report the real operation before the
		// network call so failures retain the correct stage.
		reportProgress(domain.AliasCreationPhaseReconciling, autoCreateReconcilingPercent, 1)
	} else {
		reportProgress(domain.AliasCreationPhaseCheckingForwarding, autoCreateCheckingForwardingPercent, 0)
	}
	settings, listedSession, err := s.client.ListAliases(ctx, validated)
	if err != nil {
		mapped := mapAppleError(err, false)
		if errors.Is(mapped, ErrSessionExpired) {
			mapped = expireAutoSession(mapped)
		}
		if hasPendingConfirmation &&
			(errors.Is(mapped, ErrUpstream) || errors.Is(mapped, ErrRateLimited)) {
			return domain.Alias{}, wrapError(CodeAliasConfirmationPending, ErrAliasConfirmationPending, mapped)
		}
		return domain.Alias{}, mapped
	}
	if strings.TrimSpace(listedSession.AppleID) != "" &&
		!strings.EqualFold(strings.TrimSpace(listedSession.AppleID), strings.TrimSpace(record.AppleID)) {
		return domain.Alias{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	if identityErr := validateSessionDSID(trustedDSID, listedSession); identityErr != nil {
		if errors.Is(identityErr, ErrSessionExpired) {
			identityErr = expireAutoSession(identityErr)
		}
		return domain.Alias{}, identityErr
	}
	normalizeSession(&listedSession, record.AppleID, validated.Region, s.now())
	listedRegion, err := normalizeRegion(listedSession.Region)
	if err != nil {
		return domain.Alias{}, err
	}
	listedSession.Region = listedRegion
	checkpointSession := func(operationContext context.Context, session apple.Session) error {
		if releaseAccount != nil {
			_, err := s.saveSession(operationContext, accountID, session)
			return err
		}
		_, err := s.persistSession(operationContext, accountID, identityOf(account), session)
		return err
	}
	checkpointReturnedSession := func(operationContext context.Context, returned *apple.Session, fallbackRegion apple.Region) error {
		if strings.TrimSpace(returned.AppleID) != "" &&
			!strings.EqualFold(strings.TrimSpace(returned.AppleID), strings.TrimSpace(record.AppleID)) {
			return wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
		}
		if identityErr := validateSessionDSID(trustedDSID, *returned); identityErr != nil {
			return identityErr
		}
		normalizeSession(returned, record.AppleID, fallbackRegion, s.now())
		region, err := normalizeRegion(returned.Region)
		if err != nil {
			return err
		}
		returned.Region = region
		return checkpointSession(operationContext, *returned)
	}
	publishAlias := func(
		operationContext context.Context,
		sessionRecord domain.AppleWebSession,
		alias domain.Alias,
		apiKeyCiphertext string,
	) (domain.Alias, error) {
		var saved domain.Alias
		publish := func() error {
			current, err := s.repo.GetAccount(operationContext, accountID)
			if err != nil {
				return err
			}
			if !current.Enabled {
				return wrapError(CodeAccountDisabled, ErrAccountDisabled, nil)
			}
			if !sameIdentity(identityOf(current), identityOf(account)) {
				return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
			}
			var saveErr error
			saved, _, saveErr = createAlias(
				operationContext, sessionRecord, alias, apiKeyCiphertext,
			)
			return saveErr
		}
		var publishErr error
		if releaseAccount != nil {
			publishErr = publish()
		} else {
			publishErr = s.locker.WithAccountLock(operationContext, accountID, publish)
		}
		if errors.Is(publishErr, store.ErrAliasOwnershipConflict) {
			return domain.Alias{}, wrapError(CodeAliasOwnershipConflict, ErrAliasOwnershipConflict, publishErr)
		}
		return saved, publishErr
	}
	confirmPendingAlias := func(
		operationContext context.Context,
		pending domain.Alias,
		confirmed apple.Alias,
		returned apple.Session,
		attempt int,
	) (domain.Alias, error) {
		reportProgress(domain.AliasCreationPhaseConfirming, autoCreateConfirmingPercent, attempt)
		address, err := normalizeAutoAliasAddress(confirmed.HME)
		if err != nil {
			return domain.Alias{}, wrapError(CodeAliasConfirmationPending, ErrAliasConfirmationPending, err)
		}
		if address != domain.NormalizeEmail(pending.Address) {
			return domain.Alias{}, wrapError(CodeAliasConfirmationPending, ErrAliasConfirmationPending,
				errors.New("Apple returned a different pending alias address"))
		}
		if !confirmed.IsActive {
			return domain.Alias{}, wrapError(CodeAliasConfirmationPending, ErrAliasConfirmationPending,
				errors.New("Apple returned an inactive pending alias"))
		}
		if !sameEmail(confirmed.ForwardToEmail, account.Email) {
			return domain.Alias{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
		}
		sessionRecord, err := s.autoCreateSessionRecord(accountID, record.AppleID, trustedDSID, validated.Region, returned)
		if err != nil {
			if Code(err) != "" {
				return domain.Alias{}, err
			}
			return domain.Alias{}, wrapPersistenceError(err)
		}
		reportProgress(domain.AliasCreationPhaseSavingResult, autoCreateSavingResultPercent, attempt)
		var saved domain.Alias
		confirm := func() error {
			current, err := s.repo.GetAccount(operationContext, accountID)
			if err != nil {
				return err
			}
			if !current.Enabled {
				return wrapError(CodeAccountDisabled, ErrAccountDisabled, nil)
			}
			if !sameIdentity(identityOf(current), identityOf(account)) {
				return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
			}
			var confirmErr error
			saved, _, confirmErr = confirmPending(operationContext, sessionRecord, pending.ID)
			return confirmErr
		}
		var confirmErr error
		if releaseAccount != nil {
			confirmErr = confirm()
		} else {
			confirmErr = s.locker.WithAccountLock(operationContext, accountID, confirm)
		}
		if confirmErr != nil {
			confirmErr = wrapPersistenceError(confirmErr)
		}
		return saved, confirmErr
	}
	// ListAliases may rotate cookies even when the forwarding preflight rejects
	// the operation. Preserve that valid checkpoint so a corrected setting does
	// not force an otherwise unnecessary Apple login.
	if err := checkpointSession(ctx, listedSession); err != nil {
		return domain.Alias{}, wrapPersistenceError(err)
	}
	if hasPendingConfirmation {
		confirmed, found := findAppleAlias(settings.Aliases, pendingConfirmation.Alias.Address)
		if !found {
			return domain.Alias{}, wrapError(
				CodeAliasConfirmationPending,
				ErrAliasConfirmationPending,
				errors.New("Apple list omitted the pending reserved alias"),
			)
		}
		return confirmPendingAlias(ctx, pendingConfirmation.Alias, confirmed, listedSession, 1)
	}
	forwardingErr := validateAutoCreateForwardingTarget(settings, account.Email)
	if errors.Is(forwardingErr, ErrForwardingTargetMissing) {
		updater, canUpdate := s.client.(ForwardingTargetUpdater)
		if !canUpdate || len(settings.Aliases) != 0 ||
			!containsForwardingCandidate(settings.ForwardToEmails, account.Email) {
			return domain.Alias{}, forwardingErr
		}

		if err := ctx.Err(); err != nil {
			return domain.Alias{}, err
		}
		reportProgress(domain.AliasCreationPhaseInitializingForwarding, autoCreateInitializingForwardingPercent, 0)
		updatedForwardingSession, updateErr := updater.UpdateForwardTo(ctx, listedSession, account.Email)
		if !hasAppleSessionState(updatedForwardingSession) {
			updatedForwardingSession = listedSession
		}
		var mappedUpdateErr error
		if updateErr != nil {
			mappedUpdateErr = mapAppleError(updateErr, false)
			if errors.Is(mappedUpdateErr, ErrSessionExpired) {
				return domain.Alias{}, expireAutoSession(mappedUpdateErr)
			}
		}
		checkpointContext, cancelCheckpoint := context.WithTimeout(context.WithoutCancel(ctx), autoCreatePersistTimeout)
		checkpointErr := checkpointReturnedSession(checkpointContext, &updatedForwardingSession, listedSession.Region)
		cancelCheckpoint()
		if checkpointErr != nil {
			checkpointErr = wrapPersistenceError(checkpointErr)
			if mappedUpdateErr != nil {
				return domain.Alias{}, errors.Join(checkpointErr, mappedUpdateErr)
			}
			return domain.Alias{}, checkpointErr
		}
		if mappedUpdateErr != nil && !errors.Is(mappedUpdateErr, ErrUpstream) &&
			!errors.Is(mappedUpdateErr, context.Canceled) &&
			!errors.Is(mappedUpdateErr, context.DeadlineExceeded) {
			return domain.Alias{}, mappedUpdateErr
		}

		verifyContext, cancelVerify := context.WithTimeout(context.WithoutCancel(ctx), autoCreateRecoveryTimeout)
		verifiedSettings, verifiedSession, verifyErr := s.client.ListAliases(verifyContext, updatedForwardingSession)
		cancelVerify()
		if !hasAppleSessionState(verifiedSession) {
			verifiedSession = updatedForwardingSession
		}
		var mappedVerifyErr error
		if verifyErr != nil {
			mappedVerifyErr = mapAppleError(verifyErr, false)
			if errors.Is(mappedVerifyErr, ErrSessionExpired) {
				mappedVerifyErr = expireAutoSession(mappedVerifyErr)
				if mappedUpdateErr != nil {
					return domain.Alias{}, errors.Join(mappedVerifyErr, mappedUpdateErr)
				}
				return domain.Alias{}, mappedVerifyErr
			}
		}
		verifyCheckpointContext, cancelVerifyCheckpoint := context.WithTimeout(context.WithoutCancel(ctx), autoCreatePersistTimeout)
		checkpointErr = checkpointReturnedSession(verifyCheckpointContext, &verifiedSession, updatedForwardingSession.Region)
		cancelVerifyCheckpoint()
		if checkpointErr != nil {
			checkpointErr = wrapPersistenceError(checkpointErr)
			if mappedVerifyErr != nil {
				checkpointErr = errors.Join(checkpointErr, mappedVerifyErr)
			}
			if mappedUpdateErr != nil {
				return domain.Alias{}, errors.Join(checkpointErr, mappedUpdateErr)
			}
			return domain.Alias{}, checkpointErr
		}
		if mappedVerifyErr != nil {
			if callerErr := ctx.Err(); callerErr != nil {
				return domain.Alias{}, callerErr
			}
			if mappedUpdateErr != nil {
				return domain.Alias{}, errors.Join(mappedVerifyErr, mappedUpdateErr)
			}
			return domain.Alias{}, mappedVerifyErr
		}
		settings = verifiedSettings
		listedSession = verifiedSession
		forwardingErr = validateAutoCreateForwardingTarget(settings, account.Email)
		if callerErr := ctx.Err(); callerErr != nil {
			return domain.Alias{}, callerErr
		}
		if forwardingErr != nil && mappedUpdateErr != nil {
			return domain.Alias{}, errors.Join(forwardingErr, mappedUpdateErr)
		}
	}
	if forwardingErr != nil {
		return domain.Alias{}, forwardingErr
	}

	// Prepare the one-time API key before reserve. The store later reuses this
	// exact key for the v2 bundle, so the legacy delivery queue and every v2
	// authenticator agree on one credential.
	reportProgress(domain.AliasCreationPhasePreparingKey, autoCreatePreparingKeyPercent, 0)
	rawKey, hash, prefix, err := secure.NewAPIKey()
	if err != nil {
		return domain.Alias{}, wrapCryptoError(fmt.Errorf("generate automatic alias API key: %w", err))
	}
	apiKeyCiphertext, err := keyCipher.EncryptPendingAliasAPIKey(rawKey)
	rawKey = ""
	if err != nil {
		return domain.Alias{}, wrapCryptoError(fmt.Errorf("encrypt automatic alias API key: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return domain.Alias{}, err
	}

	reportProgress(domain.AliasCreationPhaseReserving, autoCreateReservingPercent, 0)
	created, updated, createErr := autoClient.CreateAlias(ctx, listedSession, autoCreateLabel, autoCreateNote)
	var mappedCreateErr error
	if createErr != nil {
		mappedCreateErr = mapAppleError(createErr, false)
	}

	// Once reserve may have reached Apple, a valid generated HME is the durable
	// idempotency marker. Persist it before any session checkpoint, response
	// validation, or use of the caller's potentially canceled context.
	if strings.TrimSpace(created.HME) == "" {
		if mappedCreateErr != nil {
			if !errors.Is(mappedCreateErr, context.Canceled) &&
				!errors.Is(mappedCreateErr, context.DeadlineExceeded) &&
				hasAppleSessionState(updated) {
				if checkpointErr := checkpointReturnedSession(ctx, &updated, listedSession.Region); checkpointErr != nil {
					return domain.Alias{}, errors.Join(wrapPersistenceError(checkpointErr), mappedCreateErr)
				}
			}
			if errors.Is(mappedCreateErr, ErrSessionExpired) {
				mappedCreateErr = expireAutoSession(mappedCreateErr)
			}
			return domain.Alias{}, mappedCreateErr
		}
		return domain.Alias{}, wrapError(CodeUpstreamError, ErrUpstream,
			errors.New("Apple reserve response omitted the generated alias address"))
	}

	address, addressErr := normalizeAutoAliasAddress(created.HME)
	if addressErr != nil {
		addressErr = markRemoteSideEffectPossible(addressErr)
		if mappedCreateErr != nil {
			return domain.Alias{}, errors.Join(mappedCreateErr, addressErr)
		}
		return domain.Alias{}, addressErr
	}
	label := strings.TrimSpace(created.Label)
	if label == "" {
		label = autoCreateLabel
	}
	sessionForConfirmation := updated
	if !hasAppleSessionState(sessionForConfirmation) {
		sessionForConfirmation = listedSession
	}
	sessionRecord, sessionRecordErr := s.autoCreateSessionRecord(
		accountID,
		record.AppleID,
		trustedDSID,
		listedSession.Region,
		sessionForConfirmation,
	)
	if sessionRecordErr != nil {
		// Reserve already returned an address, so keep the last valid session and
		// publish the candidate before returning any account or crypto diagnostic.
		sessionRecord = record
	}
	reportProgress(domain.AliasCreationPhaseSavingCandidate, autoCreateSavingCandidatePercent, 0)
	persistContext, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), autoCreatePersistTimeout)
	provisional, persistErr := publishAlias(persistContext, sessionRecord, domain.Alias{
		AccountID:      accountID,
		AccountEmail:   account.Email,
		Address:        address,
		Label:          label,
		APIKeyHash:     hash,
		APIKeyPrefix:   prefix,
		Enabled:        false,
		LastSyncStatus: domain.SyncStatusPending,
		LastSyncError:  domain.AppleAliasConfirmationPending,
	}, apiKeyCiphertext)
	cancelPersist()
	if persistErr != nil {
		persistErr = markRemoteSideEffectPossible(wrapPersistenceError(persistErr))
		if sessionRecordErr != nil {
			persistErr = errors.Join(persistErr, wrapPersistenceError(sessionRecordErr))
		}
		if mappedCreateErr != nil {
			return domain.Alias{}, errors.Join(persistErr, mappedCreateErr)
		}
		return domain.Alias{}, persistErr
	}
	pendingConfirmationTracked = true
	if sessionRecordErr != nil {
		sessionPersistenceErr := sessionRecordErr
		if Code(sessionPersistenceErr) == "" {
			sessionPersistenceErr = wrapPersistenceError(sessionPersistenceErr)
		}
		if mappedCreateErr != nil &&
			!errors.Is(mappedCreateErr, context.Canceled) &&
			!errors.Is(mappedCreateErr, context.DeadlineExceeded) {
			return domain.Alias{}, errors.Join(sessionPersistenceErr, mappedCreateErr)
		}
		return domain.Alias{}, sessionPersistenceErr
	}
	if mappedCreateErr != nil && (errors.Is(mappedCreateErr, context.Canceled) ||
		errors.Is(mappedCreateErr, context.DeadlineExceeded)) {
		return domain.Alias{}, mappedCreateErr
	}
	if mappedCreateErr != nil && errors.Is(mappedCreateErr, ErrSessionExpired) {
		return domain.Alias{}, expireAutoSession(mappedCreateErr)
	}

	// A complete, internally consistent reserve response can publish the staged
	// alias immediately. Use a detached short context because the remote side
	// effect and its durable marker already exist even if the request timed out.
	if mappedCreateErr == nil && strings.TrimSpace(created.ForwardToEmail) != "" {
		if !created.IsActive {
			reportProgress(domain.AliasCreationPhaseConfirming, autoCreateConfirmingPercent, 1)
			return domain.Alias{}, wrapError(
				CodeAliasConfirmationPending,
				ErrAliasConfirmationPending,
				errors.New("Apple returned an inactive reserved alias"),
			)
		}
		if !sameEmail(created.ForwardToEmail, account.Email) {
			reportProgress(domain.AliasCreationPhaseConfirming, autoCreateConfirmingPercent, 1)
			return domain.Alias{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
		}
		confirmContext, cancelConfirm := context.WithTimeout(context.WithoutCancel(ctx), autoCreatePersistTimeout)
		saved, confirmErr := confirmPendingAlias(confirmContext, provisional, created, sessionForConfirmation, 1)
		cancelConfirm()
		return saved, confirmErr
	}

	// Minimal or ambiguous reserve results are reconciled only through bounded,
	// read-only directory requests. The pending row prevents every later plan
	// from issuing another reserve while Apple directory visibility catches up.
	confirmationSession := sessionForConfirmation
	confirmationCause := mappedCreateErr
	for attemptIndex := 0; ; attemptIndex++ {
		attempt := attemptIndex + 1
		reportProgress(domain.AliasCreationPhaseReconciling, autoCreateReconcilingPercent, attempt)
		confirmed, returnedSession, listErr := s.client.ListAliases(ctx, confirmationSession)
		var confirmationErr error
		requiresAccountAction := false
		if listErr != nil {
			confirmationErr = mapAppleError(listErr, false)
			if errors.Is(confirmationErr, ErrSessionExpired) {
				expiredErr := expireAutoSession(confirmationErr)
				if confirmationCause != nil {
					return domain.Alias{}, errors.Join(expiredErr, confirmationCause)
				}
				return domain.Alias{}, expiredErr
			}
			if errors.Is(confirmationErr, context.Canceled) ||
				errors.Is(confirmationErr, context.DeadlineExceeded) {
				return domain.Alias{}, confirmationErr
			}
			// These responses require an account/session action and will not be
			// fixed by waiting for directory propagation. Preserve their stable
			// Apple diagnostic code instead of relabeling them as pending.
			if errors.Is(confirmationErr, ErrAccountActionRequired) ||
				errors.Is(confirmationErr, ErrAccountMismatch) ||
				errors.Is(confirmationErr, ErrCredentialsInvalid) ||
				errors.Is(confirmationErr, ErrLoginRequired) {
				requiresAccountAction = true
			}
		}
		if hasAppleSessionState(returnedSession) {
			if err := checkpointReturnedSession(ctx, &returnedSession, confirmationSession.Region); err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					return domain.Alias{}, errors.Join(
						wrapPersistenceError(err),
						confirmationErr,
						confirmationCause,
					)
				}
				return domain.Alias{}, err
			}
			confirmationSession = returnedSession
		}
		if requiresAccountAction {
			return domain.Alias{}, errors.Join(confirmationErr, confirmationCause)
		}

		if listErr == nil {
			confirmedAlias, found := findAppleAlias(confirmed.Aliases, address)
			if found {
				saved, confirmErr := confirmPendingAlias(ctx, provisional, confirmedAlias, confirmationSession, attempt)
				if confirmErr != nil && confirmationCause != nil &&
					!errors.Is(confirmErr, context.Canceled) &&
					!errors.Is(confirmErr, context.DeadlineExceeded) {
					return domain.Alias{}, errors.Join(confirmErr, confirmationCause)
				}
				return saved, confirmErr
			}
			confirmationErr = wrapError(CodeUpstreamError, ErrUpstream,
				errors.New("Apple list omitted the newly reserved alias"))
		}

		// Keep the latest read-only confirmation failure first so errors.As
		// exposes its HTTP operation/status; the reserve cause remains useful
		// context for diagnosing the original ambiguous side effect.
		combinedErr := errors.Join(confirmationErr, confirmationCause)
		if !shouldRetryAutoCreateConfirmation(confirmationErr) ||
			attemptIndex >= len(s.autoCreateConfirmationDelays) {
			return domain.Alias{}, wrapError(
				CodeAliasConfirmationPending,
				ErrAliasConfirmationPending,
				combinedErr,
			)
		}
		if err := waitForAutoCreateConfirmation(ctx, s.autoCreateConfirmationDelays[attemptIndex]); err != nil {
			return domain.Alias{}, err
		}
	}
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

func normalizeAutoAliasAddress(value string) (string, error) {
	address := domain.NormalizeEmail(value)
	parsed, err := mail.ParseAddress(address)
	if address == "" || err != nil || parsed.Name != "" || domain.NormalizeEmail(parsed.Address) != address {
		return "", wrapError(CodeUpstreamError, ErrUpstream,
			errors.New("Apple returned an invalid alias address"))
	}
	return address, nil
}

func (s *Service) autoCreateSessionRecord(
	accountID int64,
	expectedAppleID string,
	expectedDSID string,
	fallbackRegion apple.Region,
	session apple.Session,
) (domain.AppleWebSession, error) {
	if strings.TrimSpace(session.AppleID) != "" &&
		!strings.EqualFold(strings.TrimSpace(session.AppleID), strings.TrimSpace(expectedAppleID)) {
		return domain.AppleWebSession{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	if err := validateSessionDSID(expectedDSID, session); err != nil {
		return domain.AppleWebSession{}, err
	}
	normalizeSession(&session, expectedAppleID, fallbackRegion, s.now())
	region, err := normalizeRegion(session.Region)
	if err != nil {
		return domain.AppleWebSession{}, err
	}
	session.Region = region
	payload, err := json.Marshal(session)
	if err != nil {
		return domain.AppleWebSession{}, wrapCryptoError(fmt.Errorf("encode rotated Apple session: %w", err))
	}
	ciphertext, err := s.cipher.EncryptAppleSession(string(payload))
	if err != nil {
		return domain.AppleWebSession{}, wrapCryptoError(fmt.Errorf("encrypt rotated Apple session: %w", err))
	}
	validatedAt := session.ValidatedAt
	if validatedAt.IsZero() {
		validatedAt = s.now().UTC()
	}
	return domain.AppleWebSession{
		AccountID:       accountID,
		Ciphertext:      ciphertext,
		AppleID:         strings.TrimSpace(session.AppleID),
		Region:          publicRegion(string(session.Region)),
		Authenticated:   true,
		LastValidatedAt: &validatedAt,
	}, nil
}

func shouldRetryAutoCreateConfirmation(err error) bool {
	if err == nil {
		return false
	}
	// errors.As returns the first matching branch of errors.Join. A failed
	// reserve/read pair can contain a non-retryable Apple error followed by a
	// retryable directory error, so inspect every branch before deciding.
	foundApple := false
	retryableApple := false
	var visit func(error)
	visit = func(current error) {
		if current == nil || retryableApple {
			return
		}
		if upstream, ok := current.(*apple.Error); ok {
			if upstream == nil {
				return
			}
			foundApple = true
			if upstream.Retryable {
				retryableApple = true
			}
			// Continue into the wrapped cause only when the typed error itself
			// was not retryable; a nested Apple error may carry the decision.
			if !upstream.Retryable {
				visit(upstream.Err)
				if upstream.Err == nil {
					visit(upstream.Kind)
				}
			}
			return
		}
		switch unwrapped := current.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range unwrapped.Unwrap() {
				visit(child)
				if retryableApple {
					return
				}
			}
		case interface{ Unwrap() error }:
			visit(unwrapped.Unwrap())
		}
	}
	visit(err)
	if retryableApple {
		return true
	}
	// A mapped non-Apple upstream marker has no typed retry metadata. Preserve
	// the historical retry behavior for that case, but do not let it override
	// an explicitly non-retryable typed Apple response.
	return !foundApple && (errors.Is(err, ErrUpstream) || errors.Is(err, ErrRateLimited))
}

func waitForAutoCreateConfirmation(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

// validateAutoCreateForwardingTarget requires Apple's explicit default because
// reserve does not accept a per-request forwarding target. Candidate and
// alias-level addresses cannot safely replace selectedForwardTo.
func validateAutoCreateForwardingTarget(result apple.ListResult, accountEmail string) error {
	selected := domain.NormalizeEmail(result.SelectedForwardTo)
	if selected == "" {
		return wrapError(CodeForwardingTargetMissing, ErrForwardingTargetMissing, nil)
	}
	if !sameEmail(selected, accountEmail) {
		return wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	return nil
}

func containsForwardingCandidate(candidates []string, accountEmail string) bool {
	for _, candidate := range candidates {
		if sameEmail(candidate, accountEmail) {
			return true
		}
	}
	return false
}

func validateSessionDSID(expected string, returned apple.Session) error {
	expected = strings.TrimSpace(expected)
	actual := strings.TrimSpace(returned.DSID)
	if expected == "" || actual == "" {
		return wrapError(CodeSessionExpired, ErrSessionExpired, nil)
	}
	if expected != actual {
		return wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	return nil
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

package hmesync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

func TestAuthChallengeIsOwnerBoundEncryptedAndSingleUse(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 7, Email: "primary@icloud.com"}, now)
	locker := &fakeLocker{}
	client := &fakeAppleClient{}
	client.signIn = func(_ context.Context, appleID, password string, region apple.Region, previous *apple.Session) (apple.Session, bool, error) {
		assertNetworkOutsideAccountLock(t, locker)
		if appleID != "owner@example.com" || password != "main-password" || region != apple.RegionChina || previous != nil {
			t.Fatalf("unexpected sign-in input: appleID=%q password=%q region=%q previous=%#v", appleID, password, region, previous)
		}
		return apple.Session{
			AppleID:      appleID,
			Region:       region,
			SessionToken: "trusted-session-token",
		}, true, nil
	}
	client.verify = func(_ context.Context, session apple.Session, code string) (apple.Session, error) {
		assertNetworkOutsideAccountLock(t, locker)
		if code != "123456" || session.SessionToken != "trusted-session-token" {
			t.Fatalf("unexpected verification input: code=%q session=%#v", code, session)
		}
		session.HSATrustedBrowser = true
		return session, nil
	}

	service := newTestService(t, repo, client, locker, func() time.Time { return now })
	started, err := service.StartAuth(ctx, 11, 7, "owner@example.com", "main-password", apple.Region("china"))
	if err != nil {
		t.Fatalf("start auth: %v", err)
	}
	if started.Status != StatusVerificationRequired || len(started.ChallengeID) < 32 ||
		started.Session.Status != StatusVerificationRequired || started.Session.Region != RegionChina {
		t.Fatalf("start result = %#v", started)
	}
	if repo.sessionCount() != 0 {
		t.Fatal("untrusted session was persisted before verification")
	}

	if _, err := service.VerifyAuth(ctx, 12, 7, started.ChallengeID, "123456"); !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("other administrator verification error = %v, want ErrFlowExpired", err)
	}
	if client.verifyCalls.Load() != 0 {
		t.Fatal("owner mismatch reached Apple verification")
	}

	verified, err := service.VerifyAuth(ctx, 11, 7, started.ChallengeID, "123456")
	if err != nil {
		t.Fatalf("verify auth: %v", err)
	}
	if verified.Status != StatusAuthenticated || verified.Session.Status != StatusAuthenticated {
		t.Fatalf("verified result = %#v", verified)
	}
	record := repo.mustSession(t, 7)
	if !strings.HasPrefix(record.Ciphertext, "as1.") || strings.Contains(record.Ciphertext, "main-password") || strings.Contains(record.Ciphertext, "123456") {
		t.Fatalf("persisted ciphertext is not an isolated Apple session ciphertext: %q", record.Ciphertext)
	}
	plaintext, err := service.cipher.DecryptAppleSession(record.Ciphertext)
	if err != nil {
		t.Fatalf("decrypt persisted session: %v", err)
	}
	if strings.Contains(plaintext, "main-password") || strings.Contains(plaintext, "123456") || !strings.Contains(plaintext, "trusted-session-token") {
		t.Fatalf("persisted session contains transient credentials or omitted session state: %s", plaintext)
	}
	if _, err := service.VerifyAuth(ctx, 11, 7, started.ChallengeID, "123456"); !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("challenge replay error = %v, want ErrFlowExpired", err)
	}
	if got := Code(wrapError(CodeFlowExpired, ErrFlowExpired, nil)); got != CodeFlowExpired {
		t.Fatalf("Code(flow error) = %q", got)
	}
}

func TestAuthChallengeExpiresWithoutCallingApple(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 1, Email: "primary@icloud.com"}, now)
	client := &fakeAppleClient{signIn: func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error) {
		return apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal}, true, nil
	}}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	service.challengeTTL = time.Minute
	started, err := service.StartAuth(ctx, 2, 1, "owner@example.com", "password", apple.RegionGlobal)
	if err != nil {
		t.Fatalf("start auth: %v", err)
	}
	now = now.Add(time.Minute)
	_, err = service.VerifyAuth(ctx, 2, 1, started.ChallengeID, "123456")
	if !errors.Is(err, ErrFlowExpired) || Code(err) != CodeFlowExpired {
		t.Fatalf("expired verification error = %v code=%q", err, Code(err))
	}
	if client.verifyCalls.Load() != 0 {
		t.Fatal("expired challenge reached Apple verification")
	}
}

func TestInvalidVerificationCanRetryUntilSuccess(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 1, Email: "primary@icloud.com"}, now)
	client := &fakeAppleClient{
		signIn: func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error) {
			return apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal}, true, nil
		},
		verify: func(_ context.Context, session apple.Session, code string) (apple.Session, error) {
			if code == "000000" {
				return apple.Session{}, apple.ErrTwoFactorCode
			}
			return session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	started, err := service.StartAuth(ctx, 2, 1, "owner@example.com", "password", apple.RegionGlobal)
	if err != nil {
		t.Fatalf("start auth: %v", err)
	}
	if _, err := service.VerifyAuth(ctx, 2, 1, started.ChallengeID, "000000"); !errors.Is(err, ErrVerificationInvalid) || Code(err) != CodeVerificationInvalid {
		t.Fatalf("invalid code error = %v code=%q", err, Code(err))
	}
	if _, err := service.VerifyAuth(ctx, 2, 1, started.ChallengeID, "123456"); err != nil {
		t.Fatalf("retry verification: %v", err)
	}
}

func TestVerificationRejectsIMAPUsernameChangeDuringAppleRequest(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{
		ID: 1, Email: "primary@icloud.com", IMAPUsername: "primary@icloud.com",
	}, now)
	client := &fakeAppleClient{
		signIn: func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error) {
			return apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal}, true, nil
		},
		verify: func(_ context.Context, session apple.Session, _ string) (apple.Session, error) {
			repo.setIMAPUsername("changed@icloud.com")
			return session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	started, err := service.StartAuth(ctx, 2, 1, "owner@example.com", "password", apple.RegionGlobal)
	if err != nil {
		t.Fatalf("start auth: %v", err)
	}
	_, err = service.VerifyAuth(ctx, 2, 1, started.ChallengeID, "123456")
	if !errors.Is(err, ErrAccountChanged) || Code(err) != CodeAccountChanged {
		t.Fatalf("verification identity race error = %v code=%q", err, Code(err))
	}
	if repo.sessionCount() != 0 {
		t.Fatal("verification resurrected a session after the IMAP identity changed")
	}
}

func TestSyncFiltersByForwardingMailboxAndReturnsOnlyNewAliases(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "Primary@icloud.com"}, now)
	locker := &fakeLocker{}
	client := &fakeAppleClient{}
	client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
		assertNetworkOutsideAccountLock(t, locker)
		session.ValidatedAt = now.Add(time.Minute)
		return session, nil
	}
	client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		assertNetworkOutsideAccountLock(t, locker)
		return apple.ListResult{
			SelectedForwardTo: "elsewhere@example.com",
			ForwardToEmails:   []string{"primary@icloud.com", "elsewhere@example.com"},
			Aliases: []apple.Alias{
				{AnonymousID: "one", HME: "NEW@icloud.com", ForwardToEmail: "primary@icloud.com", IsActive: true, Label: " New "},
				{AnonymousID: "two", HME: "old@icloud.com", ForwardToEmail: "PRIMARY@ICLOUD.COM", IsActive: false},
				{AnonymousID: "three", HME: "other@icloud.com", ForwardToEmail: "elsewhere@example.com", IsActive: true},
			},
		}, session, nil
	}
	repo.importFn = func(_ context.Context, accountID int64, candidates []domain.AliasImportCandidate) (domain.AliasImportResult, error) {
		if locker.held.Load() == 0 {
			t.Fatal("alias import did not run under the account lock")
		}
		if accountID != 3 || len(candidates) != 2 || candidates[0].Address != "new@icloud.com" ||
			candidates[0].Label != "New" || !candidates[0].Active || candidates[1].Address != "old@icloud.com" || candidates[1].Active {
			t.Fatalf("import candidates = %#v", candidates)
		}
		return domain.AliasImportResult{
			Created:               []domain.Alias{{ID: 10, AccountID: accountID, Address: candidates[0].Address, CredentialVersion: 1}},
			Existing:              []domain.Alias{{ID: 11, AccountID: accountID, Address: candidates[1].Address}},
			ImportedDisabledCount: 1,
		}, nil
	}
	service := newTestService(t, repo, client, locker, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{
		AppleID:      "owner@example.com",
		Region:       apple.RegionGlobal,
		SessionToken: "session-token",
		ValidatedAt:  now,
	})

	result, err := service.SyncAliases(ctx, 3)
	if err != nil {
		t.Fatalf("sync aliases: %v", err)
	}
	if result.Summary != (SyncSummary{Total: 2, CreatedCount: 1, ExistingCount: 1, InactiveCount: 1, ImportedDisabledCount: 1, FilteredOutCount: 1}) {
		t.Fatalf("summary = %#v", result.Summary)
	}
	if len(result.Created) != 1 || result.Created[0].Alias.Address != "new@icloud.com" ||
		result.Created[0].Alias.CredentialVersion != 1 {
		t.Fatalf("created aliases = %#v", result.Created)
	}
}

func TestSyncRejectsMismatchedAppleAccountWithoutWrites(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com"}, now)
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{
				SelectedForwardTo: "other@example.com",
				ForwardToEmails:   []string{"other@example.com"},
				Aliases: []apple.Alias{{
					AnonymousID: "other", HME: "other@icloud.com", ForwardToEmail: "other@example.com", IsActive: true,
				}},
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
	upsertsBefore := repo.upserts.Load()

	_, err := service.SyncAliases(ctx, 3)
	if !errors.Is(err, ErrAccountMismatch) || Code(err) != CodeAccountMismatch {
		t.Fatalf("mismatch error = %v code=%q", err, Code(err))
	}
	if repo.imports.Load() != 0 || repo.upserts.Load() != upsertsBefore {
		t.Fatalf("mismatched account wrote state: imports=%d upserts=%d", repo.imports.Load(), repo.upserts.Load()-upsertsBefore)
	}
}

func TestCreateAutoAliasChecksSelectedForwardBeforeReserve(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			session.SessionToken = "rotated-by-forwarding-preflight"
			return apple.ListResult{
				SelectedForwardTo: "other@example.com",
				ForwardToEmails:   []string{"primary@icloud.com", "other@example.com"},
			}, session, nil
		},
		create: func(context.Context, apple.Session, string, string) (apple.Alias, apple.Session, error) {
			t.Fatal("mismatched forwarding target reached Apple reserve")
			return apple.Alias{}, apple.Session{}, nil
		},
	}
	service := newTestService(t, repo, client, newFakeAcquiringLocker(), func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, ErrAccountMismatch) || Code(err) != CodeAccountMismatch {
		t.Fatalf("forwarding mismatch error = %v code=%q", err, Code(err))
	}
	if client.createCalls.Load() != 0 || repo.creates.Load() != 0 {
		t.Fatalf("mismatched target caused side effects: reserves=%d writes=%d", client.createCalls.Load(), repo.creates.Load())
	}
	if ctx.Err() != nil {
		t.Fatalf("forwarding preflight session checkpoint re-entered the account lock: %v", ctx.Err())
	}
	stored, decryptErr := service.decryptSession(repo.mustSession(t, 3))
	if decryptErr != nil || stored.SessionToken != "rotated-by-forwarding-preflight" {
		t.Fatalf("rotated preflight session was not preserved: session=%#v err=%v", stored, decryptErr)
	}
}

func TestCreateAutoAliasAcceptsMinimalReserveResponseAfterPreflight(t *testing.T) {
	var progress []domain.AliasCreationProgressUpdate
	ctx := domain.WithAliasCreationProgressReporter(
		context.Background(),
		func(update domain.AliasCreationProgressUpdate) {
			progress = append(progress, update)
		},
	)
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "Primary@icloud.com", Enabled: true}, now)
	var listCalls atomic.Int32
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			session.SessionToken = "listed-session-token"
			result := apple.ListResult{
				SelectedForwardTo: "primary@icloud.com",
				ForwardToEmails:   []string{"primary@icloud.com"},
			}
			if listCalls.Add(1) == 2 {
				result.Aliases = []apple.Alias{{
					HME:            "New-Alias@icloud.com",
					IsActive:       true,
					ForwardToEmail: "primary@icloud.com",
					Label:          autoCreateLabel,
					Note:           autoCreateNote,
				}}
			}
			return result, session, nil
		},
		create: func(_ context.Context, session apple.Session, label, note string) (apple.Alias, apple.Session, error) {
			if label != autoCreateLabel || note != autoCreateNote {
				t.Fatalf("automatic alias metadata = %q/%q", label, note)
			}
			if session.SessionToken != "listed-session-token" {
				t.Fatalf("automatic alias creation did not use the session returned by preflight: %#v", session)
			}
			// Apple may omit forwardToEmail from a successful reserve response.
			return apple.Alias{HME: "New-Alias@icloud.com", IsActive: true, Label: label}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	created, err := service.CreateAutoAlias(ctx, 3)
	if err != nil {
		t.Fatalf("create automatic alias: %v", err)
	}
	if created.ID == 0 || created.Address != "new-alias@icloud.com" ||
		client.createCalls.Load() != 1 || listCalls.Load() != 2 || repo.creates.Load() != 1 {
		t.Fatalf("created=%#v lists=%d reserves=%d writes=%d", created, listCalls.Load(), client.createCalls.Load(), repo.creates.Load())
	}
	wantProgress := []domain.AliasCreationProgressUpdate{
		{Phase: domain.AliasCreationPhasePreparing, Percent: 5},
		{Phase: domain.AliasCreationPhaseCheckingAccount, Percent: 10},
		{Phase: domain.AliasCreationPhaseCheckingCapacity, Percent: 15},
		{Phase: domain.AliasCreationPhaseLoadingSession, Percent: 25},
		{Phase: domain.AliasCreationPhaseValidatingSession, Percent: 35},
		{Phase: domain.AliasCreationPhaseCheckingForwarding, Percent: 45},
		{Phase: domain.AliasCreationPhasePreparingKey, Percent: 55},
		{Phase: domain.AliasCreationPhaseReserving, Percent: 65},
		{Phase: domain.AliasCreationPhaseSavingCandidate, Percent: 75},
		{Phase: domain.AliasCreationPhaseReconciling, Percent: 85, Attempt: 1},
		{Phase: domain.AliasCreationPhaseConfirming, Percent: 85, Attempt: 1},
		{Phase: domain.AliasCreationPhaseSavingResult, Percent: 95, Attempt: 1},
		{Phase: domain.AliasCreationPhaseCompleted, Percent: 100},
	}
	if len(progress) != len(wantProgress) {
		t.Fatalf("progress updates = %#v, want %#v", progress, wantProgress)
	}
	for index := range wantProgress {
		if progress[index] != wantProgress[index] {
			t.Fatalf("progress[%d] = %#v, want %#v", index, progress[index], wantProgress[index])
		}
	}
	encodedProgress, err := json.Marshal(progress)
	if err != nil {
		t.Fatalf("encode progress: %v", err)
	}
	for _, sensitive := range []string{"new-alias@icloud.com", "owner@example.com", "listed-session-token"} {
		if strings.Contains(string(encodedProgress), sensitive) {
			t.Fatalf("progress exposed sensitive value %q: %s", sensitive, encodedProgress)
		}
	}
}

func TestShouldRetryAutoCreateConfirmation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "directory visibility lag",
			err:  wrapError(CodeUpstreamError, ErrUpstream, errors.New("alias not visible yet")),
			want: true,
		},
		{
			name: "retryable Apple response",
			err: wrapError(CodeUpstreamError, ErrUpstream, &apple.Error{
				Kind:       apple.ErrService,
				StatusCode: 503,
				Retryable:  true,
			}),
			want: true,
		},
		{
			name: "non-retryable Apple response",
			err: wrapError(CodeUpstreamError, ErrUpstream, &apple.Error{
				Kind:       apple.ErrService,
				StatusCode: 409,
			}),
		},
		{
			name: "retryable Apple response in a later joined cause",
			err: errors.Join(
				wrapError(CodeUpstreamError, ErrUpstream, &apple.Error{
					Op:         "reserve Hide My Email alias",
					Kind:       apple.ErrService,
					StatusCode: 409,
				}),
				wrapError(CodeUpstreamError, ErrUpstream, &apple.Error{
					Op:         "list Hide My Email aliases",
					Kind:       apple.ErrService,
					StatusCode: 503,
					Retryable:  true,
				}),
			),
			want: true,
		},
		{name: "rate limit without typed cause", err: ErrRateLimited, want: true},
		{name: "canceled", err: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldRetryAutoCreateConfirmation(test.err); got != test.want {
				t.Fatalf("shouldRetryAutoCreateConfirmation() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCreateAutoAliasRetriesConfirmationUntilReservedAliasAppears(t *testing.T) {
	var progress []domain.AliasCreationProgressUpdate
	ctx := domain.WithAliasCreationProgressReporter(
		context.Background(),
		func(update domain.AliasCreationProgressUpdate) {
			progress = append(progress, update)
		},
	)
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	var listCalls atomic.Int32
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			call := listCalls.Add(1)
			result := apple.ListResult{SelectedForwardTo: "primary@icloud.com"}
			switch call {
			case 1:
				if session.SessionToken != "initial-session-token" {
					t.Fatalf("preflight session token = %q", session.SessionToken)
				}
				session.SessionToken = "preflight-session-token"
			case 2:
				if session.SessionToken != "reserve-session-token" {
					t.Fatalf("first confirmation session token = %q", session.SessionToken)
				}
				session.SessionToken = "confirmation-session-token-1"
			case 3:
				if session.SessionToken != "confirmation-session-token-1" {
					t.Fatalf("second confirmation session token = %q", session.SessionToken)
				}
				session.SessionToken = "confirmation-session-token-2"
			case 4:
				if session.SessionToken != "confirmation-session-token-2" {
					t.Fatalf("third confirmation session token = %q", session.SessionToken)
				}
				session.SessionToken = "confirmation-session-token-3"
				result.Aliases = []apple.Alias{{
					HME:            "new-alias@icloud.com",
					IsActive:       true,
					ForwardToEmail: "primary@icloud.com",
					Label:          autoCreateLabel,
					Note:           autoCreateNote,
				}}
			default:
				t.Fatalf("unexpected confirmation list call %d", call)
			}
			return result, session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			if session.SessionToken != "preflight-session-token" {
				t.Fatalf("reserve session token = %q", session.SessionToken)
			}
			session.SessionToken = "reserve-session-token"
			return apple.Alias{HME: "new-alias@icloud.com", IsActive: true}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	service.autoCreateConfirmationDelays = []time.Duration{0, 0, 0}
	storeSession(t, service, repo, 3, apple.Session{
		AppleID:      "owner@example.com",
		Region:       apple.RegionGlobal,
		SessionToken: "initial-session-token",
	})

	created, err := service.CreateAutoAlias(ctx, 3)
	if err != nil {
		t.Fatalf("create automatic alias after delayed confirmation: %v", err)
	}
	if created.ID == 0 || created.Address != "new-alias@icloud.com" ||
		listCalls.Load() != 4 || client.createCalls.Load() != 1 || repo.creates.Load() != 1 {
		t.Fatalf("created=%#v lists=%d reserves=%d writes=%d", created, listCalls.Load(), client.createCalls.Load(), repo.creates.Load())
	}
	stored, decryptErr := service.decryptSession(repo.mustSession(t, 3))
	if decryptErr != nil || stored.SessionToken != "confirmation-session-token-3" {
		t.Fatalf("latest confirmation session was not persisted: session=%#v err=%v", stored, decryptErr)
	}
	var reconciliationAttempts []int
	for _, update := range progress {
		if update.Phase != domain.AliasCreationPhaseReconciling {
			continue
		}
		if update.Percent != 85 {
			t.Fatalf("reconciliation percent = %d, want 85", update.Percent)
		}
		reconciliationAttempts = append(reconciliationAttempts, update.Attempt)
	}
	wantAttempts := []int{1, 2, 3}
	if len(reconciliationAttempts) != len(wantAttempts) {
		t.Fatalf("reconciliation attempts = %v, want %v", reconciliationAttempts, wantAttempts)
	}
	for index := range wantAttempts {
		if reconciliationAttempts[index] != wantAttempts[index] {
			t.Fatalf("reconciliation attempts = %v, want %v", reconciliationAttempts, wantAttempts)
		}
	}
}

func TestCreateAutoAliasRetriesRetryableConfirmationError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	var listCalls atomic.Int32
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			call := listCalls.Add(1)
			switch call {
			case 1:
				session.SessionToken = "preflight-session-token"
				return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
			case 2:
				if session.SessionToken != "reserve-session-token" {
					t.Fatalf("failed confirmation session token = %q", session.SessionToken)
				}
				session.SessionToken = "retryable-error-session-token"
				return apple.ListResult{}, session, &apple.Error{
					Op:         "list Hide My Email aliases",
					Kind:       apple.ErrService,
					StatusCode: 503,
					Retryable:  true,
				}
			case 3:
				if session.SessionToken != "retryable-error-session-token" {
					t.Fatalf("retried confirmation session token = %q", session.SessionToken)
				}
				session.SessionToken = "successful-confirmation-session-token"
				return apple.ListResult{
					SelectedForwardTo: "primary@icloud.com",
					Aliases: []apple.Alias{{
						HME:            "new-alias@icloud.com",
						IsActive:       true,
						ForwardToEmail: "primary@icloud.com",
					}},
				}, session, nil
			default:
				t.Fatalf("unexpected confirmation list call %d", call)
				return apple.ListResult{}, session, nil
			}
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			if session.SessionToken != "preflight-session-token" {
				t.Fatalf("reserve session token = %q", session.SessionToken)
			}
			session.SessionToken = "reserve-session-token"
			return apple.Alias{HME: "new-alias@icloud.com", IsActive: true}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	service.autoCreateConfirmationDelays = []time.Duration{0, 0}
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	created, err := service.CreateAutoAlias(ctx, 3)
	if err != nil {
		t.Fatalf("create automatic alias after retryable confirmation error: %v", err)
	}
	if created.ID == 0 || created.Address != "new-alias@icloud.com" ||
		listCalls.Load() != 3 || client.createCalls.Load() != 1 || repo.creates.Load() != 1 {
		t.Fatalf("created=%#v lists=%d reserves=%d writes=%d", created, listCalls.Load(), client.createCalls.Load(), repo.creates.Load())
	}
	stored, decryptErr := service.decryptSession(repo.mustSession(t, 3))
	if decryptErr != nil || stored.SessionToken != "successful-confirmation-session-token" {
		t.Fatalf("latest successful confirmation session was not persisted: session=%#v err=%v", stored, decryptErr)
	}
}

func TestCreateAutoAliasPersistsAndReconcilesPendingConfirmationWithoutAnotherReserve(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	var listCalls atomic.Int32
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			call := listCalls.Add(1)
			result := apple.ListResult{SelectedForwardTo: "primary@icloud.com"}
			switch call {
			case 1:
				session.SessionToken = "preflight-session-token"
			case 2:
				if session.SessionToken != "reserve-session-token" {
					t.Fatalf("first confirmation session token = %q", session.SessionToken)
				}
				session.SessionToken = "pending-session-token"
			case 3:
				if session.SessionToken != "pending-session-token" {
					t.Fatalf("next-slot session token = %q", session.SessionToken)
				}
				return apple.ListResult{}, session, &apple.Error{
					Kind:       apple.ErrService,
					StatusCode: 503,
					Retryable:  true,
				}
			case 4:
				if session.SessionToken != "pending-session-token" {
					t.Fatalf("confirmed-slot session token = %q", session.SessionToken)
				}
				session.SessionToken = "confirmed-session-token"
				result.Aliases = []apple.Alias{{
					HME:            "new-alias@icloud.com",
					IsActive:       true,
					ForwardToEmail: "primary@icloud.com",
					Label:          autoCreateLabel,
				}}
			default:
				t.Fatalf("unexpected list call %d", call)
			}
			return result, session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			if session.SessionToken != "preflight-session-token" {
				t.Fatalf("reserve session token = %q", session.SessionToken)
			}
			session.SessionToken = "reserve-session-token"
			return apple.Alias{HME: "new-alias@icloud.com", IsActive: true}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	service.autoCreateConfirmationDelays = nil
	storeSession(t, service, repo, 3, apple.Session{
		AppleID:      "owner@example.com",
		Region:       apple.RegionGlobal,
		SessionToken: "initial-session-token",
	})

	first, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, ErrAliasConfirmationPending) || Code(err) != CodeAliasConfirmationPending {
		t.Fatalf("first attempt = %#v, err=%v code=%q", first, err, Code(err))
	}
	if first.ID != 0 || client.createCalls.Load() != 1 || repo.creates.Load() != 1 || repo.confirms.Load() != 0 {
		t.Fatalf("first attempt side effects: alias=%#v reserves=%d creates=%d confirms=%d",
			first, client.createCalls.Load(), repo.creates.Load(), repo.confirms.Load())
	}
	pending, pendingErr := repo.GetPendingAutoAliasConfirmation(ctx, 3)
	if pendingErr != nil || pending.ID == 0 || pending.Enabled ||
		pending.LastSyncError != domain.AppleAliasConfirmationPending ||
		pending.CredentialVersion != 1 {
		t.Fatalf("persisted pending confirmation = %#v, err=%v", pending, pendingErr)
	}

	second, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, ErrAliasConfirmationPending) || Code(err) != CodeAliasConfirmationPending || second.ID != 0 {
		t.Fatalf("second attempt = %#v, err=%v code=%q", second, err, Code(err))
	}
	if client.createCalls.Load() != 1 || repo.creates.Load() != 1 || repo.confirms.Load() != 0 {
		t.Fatalf("second attempt repeated side effects: reserves=%d creates=%d confirms=%d",
			client.createCalls.Load(), repo.creates.Load(), repo.confirms.Load())
	}

	confirmed, err := service.CreateAutoAlias(ctx, 3)
	if err != nil {
		t.Fatalf("reconcile pending confirmation: %v", err)
	}
	if confirmed.ID != pending.ID || confirmed.Address != pending.Address || !confirmed.Enabled ||
		confirmed.LastSyncError != "" || client.createCalls.Load() != 1 || repo.creates.Load() != 1 ||
		repo.confirms.Load() != 1 || listCalls.Load() != 4 {
		t.Fatalf("confirmed=%#v lists=%d reserves=%d creates=%d confirms=%d",
			confirmed, listCalls.Load(), client.createCalls.Load(), repo.creates.Load(), repo.confirms.Load())
	}
	stored, decryptErr := service.decryptSession(repo.mustSession(t, 3))
	if decryptErr != nil || stored.SessionToken != "confirmed-session-token" {
		t.Fatalf("confirmed session = %#v, err=%v", stored, decryptErr)
	}
}

func TestCreateAutoAliasConfirmsMissingReserveForwardFromAuthoritativeList(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	var listCalls atomic.Int32
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			result := apple.ListResult{SelectedForwardTo: "primary@icloud.com"}
			if listCalls.Add(1) == 2 {
				result.SelectedForwardTo = "other@example.com"
				result.Aliases = []apple.Alias{{
					HME:            "new-alias@icloud.com",
					IsActive:       true,
					ForwardToEmail: "other@example.com",
					Label:          autoCreateLabel,
					Note:           autoCreateNote,
				}}
			}
			return result, session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			return apple.Alias{HME: "new-alias@icloud.com", IsActive: true}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, ErrAccountMismatch) || Code(err) != CodeAccountMismatch {
		t.Fatalf("post-reserve forwarding mismatch error = %v code=%q", err, Code(err))
	}
	if listCalls.Load() != 2 || client.createCalls.Load() != 1 || repo.creates.Load() != 1 || repo.confirms.Load() != 0 {
		t.Fatalf("post-reserve mismatch calls: lists=%d reserves=%d writes=%d", listCalls.Load(), client.createCalls.Load(), repo.creates.Load())
	}
	if pending, pendingErr := repo.GetPendingAutoAliasConfirmation(ctx, 3); pendingErr != nil || pending.Address != "new-alias@icloud.com" {
		t.Fatalf("post-reserve mismatch lost pending alias: pending=%#v err=%v", pending, pendingErr)
	}
}

func TestCreateAutoAliasPreservesRotatedSessionWhenReserveFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	var listCalls atomic.Int32
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			if listCalls.Add(1) == 1 {
				session.SessionToken = "preflight-session-token"
			}
			return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			session.SessionToken = "rotated-during-reserve"
			return apple.Alias{HME: "new-alias@icloud.com"}, session, errors.New("ambiguous reserve failure")
		},
	}
	service := newTestService(t, repo, client, newFakeAcquiringLocker(), func() time.Time { return now })
	service.autoCreateConfirmationDelays = nil
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, ErrAliasConfirmationPending) || Code(err) != CodeAliasConfirmationPending {
		t.Fatalf("reserve failure error = %v code=%q", err, Code(err))
	}
	if ctx.Err() != nil {
		t.Fatalf("reserve failure checkpoint re-entered the account lock: %v", ctx.Err())
	}
	stored, decryptErr := service.decryptSession(repo.mustSession(t, 3))
	if decryptErr != nil || stored.SessionToken != "rotated-during-reserve" {
		t.Fatalf("reserve failure lost rotated session: session=%#v err=%v", stored, decryptErr)
	}
	if client.createCalls.Load() != 1 || repo.creates.Load() != 1 {
		t.Fatalf("reserve failure calls: reserves=%d writes=%d", client.createCalls.Load(), repo.creates.Load())
	}
	pending, pendingErr := repo.GetPendingAutoAliasConfirmation(ctx, 3)
	if pendingErr != nil || pending.Address != "new-alias@icloud.com" || pending.Label != autoCreateLabel {
		t.Fatalf("ambiguous reserve pending alias = %#v, err=%v", pending, pendingErr)
	}
}

func TestCreateAutoAliasPersistsCandidateAfterCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	var listCalls atomic.Int32
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(callContext context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			if listCalls.Add(1) == 1 {
				return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
			}
			return apple.ListResult{}, session, callContext.Err()
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			session.SessionToken = "reserve-session-token"
			cancel()
			return apple.Alias{HME: "new-alias@icloud.com", IsActive: true}, session, nil
		},
	}
	service := newTestService(t, repo, client, newFakeAcquiringLocker(), func() time.Time { return now })
	service.autoCreateConfirmationDelays = nil
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reserve result error = %v", err)
	}
	if client.createCalls.Load() != 1 || repo.creates.Load() != 1 || repo.confirms.Load() != 0 {
		t.Fatalf("canceled reserve side effects: reserves=%d creates=%d confirms=%d",
			client.createCalls.Load(), repo.creates.Load(), repo.confirms.Load())
	}
	pending, pendingErr := repo.GetPendingAutoAliasConfirmation(context.Background(), 3)
	if pendingErr != nil || pending.Address != "new-alias@icloud.com" || pending.Label != autoCreateLabel {
		t.Fatalf("canceled reserve pending alias = %#v, err=%v", pending, pendingErr)
	}
	stored, decryptErr := service.decryptSession(repo.mustSession(t, 3))
	if decryptErr != nil || stored.SessionToken != "reserve-session-token" {
		t.Fatalf("canceled reserve session = %#v, err=%v", stored, decryptErr)
	}
}

func TestValidateAutoCreateForwardingTarget(t *testing.T) {
	tests := []struct {
		name     string
		result   apple.ListResult
		wantCode string
		wantKind error
	}{
		{
			name:   "selected target is authoritative",
			result: apple.ListResult{SelectedForwardTo: " PRIMARY@ICLOUD.COM ", ForwardToEmails: []string{"other@example.com"}},
		},
		{
			name: "selected mismatch is not overridden",
			result: apple.ListResult{
				SelectedForwardTo: "other@example.com",
				ForwardToEmails:   []string{"primary@icloud.com"},
			},
			wantCode: CodeAccountMismatch,
			wantKind: ErrAccountMismatch,
		},
		{
			name:     "available target does not replace missing selection",
			result:   apple.ListResult{ForwardToEmails: []string{"primary@icloud.com"}},
			wantCode: CodeForwardingTargetMissing,
			wantKind: ErrForwardingTargetMissing,
		},
		{
			name: "existing alias does not replace missing selection",
			result: apple.ListResult{Aliases: []apple.Alias{{
				HME: "existing@icloud.com", ForwardToEmail: "primary@icloud.com", IsActive: true,
			}}},
			wantCode: CodeForwardingTargetMissing,
			wantKind: ErrForwardingTargetMissing,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAutoCreateForwardingTarget(test.result, "primary@icloud.com")
			if Code(err) != test.wantCode {
				t.Fatalf("error = %v code=%q, want %q", err, Code(err), test.wantCode)
			}
			if test.wantKind == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.wantKind != nil && !errors.Is(err, test.wantKind) {
				t.Fatalf("error = %v, want kind %v", err, test.wantKind)
			}
		})
	}
}

func TestCreateAutoAliasInitializesMissingForwardingTarget(t *testing.T) {
	var progress []domain.AliasCreationProgressUpdate
	ctx := domain.WithAliasCreationProgressReporter(context.Background(), func(update domain.AliasCreationProgressUpdate) {
		progress = append(progress, update)
	})
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	var events []string
	listCalls := 0
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			listCalls++
			events = append(events, fmt.Sprintf("list-%d", listCalls))
			if listCalls == 1 {
				session.SessionToken = "listed-session"
				return apple.ListResult{ForwardToEmails: []string{"primary@icloud.com"}}, session, nil
			}
			if session.SessionToken != "updated-session" {
				t.Fatalf("forwarding verification session = %#v", session)
			}
			session.SessionToken = "verified-session"
			return apple.ListResult{
				SelectedForwardTo: "primary@icloud.com",
				ForwardToEmails:   []string{"primary@icloud.com"},
			}, session, nil
		},
		update: func(_ context.Context, session apple.Session, forwardToEmail string) (apple.Session, error) {
			events = append(events, "update")
			if session.SessionToken != "listed-session" || forwardToEmail != "primary@icloud.com" {
				t.Fatalf("forwarding update = session %#v target %q", session, forwardToEmail)
			}
			session.SessionToken = "updated-session"
			return session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			events = append(events, "create")
			if session.SessionToken != "verified-session" {
				t.Fatalf("reserve session = %#v", session)
			}
			return apple.Alias{
				HME: "created@icloud.com", ForwardToEmail: "primary@icloud.com", IsActive: true,
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal, DSID: "42"})

	created, err := service.CreateAutoAlias(ctx, 3)
	if err != nil {
		t.Fatalf("initialize forwarding and create: %v", err)
	}
	if created.Address != "created@icloud.com" || listCalls != 2 || client.updateCalls.Load() != 1 ||
		client.createCalls.Load() != 1 || repo.creates.Load() != 1 || strings.Join(events, ",") != "list-1,update,list-2,create" {
		t.Fatalf("created=%#v lists=%d updates=%d reserves=%d writes=%d events=%v",
			created, listCalls, client.updateCalls.Load(), client.createCalls.Load(), repo.creates.Load(), events)
	}
	if !containsAliasCreationPhase(progress, domain.AliasCreationPhaseInitializingForwarding) {
		t.Fatalf("forwarding initialization progress missing: %#v", progress)
	}
}

func TestCreateAutoAliasDoesNotInitializeUnsafeForwardingState(t *testing.T) {
	tests := []struct {
		name   string
		result apple.ListResult
	}{
		{
			name: "existing remote alias",
			result: apple.ListResult{
				ForwardToEmails: []string{"primary@icloud.com"},
				Aliases: []apple.Alias{{
					HME: "existing@icloud.com", ForwardToEmail: "primary@icloud.com", IsActive: true,
				}},
			},
		},
		{name: "current mailbox absent from candidates", result: apple.ListResult{ForwardToEmails: []string{"other@example.com"}}},
		{name: "no forwarding candidates", result: apple.ListResult{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
			repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
			client := &fakeAppleClient{
				validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
				list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
					return test.result, session, nil
				},
				update: func(context.Context, apple.Session, string) (apple.Session, error) {
					t.Fatal("unsafe forwarding state reached account-level update")
					return apple.Session{}, nil
				},
				create: func(context.Context, apple.Session, string, string) (apple.Alias, apple.Session, error) {
					t.Fatal("unsafe forwarding state reached reserve")
					return apple.Alias{}, apple.Session{}, nil
				},
			}
			service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
			storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

			_, err := service.CreateAutoAlias(context.Background(), 3)
			if !errors.Is(err, ErrForwardingTargetMissing) || Code(err) != CodeForwardingTargetMissing {
				t.Fatalf("unsafe forwarding error = %v code=%q", err, Code(err))
			}
			if client.updateCalls.Load() != 0 || client.createCalls.Load() != 0 || repo.creates.Load() != 0 {
				t.Fatalf("unsafe forwarding side effects: updates=%d reserves=%d writes=%d",
					client.updateCalls.Load(), client.createCalls.Load(), repo.creates.Load())
			}
		})
	}
}

func TestCreateAutoAliasReconcilesUncertainForwardingUpdate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	listCalls := 0
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			listCalls++
			if listCalls == 1 {
				return apple.ListResult{ForwardToEmails: []string{"primary@icloud.com"}}, session, nil
			}
			return apple.ListResult{
				SelectedForwardTo: "primary@icloud.com",
				ForwardToEmails:   []string{"primary@icloud.com"},
			}, session, nil
		},
		update: func(_ context.Context, session apple.Session, _ string) (apple.Session, error) {
			session.SessionToken = "rotated-during-uncertain-update"
			return session, &apple.Error{
				Op: "update Hide My Email forwarding target", Kind: apple.ErrService,
				StatusCode: http.StatusServiceUnavailable, Retryable: true,
			}
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			if session.SessionToken != "rotated-during-uncertain-update" {
				t.Fatalf("reserve lost uncertain update session: %#v", session)
			}
			return apple.Alias{
				HME: "created@icloud.com", ForwardToEmail: "primary@icloud.com", IsActive: true,
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	created, err := service.CreateAutoAlias(ctx, 3)
	if err != nil || created.Address != "created@icloud.com" || listCalls != 2 ||
		client.updateCalls.Load() != 1 || client.createCalls.Load() != 1 {
		t.Fatalf("created=%#v err=%v lists=%d updates=%d reserves=%d",
			created, err, listCalls, client.updateCalls.Load(), client.createCalls.Load())
	}
}

func TestCreateAutoAliasReconcilesCanceledForwardingUpdateWithoutReserve(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	listCalls := 0
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(callContext context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			listCalls++
			if listCalls == 1 {
				return apple.ListResult{ForwardToEmails: []string{"primary@icloud.com"}}, session, nil
			}
			if callContext.Err() != nil {
				t.Fatalf("forwarding recovery inherited canceled context: %v", callContext.Err())
			}
			if _, hasDeadline := callContext.Deadline(); !hasDeadline {
				t.Fatal("forwarding recovery context has no deadline")
			}
			if session.SessionToken != "rotated-during-canceled-update" {
				t.Fatalf("forwarding recovery session = %#v", session)
			}
			return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
		},
		update: func(_ context.Context, session apple.Session, _ string) (apple.Session, error) {
			session.SessionToken = "rotated-during-canceled-update"
			cancel()
			return session, &apple.Error{
				Op: "update Hide My Email forwarding target", Kind: apple.ErrInvalidResponse,
				StatusCode: http.StatusOK, Err: context.Canceled,
			}
		},
		create: func(context.Context, apple.Session, string, string) (apple.Alias, apple.Session, error) {
			t.Fatal("canceled forwarding recovery reached reserve")
			return apple.Alias{}, apple.Session{}, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, context.Canceled) || listCalls != 2 || client.updateCalls.Load() != 1 ||
		client.createCalls.Load() != 0 || repo.creates.Load() != 0 {
		t.Fatalf("err=%v lists=%d updates=%d reserves=%d writes=%d",
			err, listCalls, client.updateCalls.Load(), client.createCalls.Load(), repo.creates.Load())
	}
	assertStoredAppleSessionToken(t, service, repo, 3, "rotated-during-canceled-update")
}

func TestCreateAutoAliasPersistsForwardingVerificationSessionOnFailure(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	listCalls := 0
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			listCalls++
			if listCalls == 1 {
				return apple.ListResult{ForwardToEmails: []string{"primary@icloud.com"}}, session, nil
			}
			session.SessionToken = "rotated-during-failed-verification"
			return apple.ListResult{}, session, &apple.Error{
				Op: "list Hide My Email aliases", Kind: apple.ErrService,
				StatusCode: http.StatusServiceUnavailable, Retryable: true,
			}
		},
		update: func(_ context.Context, session apple.Session, _ string) (apple.Session, error) {
			session.SessionToken = "rotated-during-update"
			return session, nil
		},
		create: func(context.Context, apple.Session, string, string) (apple.Alias, apple.Session, error) {
			t.Fatal("failed forwarding verification reached reserve")
			return apple.Alias{}, apple.Session{}, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(context.Background(), 3)
	if Code(err) != CodeUpstreamError || !errors.Is(err, ErrUpstream) || listCalls != 2 ||
		client.updateCalls.Load() != 1 || client.createCalls.Load() != 0 {
		t.Fatalf("err=%v code=%q lists=%d updates=%d reserves=%d",
			err, Code(err), listCalls, client.updateCalls.Load(), client.createCalls.Load())
	}
	assertStoredAppleSessionToken(t, service, repo, 3, "rotated-during-failed-verification")
}

func TestCreateAutoAliasRequiresExplicitForwardingReadback(t *testing.T) {
	tests := []struct {
		name     string
		verified apple.ListResult
		wantCode string
		wantKind error
	}{
		{
			name:     "selection still missing",
			verified: apple.ListResult{ForwardToEmails: []string{"primary@icloud.com"}},
			wantCode: CodeForwardingTargetMissing,
			wantKind: ErrForwardingTargetMissing,
		},
		{
			name:     "selection changed to another mailbox",
			verified: apple.ListResult{SelectedForwardTo: "other@example.com"},
			wantCode: CodeAccountMismatch,
			wantKind: ErrAccountMismatch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
			repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
			listCalls := 0
			client := &fakeAppleClient{
				validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
				list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
					listCalls++
					if listCalls == 1 {
						return apple.ListResult{ForwardToEmails: []string{"primary@icloud.com"}}, session, nil
					}
					return test.verified, session, nil
				},
				update: func(_ context.Context, session apple.Session, _ string) (apple.Session, error) {
					return session, nil
				},
				create: func(context.Context, apple.Session, string, string) (apple.Alias, apple.Session, error) {
					t.Fatal("unverified forwarding initialization reached reserve")
					return apple.Alias{}, apple.Session{}, nil
				},
			}
			service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
			storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

			_, err := service.CreateAutoAlias(context.Background(), 3)
			if Code(err) != test.wantCode || !errors.Is(err, test.wantKind) || listCalls != 2 ||
				client.updateCalls.Load() != 1 || client.createCalls.Load() != 0 || repo.creates.Load() != 0 {
				t.Fatalf("err=%v code=%q lists=%d updates=%d reserves=%d writes=%d",
					err, Code(err), listCalls, client.updateCalls.Load(), client.createCalls.Load(), repo.creates.Load())
			}
		})
	}
}

func TestCreateAutoAliasRejectsDSIDChangeDuringForwardingInitialization(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	listCalls := 0
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			listCalls++
			return apple.ListResult{ForwardToEmails: []string{"primary@icloud.com"}}, session, nil
		},
		update: func(_ context.Context, session apple.Session, _ string) (apple.Session, error) {
			session.DSID = "different-account"
			return session, nil
		},
		create: func(context.Context, apple.Session, string, string) (apple.Alias, apple.Session, error) {
			t.Fatal("changed Apple account reached reserve")
			return apple.Alias{}, apple.Session{}, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{
		AppleID: "owner@example.com", Region: apple.RegionGlobal, DSID: "expected-account",
	})

	_, err := service.CreateAutoAlias(context.Background(), 3)
	if Code(err) != CodeAccountMismatch || !errors.Is(err, ErrAccountMismatch) || listCalls != 1 ||
		client.updateCalls.Load() != 1 || client.createCalls.Load() != 0 {
		t.Fatalf("err=%v code=%q lists=%d updates=%d reserves=%d",
			err, Code(err), listCalls, client.updateCalls.Load(), client.createCalls.Load())
	}
}

func TestCreateAutoAliasRejectsStoredSessionWithoutDSID(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	client := &fakeAppleClient{
		validate: func(context.Context, apple.Session) (apple.Session, error) {
			t.Fatal("session without a trusted DSID reached Apple validation")
			return apple.Session{}, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSessionExactly(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(context.Background(), 3)
	if Code(err) != CodeSessionExpired || !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("missing DSID error = %v code=%q", err, Code(err))
	}
	if _, sessionErr := repo.GetAppleWebSession(context.Background(), 3); !errors.Is(sessionErr, store.ErrNotFound) {
		t.Fatalf("session without DSID was not removed: %v", sessionErr)
	}
}

func TestCreateAutoAliasRejectsExplicitReserveForwardMismatch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			return apple.Alias{
				HME:            "new-alias@icloud.com",
				IsActive:       true,
				ForwardToEmail: "other@example.com",
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, ErrAccountMismatch) || Code(err) != CodeAccountMismatch {
		t.Fatalf("reserve forwarding mismatch error = %v code=%q", err, Code(err))
	}
	if client.createCalls.Load() != 1 || repo.creates.Load() != 1 {
		t.Fatalf("reserve mismatch side effects: reserves=%d writes=%d", client.createCalls.Load(), repo.creates.Load())
	}
	if pending, pendingErr := repo.GetPendingAutoAliasConfirmation(ctx, 3); pendingErr != nil ||
		pending.Address != "new-alias@icloud.com" {
		t.Fatalf("reserve mismatch lost pending alias: pending=%#v err=%v", pending, pendingErr)
	}
}

func TestCreateAutoAliasRejectsExplicitInactiveReserveResult(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			return apple.Alias{
				HME:            "new-alias@icloud.com",
				IsActive:       false,
				ForwardToEmail: "primary@icloud.com",
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, ErrAliasConfirmationPending) || Code(err) != CodeAliasConfirmationPending {
		t.Fatalf("inactive reserve error = %v code=%q", err, Code(err))
	}
	if client.createCalls.Load() != 1 || repo.creates.Load() != 1 {
		t.Fatalf("inactive reserve side effects: reserves=%d writes=%d", client.createCalls.Load(), repo.creates.Load())
	}
	if pending, pendingErr := repo.GetPendingAutoAliasConfirmation(ctx, 3); pendingErr != nil ||
		pending.Address != "new-alias@icloud.com" {
		t.Fatalf("inactive reserve lost pending alias: pending=%#v err=%v", pending, pendingErr)
	}
}

func TestCreateAutoAliasPreservesConfirmationPersistenceCause(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	repo.confirmErr = store.ErrAliasLimit
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			return apple.Alias{
				HME:            "new-alias@icloud.com",
				IsActive:       true,
				ForwardToEmail: "primary@icloud.com",
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if Code(err) != CodePersistenceError || !errors.Is(err, ErrPersistence) ||
		!errors.Is(err, store.ErrAliasLimit) {
		t.Fatalf("confirmation persistence error = %v code=%q", err, Code(err))
	}
	if repo.creates.Load() != 1 || repo.confirms.Load() != 0 {
		t.Fatalf("confirmation persistence side effects: creates=%d confirms=%d", repo.creates.Load(), repo.confirms.Load())
	}
	if _, pendingErr := repo.GetPendingAutoAliasConfirmation(ctx, 3); pendingErr != nil {
		t.Fatalf("pending alias was lost after confirmation persistence failure: %v", pendingErr)
	}
}

func TestCreateAutoAliasClassifiesPendingSessionCheckpointAsPersistence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	repo.pending = &domain.Alias{
		ID:                77,
		AccountID:         3,
		Address:           "pending@icloud.com",
		Enabled:           false,
		LastSyncStatus:    domain.SyncStatusPending,
		LastSyncError:     domain.AppleAliasConfirmationPending,
		CredentialVersion: 1,
	}
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			session.SessionToken = "rotated-session-token"
			return apple.ListResult{
				SelectedForwardTo: "primary@icloud.com",
				Aliases: []apple.Alias{{
					HME:            "pending@icloud.com",
					IsActive:       true,
					ForwardToEmail: "primary@icloud.com",
				}},
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
	repo.upsertSessionErr = errors.New("database unavailable")

	_, err := service.CreateAutoAlias(ctx, 3)
	if Code(err) != CodePersistenceError || !errors.Is(err, ErrPersistence) {
		t.Fatalf("pending session checkpoint error = %v code=%q", err, Code(err))
	}
	if errors.Is(err, ErrAliasConfirmationPending) {
		t.Fatalf("pending session checkpoint was relabeled as directory confirmation: %v", err)
	}
	if repo.confirms.Load() != 0 {
		t.Fatalf("failed session checkpoint reached confirmation: confirms=%d", repo.confirms.Load())
	}
}

func TestCreateAutoAliasFallsBackOnlyForUncodedSessionPersistenceError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			session.Region = apple.Region("unknown")
			return apple.Alias{
				HME:            "new-alias@icloud.com",
				IsActive:       true,
				ForwardToEmail: "primary@icloud.com",
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if Code(err) != CodePersistenceError || !errors.Is(err, ErrPersistence) {
		t.Fatalf("uncoded session persistence error = %v code=%q", err, Code(err))
	}
	if client.createCalls.Load() != 1 || repo.creates.Load() != 1 || repo.confirms.Load() != 0 {
		t.Fatalf("uncoded session fallback side effects: reserves=%d creates=%d confirms=%d",
			client.createCalls.Load(), repo.creates.Load(), repo.confirms.Load())
	}
	if _, pendingErr := repo.GetPendingAutoAliasConfirmation(ctx, 3); pendingErr != nil {
		t.Fatalf("uncoded session fallback did not retain candidate: %v", pendingErr)
	}
}

func TestCreateAutoAliasPublishesCandidateBeforeReturningCodedSessionError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			session.AppleID = "other@example.com"
			return apple.Alias{
				HME:            "new-alias@icloud.com",
				IsActive:       true,
				ForwardToEmail: "primary@icloud.com",
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if Code(err) != CodeAccountMismatch || !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("coded session error = %v code=%q", err, Code(err))
	}
	var pendingMarker interface{ PendingConfirmation() bool }
	if !errors.As(err, &pendingMarker) || !pendingMarker.PendingConfirmation() {
		t.Fatalf("coded session error did not report a pending candidate: %T %v", err, err)
	}
	if client.createCalls.Load() != 1 || repo.creates.Load() != 1 {
		t.Fatalf("coded session error candidate writes: reserves=%d creates=%d", client.createCalls.Load(), repo.creates.Load())
	}
	if pending, pendingErr := repo.GetPendingAutoAliasConfirmation(ctx, 3); pendingErr != nil || pending.Address != "new-alias@icloud.com" {
		t.Fatalf("coded session error lost pending alias: pending=%#v err=%v", pending, pendingErr)
	}
}

func TestCreateAutoAliasPrioritizesLatestConfirmationError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	var listCalls atomic.Int32
	reserveCause := &apple.Error{
		Op: "reserve Hide My Email alias", Kind: apple.ErrService, StatusCode: 503,
		ServiceCode: "RESERVE_FIXTURE", Retryable: true,
	}
	confirmationCause := &apple.Error{
		Op: "list Hide My Email aliases", Kind: apple.ErrService, StatusCode: 409,
		ServiceCode: "CONFIRM_FIXTURE", Retryable: false,
	}
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			if listCalls.Add(1) == 1 {
				return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
			}
			return apple.ListResult{}, session, confirmationCause
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			return apple.Alias{HME: "new-alias@icloud.com"}, session, reserveCause
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	service.autoCreateConfirmationDelays = nil
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if Code(err) != CodeAliasConfirmationPending || !errors.Is(err, ErrAliasConfirmationPending) {
		t.Fatalf("confirmation error = %v code=%q", err, Code(err))
	}
	var got *apple.Error
	if !errors.As(err, &got) || got != confirmationCause {
		t.Fatalf("latest confirmation cause = %#v, want %#v", got, confirmationCause)
	}
	if !errors.Is(err, reserveCause) {
		t.Fatalf("initial reserve cause was dropped: %v", err)
	}
}

func TestCreateAutoAliasExpiresSessionWithoutReenteringAccountLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	client := &fakeAppleClient{
		validate: func(context.Context, apple.Session) (apple.Session, error) {
			return apple.Session{}, apple.ErrInvalidSession
		},
	}
	locker := newFakeAcquiringLocker()
	service := newTestService(t, repo, client, locker, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, ErrSessionExpired) || Code(err) != CodeSessionExpired {
		t.Fatalf("expired session error = %v code=%q", err, Code(err))
	}
	if ctx.Err() != nil {
		t.Fatalf("session expiry deadlocked until the context ended: %v", ctx.Err())
	}
	if repo.sessionCount() != 0 {
		t.Fatal("expired session was not deleted while the account lock was held")
	}
}

func TestCreateAutoAliasKeepsStableExpiryCodeWhenSessionDeletionFails(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	repo.deleteSessionErr = errors.New("database unavailable")
	client := &fakeAppleClient{
		validate: func(context.Context, apple.Session) (apple.Session, error) {
			return apple.Session{}, apple.ErrInvalidSession
		},
	}
	service := newTestService(t, repo, client, newFakeAcquiringLocker(), func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, ErrSessionExpired) || Code(err) != CodeSessionExpired || err.Error() != CodeSessionExpired {
		t.Fatalf("session deletion failure lost stable expiry error: %v code=%q", err, Code(err))
	}
	if repo.sessionCount() != 1 {
		t.Fatal("failed session deletion unexpectedly removed the persisted session")
	}
}

func TestSyncAllowsEmptyDirectoryWhenForwardingMetadataOwnsMailbox(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com"}, now)
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{ForwardToEmails: []string{"PRIMARY@ICLOUD.COM"}}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
	result, err := service.SyncAliases(ctx, 3)
	if err != nil {
		t.Fatalf("sync empty directory: %v", err)
	}
	if result.Summary.Total != 0 || repo.imports.Load() != 1 {
		t.Fatalf("empty sync result = %#v imports=%d", result, repo.imports.Load())
	}
}

func TestSyncDetectsAccountChangeAfterNetworkBeforePersistence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com"}, now)
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			repo.setAccountEmail("changed@icloud.com")
			return apple.ListResult{
				ForwardToEmails: []string{"primary@icloud.com"},
				Aliases: []apple.Alias{{
					AnonymousID: "one", HME: "one@icloud.com", ForwardToEmail: "primary@icloud.com", IsActive: true,
				}},
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
	upsertsBefore := repo.upserts.Load()
	_, err := service.SyncAliases(ctx, 3)
	if !errors.Is(err, ErrAccountChanged) || Code(err) != CodeAccountChanged {
		t.Fatalf("account change error = %v code=%q", err, Code(err))
	}
	if repo.imports.Load() != 0 || repo.upserts.Load() != upsertsBefore {
		t.Fatalf("account change wrote state: imports=%d upserts=%d", repo.imports.Load(), repo.upserts.Load()-upsertsBefore)
	}
}

func TestSyncDetectsIMAPUsernameChangeAfterNetworkBeforePersistence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", IMAPUsername: "primary@icloud.com"}, now)
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			repo.setIMAPUsername("changed@icloud.com")
			return apple.ListResult{
				ForwardToEmails: []string{"primary@icloud.com"},
				Aliases: []apple.Alias{{
					AnonymousID: "one", HME: "one@icloud.com", ForwardToEmail: "primary@icloud.com", IsActive: true,
				}},
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
	upsertsBefore := repo.upserts.Load()
	_, err := service.SyncAliases(ctx, 3)
	if !errors.Is(err, ErrAccountChanged) || Code(err) != CodeAccountChanged {
		t.Fatalf("IMAP username change error = %v code=%q", err, Code(err))
	}
	if repo.imports.Load() != 0 || repo.upserts.Load() != upsertsBefore {
		t.Fatalf("IMAP username change wrote state: imports=%d upserts=%d", repo.imports.Load(), repo.upserts.Load()-upsertsBefore)
	}
}

func TestSyncExpiredSessionIsDeletedAndTyped(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com"}, now)
	client := &fakeAppleClient{validate: func(context.Context, apple.Session) (apple.Session, error) {
		return apple.Session{}, apple.ErrInvalidSession
	}}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
	_, err := service.SyncAliases(ctx, 3)
	if !errors.Is(err, ErrSessionExpired) || Code(err) != CodeSessionExpired {
		t.Fatalf("expired session error = %v code=%q", err, Code(err))
	}
	if repo.sessionCount() != 0 {
		t.Fatal("expired persisted session was retained")
	}
}

func TestSyncOwnershipConflictIsTypedAndDoesNotReturnKeys(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com"}, now)
	repo.importFn = func(context.Context, int64, []domain.AliasImportCandidate) (domain.AliasImportResult, error) {
		return domain.AliasImportResult{Conflicts: []domain.AliasImportConflict{{Address: "taken@icloud.com"}}}, store.ErrAliasOwnershipConflict
	}
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{
				ForwardToEmails: []string{"primary@icloud.com"},
				Aliases: []apple.Alias{{
					AnonymousID: "one", HME: "taken@icloud.com", ForwardToEmail: "primary@icloud.com", IsActive: true,
				}},
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
	result, err := service.SyncAliases(ctx, 3)
	if !errors.Is(err, ErrAliasOwnershipConflict) || Code(err) != CodeAliasOwnershipConflict {
		t.Fatalf("ownership conflict = %v code=%q", err, Code(err))
	}
	if len(result.Created) != 0 {
		t.Fatalf("conflict returned created credential bundles: %#v", result.Created)
	}
}

func TestDeleteAliasSynchronizesAppleBeforeLocalDeletion(t *testing.T) {
	tests := []struct {
		name             string
		remotePresent    bool
		remoteActive     bool
		wantEvents       string
		wantSession      string
		wantDeactivate   int32
		wantRemoteDelete int32
	}{
		{
			name:             "active alias is deactivated then permanently deleted",
			remotePresent:    true,
			remoteActive:     true,
			wantEvents:       "validate,list,deactivate,delete,local",
			wantSession:      "deleted-session",
			wantDeactivate:   1,
			wantRemoteDelete: 1,
		},
		{
			name:             "inactive alias skips deactivation",
			remotePresent:    true,
			wantEvents:       "validate,list,delete,local",
			wantSession:      "deleted-session",
			wantRemoteDelete: 1,
		},
		{
			name:        "remote absence is idempotent success",
			wantEvents:  "validate,list,local",
			wantSession: "listed-session",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
			repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
			repo.addAlias(domain.Alias{
				ID: 41, AccountID: 3, AccountEmail: "primary@icloud.com",
				Address: "alias@icloud.com", Enabled: true,
			})
			locker := &fakeLocker{}
			var events []string
			client := &fakeAppleClient{
				validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
					assertNetworkInsideAccountLock(t, locker)
					events = append(events, "validate")
					if session.SessionToken != "initial-session" {
						t.Fatalf("validation session token = %q", session.SessionToken)
					}
					session.SessionToken = "validated-session"
					return session, nil
				},
				list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
					assertNetworkInsideAccountLock(t, locker)
					events = append(events, "list")
					if session.SessionToken != "validated-session" {
						t.Fatalf("list session token = %q", session.SessionToken)
					}
					session.SessionToken = "listed-session"
					result := aliasDeletionDirectory()
					if test.remotePresent {
						result.Aliases = []apple.Alias{{
							AnonymousID: "remote-id", HME: "ALIAS@icloud.com",
							ForwardToEmail: "PRIMARY@icloud.com", IsActive: test.remoteActive,
						}}
					}
					return result, session, nil
				},
				deactivate: func(_ context.Context, session apple.Session, anonymousID string) (apple.Session, error) {
					assertNetworkInsideAccountLock(t, locker)
					events = append(events, "deactivate")
					if anonymousID != "remote-id" || session.SessionToken != "listed-session" {
						t.Fatalf("deactivate input: id=%q session=%#v", anonymousID, session)
					}
					session.SessionToken = "deactivated-session"
					return session, nil
				},
				deleteRemote: func(_ context.Context, session apple.Session, anonymousID string) (apple.Session, error) {
					assertNetworkInsideAccountLock(t, locker)
					events = append(events, "delete")
					wantToken := "listed-session"
					if test.remoteActive {
						wantToken = "deactivated-session"
					}
					if anonymousID != "remote-id" || session.SessionToken != wantToken {
						t.Fatalf("delete input: id=%q session=%#v want_token=%q", anonymousID, session, wantToken)
					}
					session.SessionToken = "deleted-session"
					return session, nil
				},
			}
			repo.deleteAliasFn = func(_ context.Context, id int64) error {
				events = append(events, "local")
				if id != 41 || locker.held.Load() == 0 {
					t.Fatalf("local deletion was not published under the account lock: id=%d held=%d", id, locker.held.Load())
				}
				return nil
			}
			service := newTestService(t, repo, client, locker, func() time.Time { return now })
			storeSession(t, service, repo, 3, apple.Session{
				AppleID: "owner@example.com", Region: apple.RegionGlobal, SessionToken: "initial-session",
			})

			if err := service.DeleteAlias(ctx, 41); err != nil {
				t.Fatalf("delete alias: %v", err)
			}
			if got := strings.Join(events, ","); got != test.wantEvents {
				t.Fatalf("operation order = %q, want %q", got, test.wantEvents)
			}
			if repo.hasAlias(41) || repo.aliasDeletes.Load() != 1 {
				t.Fatalf("local alias state: exists=%v deletes=%d", repo.hasAlias(41), repo.aliasDeletes.Load())
			}
			if client.deactivateCalls.Load() != test.wantDeactivate || client.deleteCalls.Load() != test.wantRemoteDelete {
				t.Fatalf("remote calls: deactivate=%d delete=%d", client.deactivateCalls.Load(), client.deleteCalls.Load())
			}
			assertStoredAppleSessionToken(t, service, repo, 3, test.wantSession)
		})
	}
}

func TestDeleteAliasRejectsPendingConfirmationBeforeApple(t *testing.T) {
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	repo.addAlias(domain.Alias{
		ID: 41, AccountID: 3, Address: "alias@icloud.com", Enabled: false,
		LastSyncError: "  " + domain.AppleAliasConfirmationPending + "  ",
	})
	client := &fakeAppleClient{validate: func(context.Context, apple.Session) (apple.Session, error) {
		t.Fatal("pending alias reached Apple validation")
		return apple.Session{}, nil
	}}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })

	err := service.DeleteAlias(context.Background(), 41)
	if !errors.Is(err, ErrAliasConfirmationPending) ||
		!errors.Is(err, store.ErrAliasConfirmationPending) ||
		Code(err) != CodeAliasConfirmationPending {
		t.Fatalf("pending alias deletion error = %v code=%q", err, Code(err))
	}
	if !repo.hasAlias(41) || repo.aliasDeletes.Load() != 0 ||
		client.deactivateCalls.Load() != 0 || client.deleteCalls.Load() != 0 {
		t.Fatal("pending alias deletion changed local or remote state")
	}
}

func TestDeleteAliasReconcilesAmbiguousDeactivateOutcome(t *testing.T) {
	tests := []struct {
		name              string
		reconciledPresent bool
		reconciledActive  bool
		wantError         bool
		wantLocalAlias    bool
		wantRemoteDelete  int32
		wantSession       string
	}{
		{
			name:        "alias disappeared",
			wantSession: "reconciled-session",
		},
		{
			name:              "alias became inactive",
			reconciledPresent: true,
			wantRemoteDelete:  1,
			wantSession:       "deleted-session",
		},
		{
			name:              "alias remained active",
			reconciledPresent: true,
			reconciledActive:  true,
			wantError:         true,
			wantLocalAlias:    true,
			wantSession:       "reconciled-session",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
			repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
			repo.addAlias(domain.Alias{ID: 41, AccountID: 3, Address: "alias@icloud.com", Enabled: true})
			var listCalls atomic.Int32
			client := &fakeAppleClient{
				validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
					session.SessionToken = "validated-session"
					return session, nil
				},
				list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
					call := listCalls.Add(1)
					result := aliasDeletionDirectory()
					switch call {
					case 1:
						if session.SessionToken != "validated-session" {
							t.Fatalf("initial list session token = %q", session.SessionToken)
						}
						result.Aliases = []apple.Alias{{
							AnonymousID: "initial-id", HME: "alias@icloud.com",
							ForwardToEmail: "primary@icloud.com", IsActive: true,
						}}
						session.SessionToken = "listed-session"
					case 2:
						if session.SessionToken != "ambiguous-deactivate-session" {
							t.Fatalf("reconciliation session token = %q", session.SessionToken)
						}
						if test.reconciledPresent {
							result.Aliases = []apple.Alias{{
								AnonymousID: "reconciled-id", HME: "alias@icloud.com",
								ForwardToEmail: "primary@icloud.com", IsActive: test.reconciledActive,
							}}
						}
						session.SessionToken = "reconciled-session"
					default:
						t.Fatalf("unexpected list call %d", call)
					}
					return result, session, nil
				},
				deactivate: func(_ context.Context, session apple.Session, anonymousID string) (apple.Session, error) {
					if anonymousID != "initial-id" || session.SessionToken != "listed-session" {
						t.Fatalf("deactivate input: id=%q session=%#v", anonymousID, session)
					}
					session.SessionToken = "ambiguous-deactivate-session"
					return session, ambiguousAliasMutationError("deactivate")
				},
				deleteRemote: func(_ context.Context, session apple.Session, anonymousID string) (apple.Session, error) {
					if anonymousID != "reconciled-id" || session.SessionToken != "reconciled-session" {
						t.Fatalf("delete after reconcile: id=%q session=%#v", anonymousID, session)
					}
					session.SessionToken = "deleted-session"
					return session, nil
				},
			}
			service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
			storeSession(t, service, repo, 3, apple.Session{
				AppleID: "owner@example.com", Region: apple.RegionGlobal, SessionToken: "initial-session",
			})

			err := service.DeleteAlias(ctx, 41)
			if test.wantError {
				if !errors.Is(err, ErrUpstream) || Code(err) != CodeUpstreamError {
					t.Fatalf("ambiguous deactivate error = %v code=%q", err, Code(err))
				}
			} else if err != nil {
				t.Fatalf("delete after ambiguous deactivate: %v", err)
			}
			if got := repo.hasAlias(41); got != test.wantLocalAlias {
				t.Fatalf("local alias exists = %v, want %v", got, test.wantLocalAlias)
			}
			if listCalls.Load() != 2 || client.deactivateCalls.Load() != 1 || client.deleteCalls.Load() != test.wantRemoteDelete {
				t.Fatalf("calls: list=%d deactivate=%d delete=%d",
					listCalls.Load(), client.deactivateCalls.Load(), client.deleteCalls.Load())
			}
			assertStoredAppleSessionToken(t, service, repo, 3, test.wantSession)
		})
	}
}

func TestDeleteAliasReconcilesAmbiguousPermanentDeleteOutcome(t *testing.T) {
	tests := []struct {
		name           string
		stillPresent   bool
		active         bool
		wantError      bool
		wantLocalAlias bool
	}{
		{name: "alias disappeared"},
		{name: "inactive alias remained", stillPresent: true, wantError: true, wantLocalAlias: true},
		{name: "active alias remained", stillPresent: true, active: true, wantError: true, wantLocalAlias: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
			repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
			repo.addAlias(domain.Alias{ID: 41, AccountID: 3, Address: "alias@icloud.com", Enabled: true})
			var listCalls atomic.Int32
			client := &fakeAppleClient{
				validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
					session.SessionToken = "validated-session"
					return session, nil
				},
				list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
					call := listCalls.Add(1)
					result := aliasDeletionDirectory()
					switch call {
					case 1:
						result.Aliases = []apple.Alias{{
							AnonymousID: "remote-id", HME: "alias@icloud.com",
							ForwardToEmail: "primary@icloud.com", IsActive: false,
						}}
						session.SessionToken = "listed-session"
					case 2:
						if session.SessionToken != "ambiguous-delete-session" {
							t.Fatalf("delete reconciliation session token = %q", session.SessionToken)
						}
						if test.stillPresent {
							result.Aliases = []apple.Alias{{
								AnonymousID: "remote-id", HME: "alias@icloud.com",
								ForwardToEmail: "primary@icloud.com", IsActive: test.active,
							}}
						}
						session.SessionToken = "reconciled-session"
					default:
						t.Fatalf("unexpected list call %d", call)
					}
					return result, session, nil
				},
				deleteRemote: func(_ context.Context, session apple.Session, anonymousID string) (apple.Session, error) {
					if anonymousID != "remote-id" || session.SessionToken != "listed-session" {
						t.Fatalf("delete input: id=%q session=%#v", anonymousID, session)
					}
					session.SessionToken = "ambiguous-delete-session"
					return session, ambiguousAliasMutationError("delete")
				},
			}
			service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
			storeSession(t, service, repo, 3, apple.Session{
				AppleID: "owner@example.com", Region: apple.RegionGlobal, SessionToken: "initial-session",
			})

			err := service.DeleteAlias(ctx, 41)
			if test.wantError {
				if !errors.Is(err, ErrUpstream) || Code(err) != CodeUpstreamError {
					t.Fatalf("ambiguous permanent delete error = %v code=%q", err, Code(err))
				}
			} else if err != nil {
				t.Fatalf("delete after reconciliation: %v", err)
			}
			if got := repo.hasAlias(41); got != test.wantLocalAlias {
				t.Fatalf("local alias exists = %v, want %v", got, test.wantLocalAlias)
			}
			if listCalls.Load() != 2 || client.deactivateCalls.Load() != 0 || client.deleteCalls.Load() != 1 {
				t.Fatalf("calls: list=%d deactivate=%d delete=%d",
					listCalls.Load(), client.deactivateCalls.Load(), client.deleteCalls.Load())
			}
			assertStoredAppleSessionToken(t, service, repo, 3, "reconciled-session")
		})
	}
}

func TestDeleteAliasRejectsTargetForwardedToAnotherMailbox(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	repo.addAlias(domain.Alias{ID: 41, AccountID: 3, Address: "alias@icloud.com", Enabled: true})
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{
				SelectedForwardTo: "primary@icloud.com",
				ForwardToEmails:   []string{"primary@icloud.com", "other@example.com"},
				Aliases: []apple.Alias{
					{AnonymousID: "target-id", HME: "alias@icloud.com", ForwardToEmail: "other@example.com", IsActive: true},
					{AnonymousID: "owned-id", HME: "owned@icloud.com", ForwardToEmail: "primary@icloud.com", IsActive: true},
				},
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	err := service.DeleteAlias(ctx, 41)
	if !errors.Is(err, ErrAccountMismatch) || Code(err) != CodeAccountMismatch {
		t.Fatalf("forwarding mismatch error = %v code=%q", err, Code(err))
	}
	if !repo.hasAlias(41) || repo.aliasDeletes.Load() != 0 ||
		client.deactivateCalls.Load() != 0 || client.deleteCalls.Load() != 0 {
		t.Fatalf("forwarding mismatch caused deletion: exists=%v local=%d deactivate=%d delete=%d",
			repo.hasAlias(41), repo.aliasDeletes.Load(), client.deactivateCalls.Load(), client.deleteCalls.Load())
	}
}

func TestDeleteAliasRequiresUsableAppleSession(t *testing.T) {
	t.Run("login required", func(t *testing.T) {
		now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
		repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
		repo.addAlias(domain.Alias{ID: 41, AccountID: 3, Address: "alias@icloud.com", Enabled: true})
		client := &fakeAppleClient{validate: func(context.Context, apple.Session) (apple.Session, error) {
			t.Fatal("missing session reached Apple validation")
			return apple.Session{}, nil
		}}
		service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })

		err := service.DeleteAlias(context.Background(), 41)
		if !errors.Is(err, ErrLoginRequired) || Code(err) != CodeLoginRequired {
			t.Fatalf("missing session error = %v code=%q", err, Code(err))
		}
		if !repo.hasAlias(41) || repo.aliasDeletes.Load() != 0 {
			t.Fatal("missing Apple login deleted the local alias")
		}
	})

	t.Run("expired session is removed", func(t *testing.T) {
		now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
		repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
		repo.addAlias(domain.Alias{ID: 41, AccountID: 3, Address: "alias@icloud.com", Enabled: true})
		client := &fakeAppleClient{validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			session.SessionToken = "expired-rotated-session"
			return session, apple.ErrInvalidSession
		}}
		service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
		storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

		err := service.DeleteAlias(context.Background(), 41)
		if !errors.Is(err, ErrSessionExpired) || Code(err) != CodeSessionExpired {
			t.Fatalf("expired session error = %v code=%q", err, Code(err))
		}
		if !repo.hasAlias(41) || repo.aliasDeletes.Load() != 0 || repo.sessionCount() != 0 {
			t.Fatalf("expired session cleanup: alias_exists=%v local_deletes=%d sessions=%d",
				repo.hasAlias(41), repo.aliasDeletes.Load(), repo.sessionCount())
		}
	})
}

func TestDeleteAliasSerializesOperationsForTheSameAccount(t *testing.T) {
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	repo.addAlias(domain.Alias{ID: 41, AccountID: 3, Address: "one@icloud.com", Enabled: true})
	repo.addAlias(domain.Alias{ID: 42, AccountID: 3, Address: "two@icloud.com", Enabled: true})
	secondRead := make(chan struct{})
	var secondReadOnce sync.Once
	repo.getAliasFn = func(_ context.Context, id int64) error {
		if id == 42 {
			secondReadOnce.Do(func() { close(secondRead) })
		}
		return nil
	}
	firstListed := make(chan struct{})
	releaseFirst := make(chan struct{})
	var validateCalls atomic.Int32
	var listCalls atomic.Int32
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			validateCalls.Add(1)
			return session, nil
		},
		list: func(ctx context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			if listCalls.Add(1) == 1 {
				close(firstListed)
				select {
				case <-releaseFirst:
				case <-ctx.Done():
					return apple.ListResult{}, session, ctx.Err()
				}
			}
			return aliasDeletionDirectory(), session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	firstContext, cancelFirst := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() { firstDone <- service.DeleteAlias(firstContext, 41) }()
	select {
	case <-firstListed:
	case <-firstContext.Done():
		t.Fatalf("first deletion did not reach Apple list: %v", firstContext.Err())
	}

	secondContext, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() { secondDone <- service.DeleteAlias(secondContext, 42) }()
	select {
	case <-secondRead:
	case <-firstContext.Done():
		t.Fatalf("second deletion did not reach the operation lock: %v", firstContext.Err())
	}
	cancelSecond()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked deletion error = %v, want context.Canceled", err)
	}
	if validateCalls.Load() != 1 || listCalls.Load() != 1 || !repo.hasAlias(42) {
		t.Fatalf("same-account operation overlapped: validates=%d lists=%d second_exists=%v",
			validateCalls.Load(), listCalls.Load(), repo.hasAlias(42))
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first deletion: %v", err)
	}
	if repo.hasAlias(41) {
		t.Fatal("first deletion did not remove its local alias")
	}
}

func TestFilterAliasesRejectsDuplicateAddress(t *testing.T) {
	_, _, err := filterAliases(apple.ListResult{
		ForwardToEmails: []string{"primary@icloud.com"},
		Aliases: []apple.Alias{
			{HME: "DUP@icloud.com", AnonymousID: "one", ForwardToEmail: "primary@icloud.com"},
			{HME: "dup@icloud.com", AnonymousID: "two", ForwardToEmail: "primary@icloud.com"},
		},
	}, "primary@icloud.com")
	if !errors.Is(err, ErrUpstream) || Code(err) != CodeUpstreamError {
		t.Fatalf("duplicate directory error = %v code=%q", err, Code(err))
	}
}

type fakeAppleClient struct {
	signIn       func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error)
	verify       func(context.Context, apple.Session, string) (apple.Session, error)
	validate     func(context.Context, apple.Session) (apple.Session, error)
	list         func(context.Context, apple.Session) (apple.ListResult, apple.Session, error)
	update       func(context.Context, apple.Session, string) (apple.Session, error)
	create       func(context.Context, apple.Session, string, string) (apple.Alias, apple.Session, error)
	deactivate   func(context.Context, apple.Session, string) (apple.Session, error)
	deleteRemote func(context.Context, apple.Session, string) (apple.Session, error)

	verifyCalls     atomic.Int32
	updateCalls     atomic.Int32
	createCalls     atomic.Int32
	deactivateCalls atomic.Int32
	deleteCalls     atomic.Int32
}

func (c *fakeAppleClient) SignIn(ctx context.Context, appleID, password string, region apple.Region, previous *apple.Session) (apple.Session, bool, error) {
	if c.signIn == nil {
		panic("unexpected SignIn")
	}
	return c.signIn(ctx, appleID, password, region, previous)
}

func (c *fakeAppleClient) VerifyCode(ctx context.Context, session apple.Session, code string) (apple.Session, error) {
	c.verifyCalls.Add(1)
	if c.verify == nil {
		panic("unexpected VerifyCode")
	}
	return c.verify(ctx, session, code)
}

func (c *fakeAppleClient) Validate(ctx context.Context, session apple.Session) (apple.Session, error) {
	if c.validate == nil {
		panic("unexpected Validate")
	}
	return c.validate(ctx, session)
}

func (c *fakeAppleClient) ListAliases(ctx context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
	if c.list == nil {
		panic("unexpected ListAliases")
	}
	return c.list(ctx, session)
}

func (c *fakeAppleClient) UpdateForwardTo(ctx context.Context, session apple.Session, forwardToEmail string) (apple.Session, error) {
	c.updateCalls.Add(1)
	if c.update == nil {
		panic("unexpected UpdateForwardTo")
	}
	return c.update(ctx, session, forwardToEmail)
}

func (c *fakeAppleClient) CreateAlias(ctx context.Context, session apple.Session, label, note string) (apple.Alias, apple.Session, error) {
	c.createCalls.Add(1)
	if c.create == nil {
		panic("unexpected CreateAlias")
	}
	return c.create(ctx, session, label, note)
}

func (c *fakeAppleClient) DeactivateAlias(ctx context.Context, session apple.Session, anonymousID string) (apple.Session, error) {
	c.deactivateCalls.Add(1)
	if c.deactivate == nil {
		panic("unexpected DeactivateAlias")
	}
	return c.deactivate(ctx, session, anonymousID)
}

func (c *fakeAppleClient) DeleteAlias(ctx context.Context, session apple.Session, anonymousID string) (apple.Session, error) {
	c.deleteCalls.Add(1)
	if c.deleteRemote == nil {
		panic("unexpected DeleteAlias")
	}
	return c.deleteRemote(ctx, session, anonymousID)
}

type fakeLocker struct {
	held atomic.Int32
}

func (l *fakeLocker) WithAccountLock(_ context.Context, _ int64, operation func() error) error {
	l.held.Add(1)
	defer l.held.Add(-1)
	return operation()
}

func assertNetworkOutsideAccountLock(t *testing.T, locker *fakeLocker) {
	t.Helper()
	if locker.held.Load() != 0 {
		t.Fatal("Apple network request ran while the account lock was held")
	}
}

func assertNetworkInsideAccountLock(t *testing.T, locker *fakeLocker) {
	t.Helper()
	if locker.held.Load() == 0 {
		t.Fatal("Apple deletion request ran outside the account lock")
	}
}

type fakeAcquiringLocker struct {
	token chan struct{}
}

func newFakeAcquiringLocker() *fakeAcquiringLocker {
	locker := &fakeAcquiringLocker{token: make(chan struct{}, 1)}
	locker.token <- struct{}{}
	return locker
}

func (l *fakeAcquiringLocker) AcquireAccountLock(ctx context.Context, _ int64) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.token:
		return func() { l.token <- struct{}{} }, nil
	}
}

func (l *fakeAcquiringLocker) WithAccountLock(ctx context.Context, accountID int64, operation func() error) error {
	release, err := l.AcquireAccountLock(ctx, accountID)
	if err != nil {
		return err
	}
	defer release()
	return operation()
}

type fakeRepository struct {
	mu               sync.Mutex
	account          domain.Account
	sessions         map[int64]domain.AppleWebSession
	aliases          map[int64]domain.Alias
	pending          *domain.Alias
	now              time.Time
	deleteSessionErr error
	deleteAliasErr   error
	createErr        error
	confirmErr       error
	upsertSessionErr error
	getAliasFn       func(context.Context, int64) error
	deleteAliasFn    func(context.Context, int64) error
	importFn         func(context.Context, int64, []domain.AliasImportCandidate) (domain.AliasImportResult, error)
	upserts          atomic.Int32
	imports          atomic.Int32
	creates          atomic.Int32
	confirms         atomic.Int32
	aliasDeletes     atomic.Int32
}

func newFakeRepository(account domain.Account, now time.Time) *fakeRepository {
	return &fakeRepository{
		account:  account,
		sessions: make(map[int64]domain.AppleWebSession),
		aliases:  make(map[int64]domain.Alias),
		now:      now,
	}
}

func (r *fakeRepository) GetAccount(_ context.Context, id int64) (domain.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.account.ID != id {
		return domain.Account{}, sql.ErrNoRows
	}
	return r.account, nil
}

func (r *fakeRepository) GetAppleWebSession(_ context.Context, accountID int64) (domain.AppleWebSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[accountID]
	if !ok {
		return domain.AppleWebSession{}, sql.ErrNoRows
	}
	return session, nil
}

func (r *fakeRepository) UpsertAppleWebSession(_ context.Context, session domain.AppleWebSession) (domain.AppleWebSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upserts.Add(1)
	if r.upsertSessionErr != nil {
		return domain.AppleWebSession{}, r.upsertSessionErr
	}
	if existing, ok := r.sessions[session.AccountID]; ok {
		session.CreatedAt = existing.CreatedAt
	} else {
		session.CreatedAt = r.now
	}
	session.UpdatedAt = r.now
	r.sessions[session.AccountID] = session
	return session, nil
}

func (r *fakeRepository) DeleteAppleWebSession(_ context.Context, accountID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleteSessionErr != nil {
		return r.deleteSessionErr
	}
	if _, ok := r.sessions[accountID]; !ok {
		return sql.ErrNoRows
	}
	delete(r.sessions, accountID)
	return nil
}

func (r *fakeRepository) GetAlias(ctx context.Context, id int64) (domain.Alias, error) {
	if r.getAliasFn != nil {
		if err := r.getAliasFn(ctx, id); err != nil {
			return domain.Alias{}, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	alias, ok := r.aliases[id]
	if !ok {
		return domain.Alias{}, store.ErrNotFound
	}
	return alias, nil
}

func (r *fakeRepository) DeleteAlias(ctx context.Context, id int64) error {
	if r.deleteAliasFn != nil {
		if err := r.deleteAliasFn(ctx, id); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleteAliasErr != nil {
		return r.deleteAliasErr
	}
	if _, ok := r.aliases[id]; !ok {
		return store.ErrNotFound
	}
	delete(r.aliases, id)
	r.aliasDeletes.Add(1)
	return nil
}

func (r *fakeRepository) ImportAliases(ctx context.Context, accountID int64, candidates []domain.AliasImportCandidate) (domain.AliasImportResult, error) {
	r.imports.Add(1)
	if r.importFn != nil {
		return r.importFn(ctx, accountID, candidates)
	}
	return domain.AliasImportResult{}, nil
}

func (r *fakeRepository) CountEnabledAliasesByAccount(context.Context, int64) (int, error) {
	return 0, nil
}

func (r *fakeRepository) CreateAliasWithPendingAPIKey(
	ctx context.Context,
	session domain.AppleWebSession,
	alias domain.Alias,
	apiKeyCiphertext string,
) (domain.Alias, domain.AppleWebSession, error) {
	if strings.TrimSpace(apiKeyCiphertext) == "" || len(alias.APIKeyHash) != 32 || alias.APIKeyPrefix == "" {
		return domain.Alias{}, domain.AppleWebSession{}, errors.New("invalid automatic alias key publication")
	}
	return r.createAutoAliasCandidate(ctx, session, alias)
}

func (r *fakeRepository) createAutoAliasCandidate(
	ctx context.Context,
	session domain.AppleWebSession,
	alias domain.Alias,
) (domain.Alias, domain.AppleWebSession, error) {
	if err := ctx.Err(); err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, err
	}
	r.creates.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return domain.Alias{}, domain.AppleWebSession{}, r.createErr
	}
	if alias.AccountID != r.account.ID || strings.TrimSpace(alias.Address) == "" {
		return domain.Alias{}, domain.AppleWebSession{}, errors.New("invalid automatic alias publication")
	}
	alias.ID = 100 + int64(r.creates.Load())
	alias.CredentialVersion = 1
	r.sessions[session.AccountID] = session
	r.pending = &alias
	return alias, session, nil
}

func (r *fakeRepository) GetPendingAutoAliasConfirmation(_ context.Context, accountID int64) (domain.PendingAliasAPIKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil || r.pending.AccountID != accountID ||
		r.pending.Enabled || r.pending.LastSyncError != domain.AppleAliasConfirmationPending {
		return domain.PendingAliasAPIKey{}, store.ErrNotFound
	}
	return domain.PendingAliasAPIKey{Alias: *r.pending}, nil
}

func (r *fakeRepository) ConfirmPendingAutoAlias(
	ctx context.Context,
	session domain.AppleWebSession,
	aliasID int64,
) (domain.Alias, domain.AppleWebSession, error) {
	if err := ctx.Err(); err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, err
	}
	if r.confirmErr != nil {
		return domain.Alias{}, domain.AppleWebSession{}, r.confirmErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil || r.pending.ID != aliasID || r.pending.Enabled ||
		r.pending.LastSyncError != domain.AppleAliasConfirmationPending {
		return domain.Alias{}, domain.AppleWebSession{}, store.ErrNotFound
	}
	r.confirms.Add(1)
	r.pending.Enabled = true
	r.pending.LastSyncStatus = domain.SyncStatusPending
	r.pending.LastSyncError = ""
	r.sessions[session.AccountID] = session
	return *r.pending, session, nil
}

func (r *fakeRepository) setAccountEmail(email string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.account.Email = email
}

func (r *fakeRepository) setIMAPUsername(username string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.account.IMAPUsername = username
}

func (r *fakeRepository) addAlias(alias domain.Alias) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aliases[alias.ID] = alias
}

func (r *fakeRepository) hasAlias(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.aliases[id]
	return ok
}

func (r *fakeRepository) sessionCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

func (r *fakeRepository) mustSession(t *testing.T, accountID int64) domain.AppleWebSession {
	t.Helper()
	session, err := r.GetAppleWebSession(context.Background(), accountID)
	if err != nil {
		t.Fatalf("get stored Apple session: %v", err)
	}
	return session
}

func newTestService(t *testing.T, repo Repository, client AppleClient, locker AccountLocker, now func() time.Time) *Service {
	t.Helper()
	cipher, err := secure.NewCipher([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatalf("create cipher: %v", err)
	}
	service, err := New(repo, cipher, client, locker, WithClock(now))
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	return service
}

func storeSession(t *testing.T, service *Service, repo *fakeRepository, accountID int64, session apple.Session) {
	t.Helper()
	if strings.TrimSpace(session.DSID) == "" {
		session.DSID = "test-dsid"
	}
	storeSessionExactly(t, service, repo, accountID, session)
}

func storeSessionExactly(t *testing.T, service *Service, repo *fakeRepository, accountID int64, session apple.Session) {
	t.Helper()
	if session.ValidatedAt.IsZero() {
		session.ValidatedAt = repo.now
	}
	payload, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	ciphertext, err := service.cipher.EncryptAppleSession(string(payload))
	if err != nil {
		t.Fatalf("encrypt session: %v", err)
	}
	_, err = repo.UpsertAppleWebSession(context.Background(), domain.AppleWebSession{
		AccountID:       accountID,
		Ciphertext:      ciphertext,
		AppleID:         session.AppleID,
		Region:          publicRegion(string(session.Region)),
		Authenticated:   true,
		LastValidatedAt: &session.ValidatedAt,
	})
	if err != nil {
		t.Fatalf("store session: %v", err)
	}
}

func aliasDeletionDirectory() apple.ListResult {
	return apple.ListResult{
		SelectedForwardTo: "primary@icloud.com",
		ForwardToEmails:   []string{"primary@icloud.com"},
	}
}

func ambiguousAliasMutationError(operation string) error {
	return &apple.Error{
		Op:         operation + " Hide My Email alias",
		Kind:       apple.ErrService,
		StatusCode: 503,
		Retryable:  false,
		Err:        errors.New("response lost after request started"),
	}
}

func assertStoredAppleSessionToken(
	t *testing.T,
	service *Service,
	repo *fakeRepository,
	accountID int64,
	want string,
) {
	t.Helper()
	stored, err := service.decryptSession(repo.mustSession(t, accountID))
	if err != nil {
		t.Fatalf("decrypt stored Apple session: %v", err)
	}
	if stored.SessionToken != want {
		t.Fatalf("stored Apple session token = %q, want %q", stored.SessionToken, want)
	}
}

package hmesync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

func TestSyncFiltersByForwardingMailboxAndReturnsOnlyNewKeys(t *testing.T) {
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
		if len(candidates[0].APIKeyHash) != 32 || candidates[0].APIKeyPrefix == "" {
			t.Fatalf("new candidate is missing API key material: %#v", candidates[0])
		}
		return domain.AliasImportResult{
			Created:               []domain.Alias{{ID: 10, AccountID: accountID, Address: candidates[0].Address, APIKeyHash: candidates[0].APIKeyHash}},
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
	if len(result.Created) != 1 || result.Created[0].Alias.Address != "new@icloud.com" || result.Created[0].APIKey == "" {
		t.Fatalf("created credentials = %#v", result.Created)
	}
	if !secure.HashEqual(secure.HashToken(result.Created[0].APIKey), result.Created[0].Alias.APIKeyHash) {
		t.Fatal("returned raw API key does not match the imported hash")
	}
	if strings.Contains(repo.mustSession(t, 3).Ciphertext, result.Created[0].APIKey) {
		t.Fatal("raw API key leaked into the Apple session record")
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
	ctx := context.Background()
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
	if listCalls.Load() != 2 || client.createCalls.Load() != 1 || repo.creates.Load() != 0 {
		t.Fatalf("post-reserve mismatch calls: lists=%d reserves=%d writes=%d", listCalls.Load(), client.createCalls.Load(), repo.creates.Load())
	}
}

func TestCreateAutoAliasPreservesRotatedSessionWhenReserveFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			session.SessionToken = "preflight-session-token"
			return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			session.SessionToken = "rotated-during-reserve"
			return apple.Alias{}, session, errors.New("ambiguous reserve failure")
		},
	}
	service := newTestService(t, repo, client, newFakeAcquiringLocker(), func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, ErrUpstream) || Code(err) != CodeUpstreamError {
		t.Fatalf("reserve failure error = %v code=%q", err, Code(err))
	}
	if ctx.Err() != nil {
		t.Fatalf("reserve failure checkpoint re-entered the account lock: %v", ctx.Err())
	}
	stored, decryptErr := service.decryptSession(repo.mustSession(t, 3))
	if decryptErr != nil || stored.SessionToken != "rotated-during-reserve" {
		t.Fatalf("reserve failure lost rotated session: session=%#v err=%v", stored, decryptErr)
	}
	if client.createCalls.Load() != 1 || repo.creates.Load() != 0 {
		t.Fatalf("reserve failure calls: reserves=%d writes=%d", client.createCalls.Load(), repo.creates.Load())
	}
}

func TestCreateAutoAliasRejectsMissingSelectedForwardBeforeReserve(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{ForwardToEmails: []string{"primary@icloud.com"}}, session, nil
		},
		create: func(context.Context, apple.Session, string, string) (apple.Alias, apple.Session, error) {
			t.Fatal("missing selected forwarding target reached Apple reserve")
			return apple.Alias{}, apple.Session{}, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})

	_, err := service.CreateAutoAlias(ctx, 3)
	if !errors.Is(err, ErrUpstream) || Code(err) != CodeUpstreamError {
		t.Fatalf("missing selected target error = %v code=%q", err, Code(err))
	}
	if client.createCalls.Load() != 0 || repo.creates.Load() != 0 {
		t.Fatalf("missing target caused side effects: reserves=%d writes=%d", client.createCalls.Load(), repo.creates.Load())
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
	if client.createCalls.Load() != 1 || repo.creates.Load() != 0 {
		t.Fatalf("reserve mismatch side effects: reserves=%d writes=%d", client.createCalls.Load(), repo.creates.Load())
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
	if !errors.Is(err, ErrUpstream) || Code(err) != CodeUpstreamError {
		t.Fatalf("inactive reserve error = %v code=%q", err, Code(err))
	}
	if client.createCalls.Load() != 1 || repo.creates.Load() != 0 {
		t.Fatalf("inactive reserve side effects: reserves=%d writes=%d", client.createCalls.Load(), repo.creates.Load())
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
		t.Fatalf("conflict returned one-time keys: %#v", result.Created)
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
	signIn   func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error)
	verify   func(context.Context, apple.Session, string) (apple.Session, error)
	validate func(context.Context, apple.Session) (apple.Session, error)
	list     func(context.Context, apple.Session) (apple.ListResult, apple.Session, error)
	create   func(context.Context, apple.Session, string, string) (apple.Alias, apple.Session, error)

	verifyCalls atomic.Int32
	createCalls atomic.Int32
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

func (c *fakeAppleClient) CreateAlias(ctx context.Context, session apple.Session, label, note string) (apple.Alias, apple.Session, error) {
	c.createCalls.Add(1)
	if c.create == nil {
		panic("unexpected CreateAlias")
	}
	return c.create(ctx, session, label, note)
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
	now              time.Time
	deleteSessionErr error
	importFn         func(context.Context, int64, []domain.AliasImportCandidate) (domain.AliasImportResult, error)
	upserts          atomic.Int32
	imports          atomic.Int32
	creates          atomic.Int32
}

func newFakeRepository(account domain.Account, now time.Time) *fakeRepository {
	return &fakeRepository{
		account:  account,
		sessions: make(map[int64]domain.AppleWebSession),
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
	_ context.Context,
	session domain.AppleWebSession,
	alias domain.Alias,
	apiKeyCiphertext string,
) (domain.Alias, domain.AppleWebSession, error) {
	r.creates.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if alias.AccountID != r.account.ID || len(alias.APIKeyHash) != 32 ||
		alias.APIKeyPrefix == "" || strings.TrimSpace(apiKeyCiphertext) == "" {
		return domain.Alias{}, domain.AppleWebSession{}, errors.New("invalid automatic alias publication")
	}
	alias.ID = 100 + int64(r.creates.Load())
	r.sessions[session.AccountID] = session
	return alias, session, nil
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

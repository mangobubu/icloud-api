package hmesync

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

func TestCreateAutoAliasReportsPendingDirectoryReadAsReconciliation(t *testing.T) {
	now := testAutoCreateDiagnosticNow()
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	repo.pending = &domain.Alias{
		ID:                77,
		AccountID:         3,
		Address:           "pending@icloud.com",
		Enabled:           false,
		LastSyncError:     domain.AppleAliasConfirmationPending,
		LastSyncStatus:    domain.SyncStatusPending,
		CredentialVersion: 1,
	}
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{}, session, &apple.Error{
				Kind:       apple.ErrService,
				StatusCode: http.StatusServiceUnavailable,
				Retryable:  true,
			}
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{
		AppleID:      "owner@example.com",
		Region:       apple.RegionGlobal,
		SessionToken: "session-token",
	})
	var progress []domain.AliasCreationProgressUpdate
	ctx := domain.WithAliasCreationProgressReporter(context.Background(), func(update domain.AliasCreationProgressUpdate) {
		progress = append(progress, update)
	})

	_, err := service.CreateAutoAlias(ctx, 3)
	if Code(err) != CodeAliasConfirmationPending || !errors.Is(err, ErrAliasConfirmationPending) {
		t.Fatalf("pending directory read error = %v code=%q", err, Code(err))
	}
	if containsAliasCreationPhase(progress, domain.AliasCreationPhaseCheckingForwarding) {
		t.Fatalf("pending reconciliation reported forwarding preflight: %#v", progress)
	}
	if !containsAliasCreationPhase(progress, domain.AliasCreationPhaseReconciling) {
		t.Fatalf("pending reconciliation did not report reconciling: %#v", progress)
	}
	if progress[len(progress)-1].Phase != domain.AliasCreationPhaseFailed {
		t.Fatalf("terminal progress = %#v, want failed", progress[len(progress)-1])
	}
}

func TestCreateAutoAliasPreservesAccountActionDuringConfirmation(t *testing.T) {
	now := testAutoCreateDiagnosticNow()
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	listCalls := 0
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			listCalls++
			if listCalls == 1 {
				return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
			}
			return apple.ListResult{}, session, apple.ErrTermsRequired
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			return apple.Alias{HME: "new-alias@icloud.com", IsActive: true}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	service.autoCreateConfirmationDelays = nil
	storeSession(t, service, repo, 3, apple.Session{
		AppleID:      "owner@example.com",
		Region:       apple.RegionGlobal,
		SessionToken: "session-token",
	})
	var progress []domain.AliasCreationProgressUpdate
	ctx := domain.WithAliasCreationProgressReporter(context.Background(), func(update domain.AliasCreationProgressUpdate) {
		progress = append(progress, update)
	})

	_, err := service.CreateAutoAlias(ctx, 3)
	if Code(err) != CodeAccountActionRequired || !errors.Is(err, ErrAccountActionRequired) {
		t.Fatalf("confirmation account-action error = %v code=%q", err, Code(err))
	}
	if errors.Is(err, ErrAliasConfirmationPending) {
		t.Fatalf("account-action error was relabeled pending: %v", err)
	}
	if !containsAliasCreationPhase(progress, domain.AliasCreationPhaseReconciling) {
		t.Fatalf("confirmation reconciliation stage missing: %#v", progress)
	}
	if progress[len(progress)-1].Phase != domain.AliasCreationPhaseFailed {
		t.Fatalf("terminal progress = %#v, want failed", progress[len(progress)-1])
	}
}

func TestCreateAutoAliasReportsReserveValidationFailureDuringConfirmation(t *testing.T) {
	now := testAutoCreateDiagnosticNow()
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
	storeSession(t, service, repo, 3, apple.Session{
		AppleID:      "owner@example.com",
		Region:       apple.RegionGlobal,
		SessionToken: "session-token",
	})
	var progress []domain.AliasCreationProgressUpdate
	ctx := domain.WithAliasCreationProgressReporter(context.Background(), func(update domain.AliasCreationProgressUpdate) {
		progress = append(progress, update)
	})

	_, err := service.CreateAutoAlias(ctx, 3)
	if Code(err) != CodeAliasConfirmationPending || !errors.Is(err, ErrAliasConfirmationPending) {
		t.Fatalf("inactive reserve error = %v code=%q", err, Code(err))
	}
	if !containsAliasCreationPhase(progress, domain.AliasCreationPhaseConfirming) {
		t.Fatalf("reserve validation did not report confirming: %#v", progress)
	}
}

func TestCreateAutoAliasMarksUntrackedRemoteSideEffectWhenCandidatePersistenceFails(t *testing.T) {
	now := testAutoCreateDiagnosticNow()
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	repo.createErr = errors.New("candidate transaction fixture failed")
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
		},
		create: func(_ context.Context, session apple.Session, label, _ string) (apple.Alias, apple.Session, error) {
			return apple.Alias{
				HME:            "reserved@icloud.com",
				Label:          label,
				ForwardToEmail: "primary@icloud.com",
				IsActive:       true,
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{
		AppleID:      "owner@example.com",
		Region:       apple.RegionGlobal,
		SessionToken: "session-token",
	})

	_, err := service.CreateAutoAlias(context.Background(), 3)
	if Code(err) != CodePersistenceError || !errors.Is(err, ErrPersistence) {
		t.Fatalf("candidate persistence error = %v code=%q", err, Code(err))
	}
	var remote interface{ RemoteSideEffectPossible() bool }
	if !errors.As(err, &remote) || !remote.RemoteSideEffectPossible() {
		t.Fatalf("candidate persistence error lacks remote side-effect marker: %T", err)
	}
	var pending interface{ PendingConfirmation() bool }
	if errors.As(err, &pending) && pending.PendingConfirmation() {
		t.Fatal("rolled-back candidate was incorrectly marked as pending confirmation")
	}
	if client.createCalls.Load() != 1 || repo.creates.Load() != 1 || repo.pending != nil {
		t.Fatalf("reserve/write state: reserves=%d writes=%d pending=%#v", client.createCalls.Load(), repo.creates.Load(), repo.pending)
	}
}

func TestCreateAutoAliasClassifiesForwardingCheckpointPersistenceFailure(t *testing.T) {
	now := testAutoCreateDiagnosticNow()
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
		},
		create: func(context.Context, apple.Session, string, string) (apple.Alias, apple.Session, error) {
			t.Fatal("failed session checkpoint reached Apple reserve")
			return apple.Alias{}, apple.Session{}, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{
		AppleID:      "owner@example.com",
		Region:       apple.RegionGlobal,
		SessionToken: "session-token",
	})
	repo.upsertSessionErr = errors.New("database checkpoint fixture failed")

	_, err := service.CreateAutoAlias(context.Background(), 3)
	if Code(err) != CodePersistenceError || !errors.Is(err, ErrPersistence) {
		t.Fatalf("forwarding checkpoint error = %v code=%q", err, Code(err))
	}
	var remote interface{ RemoteSideEffectPossible() bool }
	if errors.As(err, &remote) && remote.RemoteSideEffectPossible() {
		t.Fatal("pre-reserve checkpoint failure was marked as a remote side effect")
	}
	if client.createCalls.Load() != 0 {
		t.Fatalf("pre-reserve checkpoint failure reserve calls = %d", client.createCalls.Load())
	}
}

func containsAliasCreationPhase(progress []domain.AliasCreationProgressUpdate, want domain.AliasCreationPhase) bool {
	for _, update := range progress {
		if update.Phase == want {
			return true
		}
	}
	return false
}

func testAutoCreateDiagnosticNow() time.Time {
	return time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
}

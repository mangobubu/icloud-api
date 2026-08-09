package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

type fakeHMESyncService struct {
	startAuth   func(context.Context, int64, int64, string, string, apple.Region) (hmesync.AuthResult, error)
	verifyAuth  func(context.Context, int64, int64, string, string) (hmesync.AuthResult, error)
	getSession  func(context.Context, int64) (hmesync.SessionInfo, error)
	clearAuth   func(context.Context, int64) error
	syncAliases func(context.Context, int64) (hmesync.SyncResult, error)
}

func (f *fakeHMESyncService) StartAuth(
	ctx context.Context,
	ownerAdminID, accountID int64,
	appleID, password string,
	region apple.Region,
) (hmesync.AuthResult, error) {
	if f.startAuth == nil {
		return hmesync.AuthResult{}, errors.New("unexpected StartAuth call")
	}
	return f.startAuth(ctx, ownerAdminID, accountID, appleID, password, region)
}

func (f *fakeHMESyncService) VerifyAuth(
	ctx context.Context,
	ownerAdminID, accountID int64,
	challengeID, code string,
) (hmesync.AuthResult, error) {
	if f.verifyAuth == nil {
		return hmesync.AuthResult{}, errors.New("unexpected VerifyAuth call")
	}
	return f.verifyAuth(ctx, ownerAdminID, accountID, challengeID, code)
}

func (f *fakeHMESyncService) GetSession(ctx context.Context, accountID int64) (hmesync.SessionInfo, error) {
	if f.getSession == nil {
		return hmesync.SessionInfo{Status: hmesync.StatusLoginRequired}, nil
	}
	return f.getSession(ctx, accountID)
}

func (f *fakeHMESyncService) ClearAuth(ctx context.Context, accountID int64) error {
	if f.clearAuth == nil {
		return errors.New("unexpected ClearAuth call")
	}
	return f.clearAuth(ctx, accountID)
}

func (f *fakeHMESyncService) SyncAliases(ctx context.Context, accountID int64) (hmesync.SyncResult, error) {
	if f.syncAliases == nil {
		return hmesync.SyncResult{}, errors.New("unexpected SyncAliases call")
	}
	return f.syncAliases(ctx, accountID)
}

func TestAdminAPIAppleAuthAndAliasSyncFlow(t *testing.T) {
	env := newAdminAPITestEnv(t)
	var logs strings.Builder
	env.server.logger = slog.New(slog.NewTextHandler(&logs, nil))
	sessionCookie, csrf, admin := env.createSession(t, "apple-flow-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "owner@icloud.com")

	const (
		appleID     = "owner@icloud.com"
		appleSecret = "ordinary-apple-password-secret"
		challengeID = "challenge-secret-0123456789"
		code        = "246810"
		apiKey      = "icm_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	)
	authenticatedAt := time.Date(2026, time.August, 7, 12, 30, 0, 0, time.UTC)
	currentSession := hmesync.SessionInfo{Status: hmesync.StatusLoginRequired}
	var startCalled, verifyCalled, clearCalled, syncCalled bool
	fake := &fakeHMESyncService{}
	fake.startAuth = func(_ context.Context, ownerAdminID, accountID int64, gotAppleID, password string, region apple.Region) (hmesync.AuthResult, error) {
		startCalled = true
		if ownerAdminID != admin.ID || accountID != account.ID || gotAppleID != appleID || password != appleSecret || region != apple.RegionChina {
			t.Fatalf("StartAuth args = admin:%d account:%d appleID:%q password:%q region:%q", ownerAdminID, accountID, gotAppleID, password, region)
		}
		currentSession = hmesync.SessionInfo{Status: hmesync.StatusVerificationRequired, AppleID: appleID, Region: hmesync.RegionChina}
		return hmesync.AuthResult{Status: hmesync.StatusVerificationRequired, ChallengeID: challengeID, Session: currentSession}, nil
	}
	fake.verifyAuth = func(_ context.Context, ownerAdminID, accountID int64, gotChallengeID, gotCode string) (hmesync.AuthResult, error) {
		verifyCalled = true
		if ownerAdminID != admin.ID || accountID != account.ID || gotChallengeID != challengeID || gotCode != code {
			t.Fatalf("VerifyAuth args = admin:%d account:%d challenge:%q code:%q", ownerAdminID, accountID, gotChallengeID, gotCode)
		}
		currentSession = hmesync.SessionInfo{
			Status:          hmesync.StatusAuthenticated,
			AppleID:         appleID,
			Region:          hmesync.RegionChina,
			AuthenticatedAt: &authenticatedAt,
		}
		return hmesync.AuthResult{Status: hmesync.StatusAuthenticated, Session: currentSession}, nil
	}
	fake.getSession = func(_ context.Context, accountID int64) (hmesync.SessionInfo, error) {
		if accountID != account.ID {
			t.Fatalf("GetSession account = %d, want %d", accountID, account.ID)
		}
		return currentSession, nil
	}
	fake.syncAliases = func(ctx context.Context, accountID int64) (hmesync.SyncResult, error) {
		syncCalled = true
		if accountID != account.ID {
			t.Fatalf("SyncAliases account = %d, want %d", accountID, account.ID)
		}
		alias, err := env.store.CreateAlias(ctx, domain.Alias{
			AccountID:    accountID,
			Address:      "generated@privaterelay.appleid.com",
			Label:        "Apple 导入",
			APIKeyHash:   secure.HashToken(apiKey),
			APIKeyPrefix: apiKey[:12],
			Enabled:      true,
		})
		if err != nil {
			return hmesync.SyncResult{}, err
		}
		return hmesync.SyncResult{
			Summary: hmesync.SyncSummary{Total: 3, CreatedCount: 1, ExistingCount: 2, InactiveCount: 1, ImportedDisabledCount: 1},
			Created: []hmesync.CreatedAlias{{Alias: alias, APIKey: apiKey}},
			Session: currentSession,
		}, nil
	}
	fake.clearAuth = func(_ context.Context, accountID int64) error {
		clearCalled = true
		if accountID != account.ID {
			t.Fatalf("ClearAuth account = %d, want %d", accountID, account.ID)
		}
		currentSession = hmesync.SessionInfo{Status: hmesync.StatusLoginRequired}
		return nil
	}
	env.server.SetHMESyncService(fake)

	basePath := fmt.Sprintf("/admin/api/v1/accounts/%d", account.ID)
	loginResponse := env.request(t, http.MethodPost, basePath+"/apple-auth", adminAPITestJSON(t, map[string]any{
		"apple_id": appleID,
		"password": appleSecret,
		"region":   "cn",
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if loginResponse.Code != http.StatusAccepted || !strings.Contains(loginResponse.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("Apple login status/cache = %d %q; body=%s", loginResponse.Code, loginResponse.Header().Get("Cache-Control"), loginResponse.Body.String())
	}
	var loginPayload struct {
		Data struct {
			Status      string `json:"status"`
			ChallengeID string `json:"challenge_id"`
			Session     struct {
				Status  string `json:"status"`
				AppleID string `json:"apple_id"`
				Region  string `json:"region"`
			} `json:"apple_session"`
		} `json:"data"`
	}
	if err := json.Unmarshal(loginResponse.Body.Bytes(), &loginPayload); err != nil {
		t.Fatalf("decode Apple login response: %v; body=%s", err, loginResponse.Body.String())
	}
	if loginPayload.Data.Status != hmesync.StatusVerificationRequired || loginPayload.Data.ChallengeID != challengeID || loginPayload.Data.Session.Status != hmesync.StatusVerificationRequired || loginPayload.Data.Session.AppleID != appleID || loginPayload.Data.Session.Region != hmesync.RegionChina {
		t.Fatalf("Apple login response = %#v", loginPayload.Data)
	}
	if strings.Contains(loginResponse.Body.String(), appleSecret) {
		t.Fatalf("Apple login response exposed password: %s", loginResponse.Body.String())
	}

	verifyResponse := env.request(t, http.MethodPost, basePath+"/apple-auth/verify", adminAPITestJSON(t, map[string]string{
		"challenge_id": challengeID,
		"code":         code,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if verifyResponse.Code != http.StatusOK || !strings.Contains(verifyResponse.Body.String(), `"status":"authenticated"`) {
		t.Fatalf("Apple verify response = %d; body=%s", verifyResponse.Code, verifyResponse.Body.String())
	}
	if strings.Contains(verifyResponse.Body.String(), code) || strings.Contains(verifyResponse.Body.String(), challengeID) {
		t.Fatalf("Apple verify response exposed submitted verification data: %s", verifyResponse.Body.String())
	}

	detailResponse := env.request(t, http.MethodGet, basePath, nil, "", []*http.Cookie{sessionCookie}, "")
	if detailResponse.Code != http.StatusOK || !strings.Contains(detailResponse.Body.String(), `"apple_session":{"status":"authenticated"`) {
		t.Fatalf("account detail Apple session = %d; body=%s", detailResponse.Code, detailResponse.Body.String())
	}

	syncResponse := env.request(t, http.MethodPost, basePath+"/aliases/sync", nil, "", []*http.Cookie{sessionCookie}, csrf)
	if syncResponse.Code != http.StatusOK {
		t.Fatalf("Apple alias sync = %d; body=%s", syncResponse.Code, syncResponse.Body.String())
	}
	var syncPayload struct {
		Data struct {
			Account struct {
				AliasCount int `json:"alias_count"`
			} `json:"account"`
			Aliases []adminAPIAliasDTO `json:"aliases"`
			Summary struct {
				Total                 int `json:"total"`
				CreatedCount          int `json:"created_count"`
				ExistingCount         int `json:"existing_count"`
				InactiveCount         int `json:"inactive_count"`
				ImportedDisabledCount int `json:"imported_disabled_count"`
			} `json:"summary"`
			Created []struct {
				Alias             adminAPIAliasDTO `json:"alias"`
				APIKey            string           `json:"api_key"`
				MailAPIDirectLink string           `json:"mail_api_direct_link"`
			} `json:"created"`
		} `json:"data"`
	}
	if err := json.Unmarshal(syncResponse.Body.Bytes(), &syncPayload); err != nil {
		t.Fatalf("decode Apple sync response: %v; body=%s", err, syncResponse.Body.String())
	}
	if syncPayload.Data.Summary.Total != 3 || syncPayload.Data.Summary.CreatedCount != 1 || syncPayload.Data.Summary.ExistingCount != 2 || syncPayload.Data.Summary.InactiveCount != 1 || syncPayload.Data.Summary.ImportedDisabledCount != 1 || syncPayload.Data.Account.AliasCount != 1 || len(syncPayload.Data.Aliases) != 1 || len(syncPayload.Data.Created) != 1 {
		t.Fatalf("Apple sync payload = %#v", syncPayload.Data)
	}
	created := syncPayload.Data.Created[0]
	if created.Alias.Address != "generated@privaterelay.appleid.com" || created.APIKey != apiKey || !strings.Contains(created.MailAPIDirectLink, "api_key=") || strings.Contains(created.MailAPIDirectLink, apiKey) {
		t.Fatalf("created credential = %#v", created)
	}

	clearResponse := env.request(t, http.MethodDelete, basePath+"/apple-auth", nil, "", []*http.Cookie{sessionCookie}, csrf)
	if clearResponse.Code != http.StatusNoContent || clearResponse.Body.Len() != 0 {
		t.Fatalf("clear Apple auth = %d; body=%s", clearResponse.Code, clearResponse.Body.String())
	}
	if !startCalled || !verifyCalled || !syncCalled || !clearCalled {
		t.Fatalf("service calls start=%t verify=%t sync=%t clear=%t", startCalled, verifyCalled, syncCalled, clearCalled)
	}

	audits, err := env.store.ListAuditLogs(context.Background(), 50, 0)
	if err != nil {
		t.Fatalf("list Apple audit logs: %v", err)
	}
	auditText := fmt.Sprintf("%+v", audits)
	for _, secret := range []string{appleSecret, challengeID, code, apiKey, sessionCookie.Value} {
		if strings.Contains(auditText, secret) {
			t.Fatalf("audit log exposed secret %q: %s", secret, auditText)
		}
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("application log exposed secret %q: %s", secret, logs.String())
		}
	}
	for _, action := range []string{"apple_auth_start", "apple_auth_verify", "sync_hme_aliases", "apple_session_clear"} {
		if !strings.Contains(auditText, action) {
			t.Fatalf("audit log omitted action %q: %s", action, auditText)
		}
	}
}

func TestAdminAPIAppleEndpointsRequireAdminCSRFAndStrictJSON(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "apple-security-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "security@icloud.com")
	callCount := 0
	env.server.SetHMESyncService(&fakeHMESyncService{
		startAuth: func(context.Context, int64, int64, string, string, apple.Region) (hmesync.AuthResult, error) {
			callCount++
			return hmesync.AuthResult{}, nil
		},
		verifyAuth: func(context.Context, int64, int64, string, string) (hmesync.AuthResult, error) {
			callCount++
			return hmesync.AuthResult{}, nil
		},
	})
	path := fmt.Sprintf("/admin/api/v1/accounts/%d/apple-auth", account.ID)
	body := adminAPITestJSON(t, map[string]string{"apple_id": "security@icloud.com", "password": "secret", "region": "global"})

	unauthenticated := env.request(t, http.MethodPost, path, body, "application/json", nil, "")
	if unauthenticated.Code != http.StatusUnauthorized || adminAPITestErrorCode(t, unauthenticated) != "AUTH_REQUIRED" {
		t.Fatalf("unauthenticated Apple auth = %d; body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}
	missingCSRF := env.request(t, http.MethodPost, path, body, "application/json", []*http.Cookie{sessionCookie}, "")
	if missingCSRF.Code != http.StatusForbidden || adminAPITestErrorCode(t, missingCSRF) != "CSRF_INVALID" {
		t.Fatalf("Apple auth without CSRF = %d; body=%s", missingCSRF.Code, missingCSRF.Body.String())
	}
	unknownField := env.request(t, http.MethodPost, path, adminAPITestJSON(t, map[string]string{
		"apple_id": "security@icloud.com", "password": "secret", "region": "global", "cookie": "must-not-be-accepted",
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if unknownField.Code != http.StatusBadRequest || adminAPITestErrorCode(t, unknownField) != "INVALID_JSON" {
		t.Fatalf("Apple auth unknown field = %d; body=%s", unknownField.Code, unknownField.Body.String())
	}
	invalidRegion := env.request(t, http.MethodPost, path, adminAPITestJSON(t, map[string]string{
		"apple_id": "security@icloud.com", "password": "secret", "region": "other",
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if invalidRegion.Code != http.StatusBadRequest || adminAPITestErrorCode(t, invalidRegion) != "VALIDATION_FAILED" {
		t.Fatalf("Apple auth invalid region = %d; body=%s", invalidRegion.Code, invalidRegion.Body.String())
	}
	invalidVerification := env.request(t, http.MethodPost, path+"/verify", adminAPITestJSON(t, map[string]string{
		"challenge_id": "challenge", "code": "12A456",
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if invalidVerification.Code != http.StatusBadRequest || adminAPITestErrorCode(t, invalidVerification) != "VALIDATION_FAILED" {
		t.Fatalf("Apple verify invalid code = %d; body=%s", invalidVerification.Code, invalidVerification.Body.String())
	}
	if callCount != 0 {
		t.Fatalf("Apple service called %d times for rejected requests", callCount)
	}
}

func TestAdminAPIAppleErrorsUseDedicatedCodesAndRedactedLogs(t *testing.T) {
	env := newAdminAPITestEnv(t)
	var logs strings.Builder
	env.server.logger = slog.New(slog.NewTextHandler(&logs, nil))
	sessionCookie, csrf, _ := env.createSession(t, "apple-error-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "errors@icloud.com")
	var serviceErr error
	env.server.SetHMESyncService(&fakeHMESyncService{
		startAuth: func(context.Context, int64, int64, string, string, apple.Region) (hmesync.AuthResult, error) {
			return hmesync.AuthResult{}, serviceErr
		},
	})

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "login required", err: hmesync.ErrLoginRequired, wantStatus: http.StatusConflict, wantCode: hmesync.CodeLoginRequired},
		{name: "login required wrapping missing row", err: fmt.Errorf("login: %w; row: %w", hmesync.ErrLoginRequired, store.ErrNotFound), wantStatus: http.StatusConflict, wantCode: hmesync.CodeLoginRequired},
		{name: "session expired", err: hmesync.ErrSessionExpired, wantStatus: http.StatusConflict, wantCode: hmesync.CodeSessionExpired},
		{name: "credentials", err: hmesync.ErrCredentialsInvalid, wantStatus: http.StatusUnprocessableEntity, wantCode: hmesync.CodeCredentialsInvalid},
		{name: "verification", err: hmesync.ErrVerificationInvalid, wantStatus: http.StatusUnprocessableEntity, wantCode: hmesync.CodeVerificationInvalid},
		{name: "flow expired", err: hmesync.ErrFlowExpired, wantStatus: http.StatusGone, wantCode: hmesync.CodeFlowExpired},
		{name: "account action required", err: hmesync.ErrAccountActionRequired, wantStatus: http.StatusConflict, wantCode: hmesync.CodeAccountActionRequired},
		{name: "rate limited", err: hmesync.ErrRateLimited, wantStatus: http.StatusTooManyRequests, wantCode: hmesync.CodeRateLimited},
		{name: "account mismatch", err: hmesync.ErrAccountMismatch, wantStatus: http.StatusConflict, wantCode: hmesync.CodeAccountMismatch},
		{name: "account changed", err: hmesync.ErrAccountChanged, wantStatus: http.StatusConflict, wantCode: hmesync.CodeAccountChanged},
		{name: "alias conflict", err: store.ErrAliasOwnershipConflict, wantStatus: http.StatusConflict, wantCode: hmesync.CodeAliasOwnershipConflict},
		{name: "deadline", err: context.DeadlineExceeded, wantStatus: http.StatusGatewayTimeout, wantCode: hmesync.CodeUpstreamError},
		{name: "upstream", err: hmesync.ErrUpstream, wantStatus: http.StatusBadGateway, wantCode: hmesync.CodeUpstreamError},
	}
	path := fmt.Sprintf("/admin/api/v1/accounts/%d/apple-auth", account.ID)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serviceErr = test.err
			response := env.request(t, http.MethodPost, path, adminAPITestJSON(t, map[string]string{
				"apple_id": "errors@icloud.com", "password": "not-logged", "region": "global",
			}), "application/json", []*http.Cookie{sessionCookie}, csrf)
			if response.Code != test.wantStatus || adminAPITestErrorCode(t, response) != test.wantCode {
				t.Fatalf("Apple error response = %d; body=%s; want %d %s", response.Code, response.Body.String(), test.wantStatus, test.wantCode)
			}
			if test.wantCode == hmesync.CodeAccountActionRequired && !strings.Contains(response.Body.String(), "请前往 Apple 官网处理后重试") {
				t.Fatalf("Apple account action response omitted actionable message: %s", response.Body.String())
			}
			if test.wantCode == "AUTH_REQUIRED" || test.wantCode == "SESSION_EXPIRED" {
				t.Fatalf("Apple error reused an administrator session code: %s", test.wantCode)
			}
		})
	}

	const secretInError = "cookie-and-token-secret"
	serviceErr = errors.New("upstream included " + secretInError)
	redacted := env.request(t, http.MethodPost, path, adminAPITestJSON(t, map[string]string{
		"apple_id": "errors@icloud.com", "password": "password-secret", "region": "global",
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if redacted.Code != http.StatusInternalServerError || adminAPITestErrorCode(t, redacted) != "INTERNAL_ERROR" {
		t.Fatalf("redacted upstream response = %d; body=%s", redacted.Code, redacted.Body.String())
	}
	if strings.Contains(redacted.Body.String(), secretInError) || strings.Contains(logs.String(), secretInError) || strings.Contains(logs.String(), "password-secret") {
		t.Fatalf("Apple upstream secret reached response or logs: body=%s logs=%s", redacted.Body.String(), logs.String())
	}
	audits, err := env.store.ListAuditLogs(context.Background(), 100, 0)
	if err != nil {
		t.Fatalf("list error audits: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%+v", audits), secretInError) {
		t.Fatalf("Apple upstream secret reached audit log: %+v", audits)
	}
}

func TestAdminAPIAppleSyncReturnsOneTimeKeyWhenDirectLinkRenderingFails(t *testing.T) {
	env := newAdminAPITestEnv(t)
	var logs strings.Builder
	env.server.logger = slog.New(slog.NewTextHandler(&logs, nil))
	sessionCookie, csrf, _ := env.createSession(t, "apple-key-fallback-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "fallback@icloud.com")
	const apiKey = "icm_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	env.server.SetHMESyncService(&fakeHMESyncService{
		syncAliases: func(ctx context.Context, accountID int64) (hmesync.SyncResult, error) {
			alias, err := env.store.CreateAlias(ctx, domain.Alias{
				AccountID:    accountID,
				Address:      "fallback@privaterelay.appleid.com",
				APIKeyHash:   secure.HashToken(apiKey),
				APIKeyPrefix: apiKey[:12],
				Enabled:      true,
			})
			if err != nil {
				return hmesync.SyncResult{}, err
			}
			// Simulate a malformed optional DTO field after the database commit.
			// The raw key still has to reach the caller.
			alias.APIKeyHash = []byte("invalid")
			return hmesync.SyncResult{
				Summary: hmesync.SyncSummary{Total: 1, CreatedCount: 1},
				Created: []hmesync.CreatedAlias{{Alias: alias, APIKey: apiKey}},
				Session: hmesync.SessionInfo{Status: hmesync.StatusAuthenticated, AppleID: account.Email, Region: hmesync.RegionGlobal},
			}, nil
		},
	})
	path := fmt.Sprintf("/admin/api/v1/accounts/%d/aliases/sync", account.ID)
	response := env.request(t, http.MethodPost, path, nil, "", []*http.Cookie{sessionCookie}, csrf)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), apiKey) || !strings.Contains(response.Body.String(), `"mail_api_direct_link":""`) {
		t.Fatalf("fallback sync response = %d; body=%s", response.Code, response.Body.String())
	}
	if _, err := env.store.GetAliasByAddress(context.Background(), "fallback@privaterelay.appleid.com"); err != nil {
		t.Fatalf("committed alias missing after fallback response: %v", err)
	}
	if strings.Contains(logs.String(), apiKey) {
		t.Fatalf("fallback warning exposed API key: %s", logs.String())
	}
}

func TestAdminAPIAppleSyncRefreshesCommittedAccountDetail(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "apple-refresh-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "refresh@icloud.com")
	const apiKey = "icm_CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	env.server.SetHMESyncService(&fakeHMESyncService{
		syncAliases: func(ctx context.Context, accountID int64) (hmesync.SyncResult, error) {
			created, err := env.store.CreateAlias(ctx, domain.Alias{
				AccountID:    accountID,
				Address:      "created@privaterelay.appleid.com",
				APIKeyHash:   secure.HashToken(apiKey),
				APIKeyPrefix: apiKey[:12],
				Enabled:      true,
			})
			if err != nil {
				return hmesync.SyncResult{}, err
			}
			if _, err := env.store.CreateAlias(ctx, domain.Alias{
				AccountID:    accountID,
				Address:      "concurrent@privaterelay.appleid.com",
				APIKeyHash:   secure.HashToken("concurrent-key"),
				APIKeyPrefix: "concurrent",
				Enabled:      true,
			}); err != nil {
				return hmesync.SyncResult{}, err
			}
			return hmesync.SyncResult{
				Summary: hmesync.SyncSummary{Total: 1, CreatedCount: 1},
				Created: []hmesync.CreatedAlias{{Alias: created, APIKey: apiKey}},
				Session: hmesync.SessionInfo{Status: hmesync.StatusAuthenticated},
			}, nil
		},
	})

	path := fmt.Sprintf("/admin/api/v1/accounts/%d/aliases/sync", account.ID)
	response := env.request(t, http.MethodPost, path, nil, "", []*http.Cookie{sessionCookie}, csrf)
	if response.Code != http.StatusOK {
		t.Fatalf("refresh sync response = %d; body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Account struct {
				AliasCount int `json:"alias_count"`
			} `json:"account"`
			Aliases []adminAPIAliasDTO `json:"aliases"`
			Created []struct {
				APIKey string `json:"api_key"`
			} `json:"created"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode refresh sync response: %v; body=%s", err, response.Body.String())
	}
	if payload.Data.Account.AliasCount != 2 || len(payload.Data.Aliases) != 2 || len(payload.Data.Created) != 1 || payload.Data.Created[0].APIKey != apiKey {
		t.Fatalf("refreshed sync payload = %#v", payload.Data)
	}
}

func TestAdminAPIAppleSyncKeepsOneTimeKeyWhenDetailRefreshFails(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "apple-refresh-fallback-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "refresh-fallback@icloud.com")
	const apiKey = "icm_DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
	env.server.SetHMESyncService(&fakeHMESyncService{
		syncAliases: func(ctx context.Context, accountID int64) (hmesync.SyncResult, error) {
			alias, err := env.store.CreateAlias(ctx, domain.Alias{
				AccountID:    accountID,
				Address:      "refresh-fallback@privaterelay.appleid.com",
				APIKeyHash:   secure.HashToken(apiKey),
				APIKeyPrefix: apiKey[:12],
				Enabled:      true,
			})
			if err != nil {
				return hmesync.SyncResult{}, err
			}
			if err := env.store.Close(); err != nil {
				return hmesync.SyncResult{}, err
			}
			return hmesync.SyncResult{
				Summary: hmesync.SyncSummary{Total: 1, CreatedCount: 1},
				Created: []hmesync.CreatedAlias{{Alias: alias, APIKey: apiKey}},
				Session: hmesync.SessionInfo{Status: hmesync.StatusAuthenticated},
			}, nil
		},
	})

	path := fmt.Sprintf("/admin/api/v1/accounts/%d/aliases/sync", account.ID)
	response := env.request(t, http.MethodPost, path, nil, "", []*http.Cookie{sessionCookie}, csrf)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), apiKey) || !strings.Contains(response.Body.String(), `"alias_count":1`) {
		t.Fatalf("refresh fallback sync response = %d; body=%s", response.Code, response.Body.String())
	}
}

func TestAdminAPIMergeCreatedAliasesDeduplicatesNormalizedAddress(t *testing.T) {
	existing := []adminAPIAliasDTO{
		{ID: 10, Address: " SAME@PrivateRelay.AppleID.com "},
		{ID: 11, Address: "other@privaterelay.appleid.com"},
		{ID: 12, Address: "OTHER@privaterelay.appleid.com"},
	}
	created := []adminAPIAppleCreatedAliasDTO{
		{Alias: adminAPIAliasDTO{ID: 20, Address: "same@privaterelay.appleid.com"}},
	}

	merged := adminAPIMergeCreatedAliases(existing, created)
	if len(merged) != 2 {
		t.Fatalf("merged aliases = %#v, want two normalized addresses", merged)
	}
	byAddress := make(map[string]adminAPIAliasDTO, len(merged))
	for _, alias := range merged {
		byAddress[domain.NormalizeEmail(alias.Address)] = alias
	}
	if same := byAddress["same@privaterelay.appleid.com"]; same.ID != 20 {
		t.Fatalf("normalized duplicate kept alias ID %d, want created ID 20", same.ID)
	}
	if other := byAddress["other@privaterelay.appleid.com"]; other.ID != 11 {
		t.Fatalf("existing normalized duplicate kept alias ID %d, want first ID 11", other.ID)
	}
}

func adminAPITestCreateAccount(t *testing.T, env *adminAPITestEnv, email string) domain.Account {
	t.Helper()
	encrypted, err := env.cipher.Encrypt("app-specific-password")
	if err != nil {
		t.Fatalf("encrypt account password: %v", err)
	}
	account, err := env.store.CreateAccount(context.Background(), domain.Account{
		Name:               "Apple API test",
		Email:              email,
		IMAPHost:           "imap.mail.me.com",
		IMAPPort:           993,
		IMAPUsername:       email,
		PasswordCiphertext: encrypted,
		Enabled:            true,
	})
	if err != nil {
		t.Fatalf("create Apple API test account: %v", err)
	}
	return account
}

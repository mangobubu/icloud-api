package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"icloud-api/internal/config"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
	"icloud-api/internal/syncer"
)

type adminAPITestEnv struct {
	store  *store.Store
	cipher *secure.Cipher
	server *Server
	router http.Handler
}

func newAdminAPITestEnv(t *testing.T) *adminAPITestEnv {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "admin-api-test.db"))
	if err != nil {
		t.Fatalf("open admin API test store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close admin API test store: %v", err)
		}
	})
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x6b}, 32))
	if err != nil {
		t.Fatalf("create admin API test cipher: %v", err)
	}
	server, err := New(db, cipher, config.Config{
		CookieSecure: false,
		SessionTTL:   time.Hour,
		PollInterval: time.Minute,
		GinMode:      gin.TestMode,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(int64) error { return nil })
	if err != nil {
		t.Fatalf("create admin API test server: %v", err)
	}
	router := gin.New()
	router.Use(server.requestContext(), server.securityHeaders(), gin.Recovery())
	server.registerAdminAPIRoutes(router.Group("/admin/api/v1"))
	return &adminAPITestEnv{store: db, cipher: cipher, server: server, router: router}
}

func (e *adminAPITestEnv) createAdmin(t *testing.T, username, password string) domain.Admin {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash admin API test password: %v", err)
	}
	admin, err := e.store.CreateAdmin(context.Background(), username, string(hash))
	if err != nil {
		t.Fatalf("create admin API test admin: %v", err)
	}
	return admin
}

func (e *adminAPITestEnv) createSession(t *testing.T, username, password string) (*http.Cookie, string, domain.Admin) {
	t.Helper()
	admin := e.createAdmin(t, username, password)
	rawToken := "admin-api-session-" + username
	csrf := "admin-api-csrf-" + username
	if err := e.store.CreateSession(context.Background(), secure.HashToken(rawToken), domain.Session{
		AdminID:         admin.ID,
		Username:        admin.Username,
		PasswordVersion: admin.PasswordVersion,
		CSRF:            csrf,
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create admin API test session: %v", err)
	}
	return &http.Cookie{Name: sessionCookie, Value: rawToken, Path: "/admin"}, csrf, admin
}

func (e *adminAPITestEnv) request(
	t *testing.T,
	method string,
	target string,
	body []byte,
	contentType string,
	cookies []*http.Cookie,
	csrf string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://admin.example.test"+target, bytes.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if csrf != "" {
		request.Header.Set(adminAPICSRFHeader, csrf)
	}
	request.Header.Set("Origin", "http://admin.example.test")
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	e.router.ServeHTTP(response, request)
	return response
}

func adminAPITestJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal admin API test body: %v", err)
	}
	return encoded
}

func adminAPITestCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name && cookie.MaxAge >= 0 {
			return cookie
		}
	}
	t.Fatalf("response did not set cookie %q", name)
	return nil
}

func adminAPITestErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode admin API error: %v; body=%s", err, response.Body.String())
	}
	if payload.Error.RequestID == "" {
		t.Fatalf("admin API error omitted request_id: %s", response.Body.String())
	}
	return payload.Error.Code
}

func TestAdminAPIAliasDTOWithholdsPendingConfirmationDirectLink(t *testing.T) {
	env := newAdminAPITestEnv(t)
	alias := domain.Alias{
		ID:             17,
		AccountID:      9,
		Address:        "pending-link@icloud.com",
		APIKeyHash:     secure.HashToken("pending-link-key"),
		APIKeyPrefix:   "icm_pending",
		Enabled:        false,
		LastSyncStatus: domain.SyncStatusPending,
		LastSyncError:  domain.AppleAliasConfirmationPending,
	}

	pendingDTO, err := env.server.adminAPIAliasFromDomain(alias)
	if err != nil {
		t.Fatalf("build pending alias DTO: %v", err)
	}
	if pendingDTO.DirectLinkPath != "" {
		t.Fatalf("pending alias direct link = %q, want empty", pendingDTO.DirectLinkPath)
	}

	alias.Enabled = true
	alias.LastSyncError = ""
	confirmedDTO, err := env.server.adminAPIAliasFromDomain(alias)
	if err != nil {
		t.Fatalf("build confirmed alias DTO: %v", err)
	}
	if !strings.Contains(confirmedDTO.DirectLinkPath, "/api/v1/mail/recent?") {
		t.Fatalf("confirmed alias direct link = %q", confirmedDTO.DirectLinkPath)
	}
}

func TestAdminAPIAccountDTOIncludesLiveSyncProgress(t *testing.T) {
	startedAt := time.Date(2026, 8, 9, 10, 11, 12, 0, time.UTC)
	updatedAt := startedAt.Add(3 * time.Second)
	server := &Server{}
	server.SetSyncProgressProvider(func(accountID int64) (domain.MailboxSyncProgress, bool) {
		if accountID != 17 {
			return domain.MailboxSyncProgress{}, false
		}
		return domain.MailboxSyncProgress{
			AccountID: accountID,
			Trigger:   domain.MailboxSyncTriggerAutomatic,
			Phase:     domain.MailboxSyncPhaseReading,
			Percent:   64,
			StartedAt: startedAt,
			UpdatedAt: updatedAt,
		}, true
	})

	dto := server.adminAPIAccountFromDomain(domain.Account{ID: 17})
	if dto.SyncProgress == nil {
		t.Fatal("active account sync progress was omitted")
	}
	if !dto.SyncProgress.Active || dto.SyncProgress.Source != "automatic" ||
		dto.SyncProgress.Stage != "reading" || dto.SyncProgress.Percentage != 64 ||
		dto.SyncProgress.StartedAt != startedAt.Format(time.RFC3339) ||
		dto.SyncProgress.UpdatedAt != updatedAt.Format(time.RFC3339) {
		t.Fatalf("sync progress DTO = %#v", dto.SyncProgress)
	}
	if inactive := server.adminAPIAccountFromDomain(domain.Account{ID: 18}); inactive.SyncProgress != nil {
		t.Fatalf("inactive account progress = %#v, want nil", inactive.SyncProgress)
	}
}

func TestAdminAPISyncErrorKeepsSummaryAndFullLog(t *testing.T) {
	fullLog := strings.Repeat("详细错误", 100)
	dto := adminAPIAccountFromDomain(domain.Account{LastSyncError: fullLog})
	if got := len([]rune(dto.LastSyncError)); got != 240 {
		t.Fatalf("sync error summary rune count = %d, want 240", got)
	}
	if dto.LastSyncErrorLog != fullLog {
		t.Fatalf("full sync error log length = %d, want %d", len([]rune(dto.LastSyncErrorLog)), len([]rune(fullLog)))
	}
}

func TestAdminAPISyncAccountReturnsAcceptedForPendingBatch(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "sync-pending-admin", "sync pending password")
	account := adminAPITestCreateAccount(t, env, "sync-pending@icloud.com")
	syncCalls := 0
	env.server.sync = func(accountID int64) error {
		syncCalls++
		if accountID != account.ID {
			t.Fatalf("sync account ID = %d, want %d", accountID, account.ID)
		}
		return syncer.ErrSyncPending
	}

	response := env.request(
		t,
		http.MethodPost,
		fmt.Sprintf("/admin/api/v1/accounts/%d/sync", account.ID),
		nil,
		"",
		[]*http.Cookie{sessionCookie},
		csrf,
	)
	if response.Code != http.StatusAccepted {
		t.Fatalf("pending sync status = %d, want 202; body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data adminAPIAccountDetailDTO `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode pending sync response: %v; body=%s", err, response.Body.String())
	}
	if syncCalls != 1 || !payload.Data.SyncPending || payload.Data.Account.ID != account.ID {
		t.Fatalf("pending sync payload/calls = %#v / %d", payload.Data, syncCalls)
	}
	logs, err := env.store.ListAuditLogs(context.Background(), 10, 0)
	if err != nil {
		t.Fatalf("list pending sync audit: %v", err)
	}
	if len(logs) == 0 || logs[0].Action != "sync" || logs[0].Result != "success" {
		t.Fatalf("pending batch audit = %#v, want successful sync", logs)
	}
}

func TestAdminAPISyncAccountReturnsAcceptedWhenQueued(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "sync-queued-admin", "sync queued password")
	account := adminAPITestCreateAccount(t, env, "sync-queued@icloud.com")
	syncCalls := 0
	env.server.sync = func(accountID int64) error {
		syncCalls++
		if accountID != account.ID {
			t.Fatalf("sync account ID = %d, want %d", accountID, account.ID)
		}
		return syncer.ErrSyncQueued
	}

	response := env.request(
		t,
		http.MethodPost,
		fmt.Sprintf("/admin/api/v1/accounts/%d/sync", account.ID),
		nil,
		"",
		[]*http.Cookie{sessionCookie},
		csrf,
	)
	if response.Code != http.StatusAccepted {
		t.Fatalf("queued sync status = %d, want 202; body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data adminAPIAccountDetailDTO `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode queued sync response: %v; body=%s", err, response.Body.String())
	}
	if syncCalls != 1 || !payload.Data.SyncPending || payload.Data.Account.ID != account.ID {
		t.Fatalf("queued sync payload/calls = %#v / %d", payload.Data, syncCalls)
	}
}

func TestAdminAPISyncAccountRejectsDisabledAccount(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "sync-disabled-admin", "sync disabled password")
	account := adminAPITestCreateAccount(t, env, "sync-disabled@icloud.com")
	account.Enabled = false
	if _, err := env.store.UpdateAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	syncCalls := 0
	env.server.sync = func(int64) error {
		syncCalls++
		return syncer.ErrSyncQueued
	}

	response := env.request(
		t,
		http.MethodPost,
		fmt.Sprintf("/admin/api/v1/accounts/%d/sync", account.ID),
		nil,
		"",
		[]*http.Cookie{sessionCookie},
		csrf,
	)
	if response.Code != http.StatusConflict || adminAPITestErrorCode(t, response) != "ACCOUNT_DISABLED" {
		t.Fatalf("disabled sync response = %d/%s; body=%s", response.Code, adminAPITestErrorCode(t, response), response.Body.String())
	}
	if syncCalls != 0 {
		t.Fatalf("disabled sync calls = %d, want 0", syncCalls)
	}
}

func TestAdminAPISyncAccountReturnsOKWhenCaughtUp(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "sync-complete-admin", "sync complete password")
	account := adminAPITestCreateAccount(t, env, "sync-complete@icloud.com")
	syncCalls := 0
	env.server.sync = func(accountID int64) error {
		syncCalls++
		if accountID != account.ID {
			t.Fatalf("sync account ID = %d, want %d", accountID, account.ID)
		}
		return nil
	}

	response := env.request(
		t,
		http.MethodPost,
		fmt.Sprintf("/admin/api/v1/accounts/%d/sync", account.ID),
		nil,
		"",
		[]*http.Cookie{sessionCookie},
		csrf,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("completed sync status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Account     adminAPIAccountDTO `json:"account"`
			SyncPending *bool              `json:"sync_pending"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode completed sync response: %v; body=%s", err, response.Body.String())
	}
	if syncCalls != 1 || payload.Data.Account.ID != account.ID || payload.Data.SyncPending != nil {
		t.Fatalf("completed sync payload/calls = %#v / %d", payload.Data, syncCalls)
	}
}

func TestAdminAPIAuthFlowUsesJSONWithoutRedirects(t *testing.T) {
	env := newAdminAPITestEnv(t)
	const (
		username = "api-admin"
		password = "correct horse battery staple"
	)
	env.createAdmin(t, username, password)

	unauthorized := env.request(t, http.MethodGet, "/admin/api/v1/auth/session", nil, "", nil, "")
	if unauthorized.Code != http.StatusUnauthorized || unauthorized.Header().Get("Location") != "" {
		t.Fatalf("unauthenticated session response = %d Location=%q", unauthorized.Code, unauthorized.Header().Get("Location"))
	}
	if code := adminAPITestErrorCode(t, unauthorized); code != "AUTH_REQUIRED" {
		t.Fatalf("unauthenticated session code = %q", code)
	}

	csrfResponse := env.request(t, http.MethodGet, "/admin/api/v1/auth/csrf", nil, "", nil, "")
	if csrfResponse.Code != http.StatusOK {
		t.Fatalf("GET login CSRF status = %d; body=%s", csrfResponse.Code, csrfResponse.Body.String())
	}
	loginCookie := adminAPITestCookie(t, csrfResponse, adminAPILoginCSRFCookie)
	if loginCookie.Path != adminAPILoginCSRFPath || !loginCookie.HttpOnly || loginCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("login CSRF cookie attributes = %#v", loginCookie)
	}
	var csrfPayload struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(csrfResponse.Body.Bytes(), &csrfPayload); err != nil || csrfPayload.Data.CSRFToken == "" {
		t.Fatalf("decode login CSRF response: %v; body=%s", err, csrfResponse.Body.String())
	}
	reusedCSRFResponse := env.request(t, http.MethodGet, "/admin/api/v1/auth/csrf", nil, "", []*http.Cookie{loginCookie}, "")
	var reusedCSRFPayload struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(reusedCSRFResponse.Body.Bytes(), &reusedCSRFPayload); err != nil || reusedCSRFPayload.Data.CSRFToken != csrfPayload.Data.CSRFToken {
		t.Fatalf("login CSRF token was not reused: err=%v body=%s", err, reusedCSRFResponse.Body.String())
	}

	loginBody := adminAPITestJSON(t, gin.H{"username": username, "password": password})
	missingCSRF := env.request(t, http.MethodPost, "/admin/api/v1/auth/login", loginBody, "application/json", []*http.Cookie{loginCookie}, "")
	if missingCSRF.Code != http.StatusForbidden || adminAPITestErrorCode(t, missingCSRF) != "CSRF_INVALID" {
		t.Fatalf("login without header CSRF = %d; body=%s", missingCSRF.Code, missingCSRF.Body.String())
	}

	loginResponse := env.request(t, http.MethodPost, "/admin/api/v1/auth/login", loginBody, "application/json", []*http.Cookie{loginCookie}, csrfPayload.Data.CSRFToken)
	if loginResponse.Code != http.StatusOK || loginResponse.Header().Get("Location") != "" {
		t.Fatalf("valid API login = %d Location=%q; body=%s", loginResponse.Code, loginResponse.Header().Get("Location"), loginResponse.Body.String())
	}
	sessionCookie := adminAPITestCookie(t, loginResponse, sessionCookie)
	if sessionCookie.Path != "/admin" || !sessionCookie.HttpOnly {
		t.Fatalf("session cookie attributes = %#v", sessionCookie)
	}
	var loginPayload struct {
		Data adminAPISessionDTO `json:"data"`
	}
	if err := json.Unmarshal(loginResponse.Body.Bytes(), &loginPayload); err != nil {
		t.Fatalf("decode API login response: %v", err)
	}
	if loginPayload.Data.Admin.Username != username || loginPayload.Data.CSRFToken == "" || loginPayload.Data.ExpiresAt == "" {
		t.Fatalf("API login session = %#v", loginPayload.Data)
	}
	for _, forbidden := range []string{password, "PasswordHash", "PasswordVersion"} {
		if strings.Contains(loginResponse.Body.String(), forbidden) {
			t.Fatalf("API login response exposed %q: %s", forbidden, loginResponse.Body.String())
		}
	}

	sessionResponse := env.request(t, http.MethodGet, "/admin/api/v1/auth/session", nil, "", []*http.Cookie{sessionCookie}, "")
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("GET authenticated session = %d; body=%s", sessionResponse.Code, sessionResponse.Body.String())
	}
	missingSessionCSRF := env.request(t, http.MethodPost, "/admin/api/v1/auth/logout", nil, "", []*http.Cookie{sessionCookie}, "")
	if missingSessionCSRF.Code != http.StatusForbidden || adminAPITestErrorCode(t, missingSessionCSRF) != "CSRF_INVALID" {
		t.Fatalf("logout without CSRF = %d; body=%s", missingSessionCSRF.Code, missingSessionCSRF.Body.String())
	}
	logoutResponse := env.request(t, http.MethodPost, "/admin/api/v1/auth/logout", nil, "", []*http.Cookie{sessionCookie}, loginPayload.Data.CSRFToken)
	if logoutResponse.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d; body=%s", logoutResponse.Code, logoutResponse.Body.String())
	}
	expired := env.request(t, http.MethodGet, "/admin/api/v1/auth/session", nil, "", []*http.Cookie{sessionCookie}, "")
	if expired.Code != http.StatusUnauthorized || expired.Header().Get("Location") != "" || adminAPITestErrorCode(t, expired) != "SESSION_EXPIRED" {
		t.Fatalf("deleted session response = %d Location=%q; body=%s", expired.Code, expired.Header().Get("Location"), expired.Body.String())
	}
}

func TestAdminAPIAccountAndAliasLifecycleDoesNotExposeStoredSecrets(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.SetHMESyncService(&fakeHMESyncService{
		deleteAlias: func(ctx context.Context, aliasID int64) error {
			return env.store.DeleteAlias(ctx, aliasID)
		},
	})
	sessionCookie, csrf, _ := env.createSession(t, "resource-admin", "unused-resource-password")
	const appPassword = "api-test-imap-app-password"
	createAccount := env.request(t, http.MethodPost, "/admin/api/v1/accounts", adminAPITestJSON(t, gin.H{
		"name":          "API Account",
		"email":         "api-account@icloud.com",
		"imap_username": "api-account@icloud.com",
		"imap_password": appPassword,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if createAccount.Code != http.StatusCreated {
		t.Fatalf("create account status = %d; body=%s", createAccount.Code, createAccount.Body.String())
	}
	var accountPayload struct {
		Data adminAPIAccountDTO `json:"data"`
	}
	if err := json.Unmarshal(createAccount.Body.Bytes(), &accountPayload); err != nil || accountPayload.Data.ID < 1 {
		t.Fatalf("decode created account: %v; body=%s", err, createAccount.Body.String())
	}
	if accountPayload.Data.IMAPHost != domain.DefaultIMAPHost || accountPayload.Data.IMAPPort != domain.DefaultIMAPPort {
		t.Fatalf("default IMAP endpoint = %q:%d", accountPayload.Data.IMAPHost, accountPayload.Data.IMAPPort)
	}
	for _, forbidden := range []string{appPassword, "PasswordCiphertext", "password_ciphertext"} {
		if strings.Contains(createAccount.Body.String(), forbidden) {
			t.Fatalf("account response exposed %q: %s", forbidden, createAccount.Body.String())
		}
	}

	accountPath := "/admin/api/v1/accounts/" + strconvFormatInt(accountPayload.Data.ID)
	updatedAccount := env.request(t, http.MethodPut, accountPath, adminAPITestJSON(t, gin.H{
		"name":          "API Account",
		"email":         "api-account@icloud.com",
		"imap_host":     "IMAP.Example.Test.",
		"imap_port":     1993,
		"imap_username": "api-account@icloud.com",
		"imap_password": "",
		"enabled":       true,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if updatedAccount.Code != http.StatusOK {
		t.Fatalf("update account status = %d; body=%s", updatedAccount.Code, updatedAccount.Body.String())
	}
	if err := json.Unmarshal(updatedAccount.Body.Bytes(), &accountPayload); err != nil {
		t.Fatalf("decode updated account: %v; body=%s", err, updatedAccount.Body.String())
	}
	if accountPayload.Data.IMAPHost != "imap.example.test" || accountPayload.Data.IMAPPort != 1993 {
		t.Fatalf("updated IMAP endpoint = %q:%d", accountPayload.Data.IMAPHost, accountPayload.Data.IMAPPort)
	}

	preservedEndpoint := env.request(t, http.MethodPut, accountPath, adminAPITestJSON(t, gin.H{
		"name":          "API Account",
		"email":         "api-account@icloud.com",
		"imap_username": "api-account@icloud.com",
		"imap_password": "",
		"enabled":       true,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if preservedEndpoint.Code != http.StatusOK {
		t.Fatalf("update account without endpoint status = %d; body=%s", preservedEndpoint.Code, preservedEndpoint.Body.String())
	}
	if err := json.Unmarshal(preservedEndpoint.Body.Bytes(), &accountPayload); err != nil || accountPayload.Data.IMAPHost != "imap.example.test" || accountPayload.Data.IMAPPort != 1993 {
		t.Fatalf("omitted endpoint was not preserved: err=%v body=%s", err, preservedEndpoint.Body.String())
	}

	createAlias := env.request(t, http.MethodPost, accountPath+"/aliases", adminAPITestJSON(t, gin.H{
		"address": "relay-api@icloud.com",
		"label":   "API Alias",
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if createAlias.Code != http.StatusCreated || createAlias.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create alias status/cache = %d %q; body=%s", createAlias.Code, createAlias.Header().Get("Cache-Control"), createAlias.Body.String())
	}
	var aliasPayload struct {
		Data adminAPIOneTimeKeyDTO `json:"data"`
	}
	if err := json.Unmarshal(createAlias.Body.Bytes(), &aliasPayload); err != nil || aliasPayload.Data.APIKey == "" {
		t.Fatalf("decode created alias: %v; body=%s", err, createAlias.Body.String())
	}
	firstKey := aliasPayload.Data.APIKey
	aliasID := aliasPayload.Data.Alias.ID
	if err := assertAdminDirectLinkPath(t, aliasPayload.Data.Alias, secure.HashToken(firstKey), env.cipher); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(createAlias.Body.String(), "APIKeyHash") || strings.Contains(createAlias.Body.String(), "api_key_hash") {
		t.Fatalf("create alias exposed API key hash: %s", createAlias.Body.String())
	}

	detail := env.request(t, http.MethodGet, accountPath, nil, "", []*http.Cookie{sessionCookie}, "")
	aliases := env.request(t, http.MethodGet, "/admin/api/v1/aliases", nil, "", []*http.Cookie{sessionCookie}, "")
	var detailPayload struct {
		Data adminAPIAccountDetailDTO `json:"data"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &detailPayload); err != nil || len(detailPayload.Data.Aliases) != 1 {
		t.Fatalf("decode account detail aliases: err=%v body=%s", err, detail.Body.String())
	}
	if err := assertAdminDirectLinkPath(t, detailPayload.Data.Aliases[0], secure.HashToken(firstKey), env.cipher); err != nil {
		t.Fatal(err)
	}
	var aliasesPayload struct {
		Data struct {
			Items      []adminAPIAliasDTO `json:"items"`
			Pagination struct {
				Offset  int  `json:"offset"`
				Total   int  `json:"total"`
				HasMore bool `json:"has_more"`
			} `json:"pagination"`
		} `json:"data"`
	}
	if err := json.Unmarshal(aliases.Body.Bytes(), &aliasesPayload); err != nil || len(aliasesPayload.Data.Items) != 1 {
		t.Fatalf("decode aliases response: err=%v body=%s", err, aliases.Body.String())
	}
	if aliasesPayload.Data.Pagination.Total != 1 || aliasesPayload.Data.Pagination.Offset != 0 || aliasesPayload.Data.Pagination.HasMore {
		t.Fatalf("aliases pagination = %#v", aliasesPayload.Data.Pagination)
	}
	if err := assertAdminDirectLinkPath(t, aliasesPayload.Data.Items[0], secure.HashToken(firstKey), env.cipher); err != nil {
		t.Fatal(err)
	}
	for name, response := range map[string]*httptest.ResponseRecorder{"detail": detail, "aliases": aliases} {
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d; body=%s", name, response.Code, response.Body.String())
		}
		for _, forbidden := range []string{firstKey, "APIKeyHash", "api_key_hash", "PasswordCiphertext", "password_ciphertext"} {
			if strings.Contains(response.Body.String(), forbidden) {
				t.Fatalf("GET %s exposed %q: %s", name, forbidden, response.Body.String())
			}
		}
	}

	aliasPath := "/admin/api/v1/aliases/" + strconvFormatInt(aliasID)
	getAlias := env.request(t, http.MethodGet, aliasPath, nil, "", []*http.Cookie{sessionCookie}, "")
	if getAlias.Code != http.StatusOK || strings.Contains(getAlias.Body.String(), firstKey) || strings.Contains(getAlias.Body.String(), "api_key_hash") {
		t.Fatalf("GET alias response = %d; body=%s", getAlias.Code, getAlias.Body.String())
	}
	disable := env.request(t, http.MethodPatch, aliasPath, adminAPITestJSON(t, gin.H{"enabled": false}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if disable.Code != http.StatusOK {
		t.Fatalf("disable alias status = %d; body=%s", disable.Code, disable.Body.String())
	}
	var disabledPayload struct {
		Data adminAPIAliasDTO `json:"data"`
	}
	if err := json.Unmarshal(disable.Body.Bytes(), &disabledPayload); err != nil || disabledPayload.Data.Enabled {
		t.Fatalf("disabled alias payload: err=%v data=%#v", err, disabledPayload.Data)
	}

	rotate := env.request(t, http.MethodPost, aliasPath+"/rotate-key", nil, "", []*http.Cookie{sessionCookie}, csrf)
	if rotate.Code != http.StatusOK || rotate.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("rotate key status/cache = %d %q; body=%s", rotate.Code, rotate.Header().Get("Cache-Control"), rotate.Body.String())
	}
	var rotatePayload struct {
		Data adminAPIOneTimeKeyDTO `json:"data"`
	}
	if err := json.Unmarshal(rotate.Body.Bytes(), &rotatePayload); err != nil || rotatePayload.Data.APIKey == "" || rotatePayload.Data.APIKey == firstKey {
		t.Fatalf("rotated key payload: err=%v data=%#v", err, rotatePayload.Data)
	}
	if rotatePayload.Data.Alias.DirectLinkPath == aliasPayload.Data.Alias.DirectLinkPath {
		t.Fatal("rotating API key did not change the derived direct-link path")
	}
	if err := assertAdminDirectLinkPath(t, rotatePayload.Data.Alias, secure.HashToken(rotatePayload.Data.APIKey), env.cipher); err != nil {
		t.Fatal(err)
	}

	audit := env.request(t, http.MethodGet, "/admin/api/v1/audit?limit=20&offset=0", nil, "", []*http.Cookie{sessionCookie}, "")
	if audit.Code != http.StatusOK || !strings.Contains(audit.Body.String(), `"items"`) || strings.Contains(audit.Body.String(), firstKey) {
		t.Fatalf("audit response = %d; body=%s", audit.Code, audit.Body.String())
	}

	deleteAlias := env.request(t, http.MethodDelete, aliasPath, nil, "", []*http.Cookie{sessionCookie}, csrf)
	if deleteAlias.Code != http.StatusNoContent {
		t.Fatalf("delete alias status = %d; body=%s", deleteAlias.Code, deleteAlias.Body.String())
	}
	deleteAccount := env.request(t, http.MethodDelete, accountPath, nil, "", []*http.Cookie{sessionCookie}, csrf)
	if deleteAccount.Code != http.StatusNoContent {
		t.Fatalf("delete account status = %d; body=%s", deleteAccount.Code, deleteAccount.Body.String())
	}
}

func TestAdminAPIAccountWithAliasesAllowsIMAPUsernameButLocksEmail(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "identity-update-admin", "unused-identity-update-password")
	account := adminAPITestCreateAccount(t, env, "identity-update@icloud.com")
	if _, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID:    account.ID,
		Address:      "identity-update-alias@privaterelay.appleid.com",
		Label:        "Identity update alias",
		APIKeyHash:   secure.HashToken("identity-update-alias-key"),
		APIKeyPrefix: "icm_identity",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create identity update alias: %v", err)
	}

	accountPath := "/admin/api/v1/accounts/" + strconvFormatInt(account.ID)
	updatedUsername := "new-login@icloud.com"
	usernameUpdate := env.request(t, http.MethodPut, accountPath, adminAPITestJSON(t, gin.H{
		"name":          account.Name,
		"email":         account.Email,
		"imap_username": updatedUsername,
		"imap_password": "",
		"enabled":       account.Enabled,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if usernameUpdate.Code != http.StatusOK {
		t.Fatalf("IMAP username update status = %d; body=%s", usernameUpdate.Code, usernameUpdate.Body.String())
	}
	var usernamePayload struct {
		Data adminAPIAccountDTO `json:"data"`
	}
	if err := json.Unmarshal(usernameUpdate.Body.Bytes(), &usernamePayload); err != nil {
		t.Fatalf("decode IMAP username update: %v; body=%s", err, usernameUpdate.Body.String())
	}
	if usernamePayload.Data.IMAPUsername != updatedUsername {
		t.Fatalf("updated IMAP username = %q, want %q", usernamePayload.Data.IMAPUsername, updatedUsername)
	}
	stored, err := env.store.GetAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("reload account after IMAP username update: %v", err)
	}
	if stored.IMAPUsername != updatedUsername {
		t.Fatalf("stored IMAP username = %q, want %q", stored.IMAPUsername, updatedUsername)
	}

	emailUpdate := env.request(t, http.MethodPut, accountPath, adminAPITestJSON(t, gin.H{
		"name":          account.Name,
		"email":         "changed-identity-update@icloud.com",
		"imap_username": updatedUsername,
		"imap_password": "",
		"enabled":       account.Enabled,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if emailUpdate.Code != http.StatusConflict || adminAPITestErrorCode(t, emailUpdate) != "ACCOUNT_IDENTITY_LOCKED" {
		t.Fatalf("primary email update response = %d; body=%s", emailUpdate.Code, emailUpdate.Body.String())
	}
	if !strings.Contains(emailUpdate.Body.String(), "已有隐私邮箱时不能修改主号邮箱") || strings.Contains(emailUpdate.Body.String(), "IMAP 用户名") {
		t.Fatalf("primary email lock message = %s", emailUpdate.Body.String())
	}
}

func TestAdminAPIPendingAliasMutationsReturnConflict(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.SetHMESyncService(&fakeHMESyncService{
		deleteAlias: func(context.Context, int64) error {
			t.Fatal("pending alias reached Apple deletion")
			return nil
		},
	})
	sessionCookie, csrf, _ := env.createSession(t, "pending-alias-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "pending-alias@icloud.com")
	pending, _, err := env.store.CreateAliasWithPendingAPIKey(
		context.Background(),
		domain.AppleWebSession{
			AccountID:     account.ID,
			Ciphertext:    "as1.pending-alias-test",
			AppleID:       account.Email,
			Region:        "global",
			Authenticated: true,
		},
		domain.Alias{
			AccountID:    account.ID,
			Address:      "pending@privaterelay.appleid.com",
			Label:        "Awaiting confirmation",
			APIKeyHash:   secure.HashToken("pending-alias-key"),
			APIKeyPrefix: "icm_pending",
			Enabled:      false,
		},
		"ak1.pending-alias-test",
	)
	if err != nil {
		t.Fatalf("create pending alias: %v", err)
	}

	aliasPath := "/admin/api/v1/aliases/" + strconvFormatInt(pending.ID)
	tests := []struct {
		name   string
		method string
		path   string
		body   []byte
		typeID string
	}{
		{
			name:   "enable",
			method: http.MethodPatch,
			path:   aliasPath,
			body:   adminAPITestJSON(t, gin.H{"enabled": true}),
			typeID: "application/json",
		},
		{name: "rotate key", method: http.MethodPost, path: aliasPath + "/rotate-key"},
		{name: "delete", method: http.MethodDelete, path: aliasPath},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := env.request(t, test.method, test.path, test.body, test.typeID, []*http.Cookie{sessionCookie}, csrf)
			if response.Code != http.StatusConflict || adminAPITestErrorCode(t, response) != "ALIAS_CONFIRMATION_PENDING" {
				t.Fatalf("pending alias response = %d; body=%s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "正在等待 Apple 目录确认") {
				t.Fatalf("pending alias response omitted actionable message: %s", response.Body.String())
			}
		})
	}
}

func assertAdminDirectLinkPath(t *testing.T, alias adminAPIAliasDTO, apiKeyHash []byte, cipher *secure.Cipher) error {
	t.Helper()
	if alias.DirectLinkPath == "" {
		return fmt.Errorf("alias %d omitted direct_link_path", alias.ID)
	}
	parsed, err := url.Parse(alias.DirectLinkPath)
	if err != nil {
		return fmt.Errorf("alias %d direct_link_path parse: %w", alias.ID, err)
	}
	if parsed.IsAbs() || parsed.Path != "/api/v1/mail/recent" {
		return fmt.Errorf("alias %d direct_link_path = %q, want same-origin recent path", alias.ID, alias.DirectLinkPath)
	}
	token := parsed.Query().Get("api_key")
	expectedQuery := url.Values{"api_key": []string{token}}.Encode()
	if parsed.Query().Encode() != expectedQuery || token == "" {
		return fmt.Errorf("alias %d direct_link_path has unexpected query: %q", alias.ID, parsed.RawQuery)
	}
	if !cipher.VerifyDirectLinkToken(token, alias.ID, apiKeyHash) {
		return fmt.Errorf("alias %d direct_link_path credential does not verify", alias.ID)
	}
	return nil
}

func TestAdminAPIListAliasesFiltersByPrimaryAccount(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, _, _ := env.createSession(t, "alias-filter-admin", "alias filter password")
	first := adminAPITestCreateAccount(t, env, "first-filter@icloud.com")
	second := adminAPITestCreateAccount(t, env, "second-filter@icloud.com")

	for index, item := range []struct {
		account domain.Account
		address string
	}{
		{account: first, address: "first-filter-alias@icloud.com"},
		{account: second, address: "second-filter-alias@icloud.com"},
	} {
		if _, err := env.store.CreateAlias(context.Background(), domain.Alias{
			AccountID:    item.account.ID,
			Address:      item.address,
			APIKeyHash:   secure.HashToken(fmt.Sprintf("alias-filter-key-%d", index)),
			APIKeyPrefix: fmt.Sprintf("filter-%d", index),
			Enabled:      true,
		}); err != nil {
			t.Fatalf("create filtered alias %q: %v", item.address, err)
		}
	}

	response := env.request(
		t,
		http.MethodGet,
		"/admin/api/v1/aliases?account_id="+strconvFormatInt(first.ID),
		nil,
		"",
		[]*http.Cookie{sessionCookie},
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("filtered aliases status = %d; body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Items      []adminAPIAliasDTO `json:"items"`
			Pagination struct {
				Offset  int  `json:"offset"`
				Total   int  `json:"total"`
				HasMore bool `json:"has_more"`
			} `json:"pagination"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode filtered aliases: %v; body=%s", err, response.Body.String())
	}
	if len(payload.Data.Items) != 1 || payload.Data.Items[0].AccountID != first.ID || payload.Data.Items[0].Address != "first-filter-alias@icloud.com" {
		t.Fatalf("filtered aliases = %#v, want only first account", payload.Data.Items)
	}
	if payload.Data.Pagination.Total != 1 || payload.Data.Pagination.Offset != 0 || payload.Data.Pagination.HasMore {
		t.Fatalf("filtered aliases pagination = %#v", payload.Data.Pagination)
	}

	invalid := env.request(
		t,
		http.MethodGet,
		"/admin/api/v1/aliases?account_id=not-an-id",
		nil,
		"",
		[]*http.Cookie{sessionCookie},
		"",
	)
	if invalid.Code != http.StatusBadRequest || adminAPITestErrorCode(t, invalid) != "VALIDATION_FAILED" {
		t.Fatalf("invalid account filter status = %d; body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestAdminAPIListAliasesUsesServerPagingAndSearch(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, _, _ := env.createSession(t, "alias-search-admin", "alias search password")
	first := adminAPITestCreateAccount(t, env, "first-alias-search@icloud.com")
	second := adminAPITestCreateAccount(t, env, "second-alias-search@icloud.com")

	for index, item := range []struct {
		account domain.Account
		address string
		label   string
	}{
		{account: first, address: "alpha-api-search@icloud.com", label: "Checkout Inbox"},
		{account: first, address: "unrelated-api-search@icloud.com", label: "Personal"},
		{account: second, address: "bravo-api-search@icloud.com", label: "Checkout Receipts"},
	} {
		if _, err := env.store.CreateAlias(context.Background(), domain.Alias{
			AccountID:    item.account.ID,
			Address:      item.address,
			Label:        item.label,
			APIKeyHash:   secure.HashToken(fmt.Sprintf("alias-search-key-%d", index)),
			APIKeyPrefix: fmt.Sprintf("search-%d", index),
			Enabled:      true,
		}); err != nil {
			t.Fatalf("create searchable alias %q: %v", item.address, err)
		}
	}

	response := env.request(
		t,
		http.MethodGet,
		"/admin/api/v1/aliases?query=CHECKOUT&limit=1&offset=1",
		nil,
		"",
		[]*http.Cookie{sessionCookie},
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("searched aliases status = %d; body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Items      []adminAPIAliasDTO `json:"items"`
			Pagination struct {
				Limit   int  `json:"limit"`
				Offset  int  `json:"offset"`
				Total   int  `json:"total"`
				HasMore bool `json:"has_more"`
			} `json:"pagination"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode searched aliases: %v; body=%s", err, response.Body.String())
	}
	if len(payload.Data.Items) != 1 || payload.Data.Items[0].Address != "bravo-api-search@icloud.com" {
		t.Fatalf("searched aliases = %#v, want second sorted match", payload.Data.Items)
	}
	if pagination := payload.Data.Pagination; pagination.Limit != 1 || pagination.Offset != 1 || pagination.Total != 2 || pagination.HasMore {
		t.Fatalf("searched aliases pagination = %#v", pagination)
	}

	combinedTarget := fmt.Sprintf(
		"/admin/api/v1/aliases?account_id=%d&query=checkout",
		first.ID,
	)
	combined := env.request(t, http.MethodGet, combinedTarget, nil, "", []*http.Cookie{sessionCookie}, "")
	if combined.Code != http.StatusOK {
		t.Fatalf("combined alias filters status = %d; body=%s", combined.Code, combined.Body.String())
	}
	payload = struct {
		Data struct {
			Items      []adminAPIAliasDTO `json:"items"`
			Pagination struct {
				Limit   int  `json:"limit"`
				Offset  int  `json:"offset"`
				Total   int  `json:"total"`
				HasMore bool `json:"has_more"`
			} `json:"pagination"`
		} `json:"data"`
	}{}
	if err := json.Unmarshal(combined.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode combined alias filters: %v; body=%s", err, combined.Body.String())
	}
	if len(payload.Data.Items) != 1 || payload.Data.Items[0].AccountID != first.ID || payload.Data.Pagination.Total != 1 {
		t.Fatalf("combined alias filters = %#v; pagination=%#v", payload.Data.Items, payload.Data.Pagination)
	}

	tooLong := "/admin/api/v1/aliases?query=" + url.QueryEscape(strings.Repeat("界", adminAPIMaxListQueryRunes+1))
	invalid := env.request(t, http.MethodGet, tooLong, nil, "", []*http.Cookie{sessionCookie}, "")
	if invalid.Code != http.StatusBadRequest || adminAPITestErrorCode(t, invalid) != "VALIDATION_FAILED" {
		t.Fatalf("oversized alias query status = %d; body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestAdminAPIListAccountsUsesServerPagingAndSearch(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, _, _ := env.createSession(t, "account-page-admin", "account page password")
	for _, email := range []string{
		"charlie-page-list@icloud.com",
		"alpha-page-list@icloud.com",
		"bravo-page-list@icloud.com",
	} {
		adminAPITestCreateAccount(t, env, email)
	}

	response := env.request(
		t,
		http.MethodGet,
		"/admin/api/v1/accounts?query=PAGE-LIST&limit=1&offset=1",
		nil,
		"",
		[]*http.Cookie{sessionCookie},
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("paged accounts status = %d; body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Items      []adminAPIAccountDTO `json:"items"`
			Pagination struct {
				Limit   int  `json:"limit"`
				Offset  int  `json:"offset"`
				Total   int  `json:"total"`
				HasMore bool `json:"has_more"`
			} `json:"pagination"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode paged accounts: %v; body=%s", err, response.Body.String())
	}
	if len(payload.Data.Items) != 1 || payload.Data.Items[0].Email != "bravo-page-list@icloud.com" {
		t.Fatalf("paged accounts = %#v, want the second sorted account", payload.Data.Items)
	}
	if pagination := payload.Data.Pagination; pagination.Limit != 1 || pagination.Offset != 1 || pagination.Total != 3 || !pagination.HasMore {
		t.Fatalf("accounts pagination = %#v", pagination)
	}

	invalidTargets := []string{
		"/admin/api/v1/accounts?limit=0",
		"/admin/api/v1/accounts?limit=1001",
		"/admin/api/v1/accounts?offset=-1",
		"/admin/api/v1/accounts?offset=1000001",
		"/admin/api/v1/accounts?query=" + url.QueryEscape(strings.Repeat("界", adminAPIMaxListQueryRunes+1)),
	}
	for _, target := range invalidTargets {
		invalid := env.request(t, http.MethodGet, target, nil, "", []*http.Cookie{sessionCookie}, "")
		if invalid.Code != http.StatusBadRequest || adminAPITestErrorCode(t, invalid) != "VALIDATION_FAILED" {
			t.Fatalf("invalid account page %q = %d; body=%s", target, invalid.Code, invalid.Body.String())
		}
	}
}

func TestAdminAPIListEndpointsAcceptConfiguredPageSizes(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, _, _ := env.createSession(t, "page-size-admin", "page size password")

	for _, endpoint := range []string{
		"/admin/api/v1/accounts",
		"/admin/api/v1/aliases",
		"/admin/api/v1/audit",
	} {
		assertLimit := func(target string, want int) {
			t.Helper()
			response := env.request(t, http.MethodGet, target, nil, "", []*http.Cookie{sessionCookie}, "")
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d; body=%s", target, response.Code, response.Body.String())
			}
			var payload struct {
				Data struct {
					Pagination struct {
						Limit int `json:"limit"`
					} `json:"pagination"`
				} `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode GET %s: %v; body=%s", target, err, response.Body.String())
			}
			if payload.Data.Pagination.Limit != want {
				t.Fatalf("GET %s pagination limit = %d, want %d", target, payload.Data.Pagination.Limit, want)
			}
		}

		assertLimit(endpoint, adminAPIDefaultPageLimit)
		for _, limit := range []int{20, 50, 100, 500, 1000} {
			assertLimit(fmt.Sprintf("%s?limit=%d", endpoint, limit), limit)
		}

		response := env.request(t, http.MethodGet, endpoint+"?limit=1001", nil, "", []*http.Cookie{sessionCookie}, "")
		if response.Code != http.StatusBadRequest || adminAPITestErrorCode(t, response) != "VALIDATION_FAILED" {
			t.Fatalf("GET %s with an oversized page = %d; body=%s", endpoint, response.Code, response.Body.String())
		}
	}
}

func TestAdminAPIStrictJSONAndPasswordRevocation(t *testing.T) {
	env := newAdminAPITestEnv(t)
	const oldPassword = "old password for API tests"
	sessionCookie, csrf, admin := env.createSession(t, "password-admin", oldPassword)

	unknownField := env.request(t, http.MethodPost, "/admin/api/v1/accounts", []byte(`{"email":"strict@icloud.com","unknown":true}`), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if unknownField.Code != http.StatusBadRequest || adminAPITestErrorCode(t, unknownField) != "INVALID_JSON" {
		t.Fatalf("unknown JSON field response = %d; body=%s", unknownField.Code, unknownField.Body.String())
	}
	validUnknownField := env.request(t, http.MethodPost, "/admin/api/v1/accounts", adminAPITestJSON(t, gin.H{
		"email":   "strict@icloud.com",
		"unknown": true,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if validUnknownField.Code != http.StatusBadRequest || adminAPITestErrorCode(t, validUnknownField) != "INVALID_JSON" {
		t.Fatalf("valid JSON with unknown field response = %d; body=%s", validUnknownField.Code, validUnknownField.Body.String())
	}
	wrongType := env.request(t, http.MethodPost, "/admin/api/v1/accounts", []byte(`email=strict@icloud.com`), "application/x-www-form-urlencoded", []*http.Cookie{sessionCookie}, csrf)
	if wrongType.Code != http.StatusUnsupportedMediaType || adminAPITestErrorCode(t, wrongType) != "UNSUPPORTED_MEDIA_TYPE" {
		t.Fatalf("wrong media type response = %d; body=%s", wrongType.Code, wrongType.Body.String())
	}
	oversized := env.request(t, http.MethodPost, "/admin/api/v1/accounts", []byte(`{"name":"`+strings.Repeat("x", adminAPIMaxJSONBytes)+`"}`), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if oversized.Code != http.StatusRequestEntityTooLarge || adminAPITestErrorCode(t, oversized) != "REQUEST_TOO_LARGE" {
		t.Fatalf("oversized JSON response = %d; body=%s", oversized.Code, oversized.Body.String())
	}

	const newPassword = "new password for API tests"
	change := env.request(t, http.MethodPut, "/admin/api/v1/auth/password", adminAPITestJSON(t, gin.H{
		"current_password": oldPassword,
		"new_password":     newPassword,
		"confirm_password": newPassword,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if change.Code != http.StatusOK || !strings.Contains(change.Body.String(), `"reauthentication_required":true`) {
		t.Fatalf("change password response = %d; body=%s", change.Code, change.Body.String())
	}
	expired := env.request(t, http.MethodGet, "/admin/api/v1/auth/session", nil, "", []*http.Cookie{sessionCookie}, "")
	if expired.Code != http.StatusUnauthorized || adminAPITestErrorCode(t, expired) != "SESSION_EXPIRED" {
		t.Fatalf("old session after password change = %d; body=%s", expired.Code, expired.Body.String())
	}
	updated, err := env.store.GetAdminByID(context.Background(), admin.ID)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(updated.PasswordHash), []byte(newPassword)) != nil {
		t.Fatalf("new password was not persisted: lookup=%v", err)
	}
}

func TestAdminAPIAccountInputValidatesAndDefaultsIMAPEndpoint(t *testing.T) {
	account, _, message := adminAPIAccountInput(
		"Primary", "owner@icloud.com", "owner@icloud.com", "app-password", nil, nil,
		domain.Account{Enabled: true},
	)
	if message != "" {
		t.Fatalf("default endpoint validation message = %q", message)
	}
	if account.IMAPHost != domain.DefaultIMAPHost || account.IMAPPort != domain.DefaultIMAPPort {
		t.Fatalf("default endpoint = %q:%d", account.IMAPHost, account.IMAPPort)
	}

	host := " IMAP.Example.Test. "
	port := 1993
	account, _, message = adminAPIAccountInput(
		"Primary", "owner@icloud.com", "owner@icloud.com", "app-password", &host, &port,
		domain.Account{Enabled: true},
	)
	if message != "" || account.IMAPHost != "imap.example.test" || account.IMAPPort != 1993 {
		t.Fatalf("custom endpoint = %#v, message=%q", account, message)
	}

	invalidHost := "https://imap.example.test"
	account, _, message = adminAPIAccountInput(
		"Primary", "owner@icloud.com", "owner@icloud.com", "app-password", &invalidHost, nil,
		domain.Account{Enabled: true},
	)
	if message == "" {
		t.Fatalf("invalid endpoint was accepted: %#v", account)
	}
}

func strconvFormatInt(value int64) string {
	return strconv.FormatInt(value, 10)
}

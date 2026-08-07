package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	for _, forbidden := range []string{appPassword, "PasswordCiphertext", "password_ciphertext"} {
		if strings.Contains(createAccount.Body.String(), forbidden) {
			t.Fatalf("account response exposed %q: %s", forbidden, createAccount.Body.String())
		}
	}

	accountPath := "/admin/api/v1/accounts/" + strconvFormatInt(accountPayload.Data.ID)
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
	if strings.Contains(createAlias.Body.String(), "APIKeyHash") || strings.Contains(createAlias.Body.String(), "api_key_hash") {
		t.Fatalf("create alias exposed API key hash: %s", createAlias.Body.String())
	}

	detail := env.request(t, http.MethodGet, accountPath, nil, "", []*http.Cookie{sessionCookie}, "")
	aliases := env.request(t, http.MethodGet, "/admin/api/v1/aliases", nil, "", []*http.Cookie{sessionCookie}, "")
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

func strconvFormatInt(value int64) string {
	return strconv.FormatInt(value, 10)
}

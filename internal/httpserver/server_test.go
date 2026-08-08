package httpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"icloud-api/internal/config"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

const testAdminSPAIndex = `<!doctype html><html><body><div id="spa-test-marker"></div></body></html>`

func TestAdminSPARoutesAndSecurityHeaders(t *testing.T) {
	env := newAdminSPAHTTPTestEnv(t)

	root := env.request(t, http.MethodGet, "/", nil, nil)
	if root.Code != http.StatusFound || root.Header().Get("Location") != "/admin/" {
		t.Fatalf("GET / = %d Location=%q, want %d /admin/", root.Code, root.Header().Get("Location"), http.StatusFound)
	}
	rootHead := env.request(t, http.MethodHead, "/", nil, nil)
	if rootHead.Code != http.StatusFound || rootHead.Header().Get("Location") != "/admin/" || rootHead.Body.Len() != 0 {
		t.Fatalf("HEAD / = %d Location=%q body=%q, want empty %d /admin/", rootHead.Code, rootHead.Header().Get("Location"), rootHead.Body.String(), http.StatusFound)
	}
	admin := env.request(t, http.MethodGet, "/admin", nil, nil)
	if admin.Code != http.StatusPermanentRedirect || admin.Header().Get("Location") != "/admin/" {
		t.Fatalf("GET /admin = %d Location=%q, want %d /admin/", admin.Code, admin.Header().Get("Location"), http.StatusPermanentRedirect)
	}

	for _, target := range []string{
		"/admin/",
		"/admin/login",
		"/admin/accounts/new",
		"/admin/accounts/42",
		"/admin/accounts/42/edit",
		"/admin/aliases",
		"/admin/audit",
		"/admin/security",
	} {
		t.Run(target, func(t *testing.T) {
			response := env.request(t, http.MethodGet, target, nil, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want %d; body=%s", target, response.Code, http.StatusOK, response.Body.String())
			}
			if response.Body.String() != testAdminSPAIndex {
				t.Fatalf("GET %s did not return the Vue index: %s", target, response.Body.String())
			}
			if got := response.Header().Get("Cache-Control"); got != "no-store, private" {
				t.Fatalf("GET %s Cache-Control = %q", target, got)
			}
			if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
				t.Fatalf("GET %s Content-Type = %q", target, got)
			}
		})
	}

	head := env.request(t, http.MethodHead, "/admin/accounts/42", nil, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD SPA route = %d body=%q, want empty 200", head.Code, head.Body.String())
	}

	csp := admin.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") || !strings.Contains(csp, "font-src 'self' data:") {
		t.Fatalf("Vue CSP is missing Element Plus allowances: %q", csp)
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "script-src 'self' 'unsafe-eval'") {
		t.Fatalf("Vue CSP weakened the script policy: %q", csp)
	}
}

func TestAdminSPAAssetsAndFallbackBoundaries(t *testing.T) {
	env := newAdminSPAHTTPTestEnv(t)

	asset := env.request(t, http.MethodGet, "/admin/assets/app-test.js", nil, nil)
	if asset.Code != http.StatusOK || asset.Body.String() != "window.__SPA_TEST__ = true;\n" {
		t.Fatalf("GET SPA asset = %d body=%q", asset.Code, asset.Body.String())
	}
	if got := asset.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("SPA asset Cache-Control = %q", got)
	}
	if got := asset.Header().Get("Pragma"); got != "" {
		t.Fatalf("SPA asset Pragma = %q, want empty", got)
	}

	assetHead := env.request(t, http.MethodHead, "/admin/assets/app-test.js", nil, nil)
	if assetHead.Code != http.StatusOK || assetHead.Body.Len() != 0 {
		t.Fatalf("HEAD SPA asset = %d body=%q, want empty 200", assetHead.Code, assetHead.Body.String())
	}

	for _, target := range []string{"/admin/assets", "/admin/assets/", "/admin/assets/missing.js"} {
		response := env.request(t, http.MethodGet, target, nil, nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", target, response.Code)
		}
		if strings.Contains(response.Header().Get("Cache-Control"), "immutable") || strings.Contains(response.Body.String(), "spa-test-marker") {
			t.Fatalf("GET %s cached or returned the SPA index: headers=%v body=%s", target, response.Header(), response.Body.String())
		}
	}

	for _, target := range []string{"/admin/api", "/admin/api/v1/not-found", "/api", "/api/v1/not-found"} {
		response := env.request(t, http.MethodGet, target, nil, nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", target, response.Code)
		}
		if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("GET %s Content-Type = %q, want JSON; body=%s", target, got, response.Body.String())
		}
		if got := response.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Fatalf("GET %s Cache-Control = %q, want no-store", target, got)
		}
		assertAPIError(t, response, http.StatusNotFound, "NOT_FOUND")
	}

	post := env.request(t, http.MethodPost, "/admin/login", nil, nil)
	if post.Code != http.StatusNotFound || strings.Contains(post.Body.String(), "spa-test-marker") {
		t.Fatalf("POST legacy admin route = %d body=%s, want non-SPA 404", post.Code, post.Body.String())
	}
}

func TestAdminSPARequiresCompleteBuild(t *testing.T) {
	root := t.TempDir()
	if _, err := loadAdminSPA(root); err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("loadAdminSPA without index error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte(testAdminSPAIndex), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAdminSPA(root); err == nil || !strings.Contains(err.Error(), "assets") {
		t.Fatalf("loadAdminSPA without assets error = %v", err)
	}
}

func TestAdminWithoutSessionRedirectsToLogin(t *testing.T) {
	env := newHTTPTestEnv(t)

	response := env.request(t, http.MethodGet, "/admin", nil, nil)
	if response.Code != http.StatusFound {
		t.Fatalf("GET /admin status = %d, want %d", response.Code, http.StatusFound)
	}
	if location := response.Header().Get("Location"); location != "/admin/login" {
		t.Fatalf("GET /admin Location = %q, want %q", location, "/admin/login")
	}
}

func TestRouterMountsAdminJSONAPI(t *testing.T) {
	env := newHTTPTestEnv(t)

	response := env.request(t, http.MethodGet, "/admin/api/v1/auth/csrf", nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET admin API CSRF status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("GET admin API CSRF Content-Type = %q, want application/json", contentType)
	}
	if cookie := requireResponseCookie(t, response, adminAPILoginCSRFCookie); cookie.Path != adminAPILoginCSRFPath {
		t.Fatalf("admin API CSRF cookie Path = %q, want %q", cookie.Path, adminAPILoginCSRFPath)
	}
}

func TestAdminLoginRequiresCSRFAndCorrectPassword(t *testing.T) {
	env := newHTTPTestEnv(t)
	const (
		username = "admin"
		password = "correct horse battery staple"
	)
	env.createAdmin(t, username, password)

	t.Run("missing csrf", func(t *testing.T) {
		csrfCookie := env.loginCSRFCookie(t)
		response := env.request(t, http.MethodPost, "/admin/login", url.Values{
			"username": {username},
			"password": {password},
		}, []*http.Cookie{csrfCookie})
		if response.Code != http.StatusForbidden {
			t.Fatalf("login without CSRF status = %d, want %d", response.Code, http.StatusForbidden)
		}
		assertNoResponseCookie(t, response, sessionCookie)
	})

	t.Run("wrong password", func(t *testing.T) {
		csrfCookie := env.loginCSRFCookie(t)
		response := env.request(t, http.MethodPost, "/admin/login", url.Values{
			"csrf_token": {csrfCookie.Value},
			"username":   {username},
			"password":   {"not-the-password"},
		}, []*http.Cookie{csrfCookie})
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("login with wrong password status = %d, want %d", response.Code, http.StatusUnauthorized)
		}
		assertNoResponseCookie(t, response, sessionCookie)
	})

	t.Run("valid credentials", func(t *testing.T) {
		csrfCookie := env.loginCSRFCookie(t)
		response := env.request(t, http.MethodPost, "/admin/login", url.Values{
			"csrf_token": {csrfCookie.Value},
			"username":   {username},
			"password":   {password},
		}, []*http.Cookie{csrfCookie})
		if response.Code != http.StatusFound {
			t.Fatalf("valid login status = %d, want %d", response.Code, http.StatusFound)
		}
		if location := response.Header().Get("Location"); location != "/admin" {
			t.Fatalf("valid login Location = %q, want %q", location, "/admin")
		}
		cookie := requireResponseCookie(t, response, sessionCookie)
		if cookie.Value == "" || !cookie.HttpOnly || cookie.Path != "/admin" {
			t.Fatalf("session cookie has unexpected attributes: %#v", cookie)
		}
	})
}

func TestLoginCSRFFailureReason(t *testing.T) {
	const token = "matching-login-csrf-token"
	tests := []struct {
		name      string
		provided  string
		cookie    string
		host      string
		origin    string
		fetchSite string
		want      string
	}{
		{name: "missing cookie", provided: token, want: "cookie_missing"},
		{name: "missing form token", cookie: token, want: "form_token_missing"},
		{name: "token mismatch", provided: "Matching-login-csrf-token", cookie: token, want: "token_mismatch"},
		{name: "cross-site fetch", provided: token, cookie: token, fetchSite: "cross-site", want: "fetch_site_cross_site"},
		{name: "invalid origin", provided: token, cookie: token, origin: "null", want: "origin_invalid"},
		{name: "origin with path", provided: token, cookie: token, origin: "https://example.com/path", want: "origin_invalid"},
		{name: "origin with user info", provided: token, cookie: token, origin: "https://user@example.com", want: "origin_invalid"},
		{name: "origin with query", provided: token, cookie: token, origin: "https://example.com?query", want: "origin_invalid"},
		{name: "non-http origin", provided: token, cookie: token, origin: "ftp://example.com", want: "origin_invalid"},
		{name: "origin host mismatch", provided: token, cookie: token, host: "internal:8080", origin: "https://admin.example.com", want: "origin_host_mismatch"},
		{name: "same origin", provided: token, cookie: token, host: "admin.example.com", origin: "https://admin.example.com"},
		{name: "missing origin", provided: token, cookie: token},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := test.host
			if host == "" {
				host = "example.com"
			}
			request := httptest.NewRequest(http.MethodPost, "http://"+host+"/admin/login", nil)
			if test.cookie != "" {
				request.AddCookie(&http.Cookie{Name: loginCSRFCookie, Value: test.cookie})
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.fetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			}
			if got := loginCSRFFailureReason(request, test.provided); got != test.want {
				t.Fatalf("loginCSRFFailureReason() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLoginCSRFFailureLogExcludesSecrets(t *testing.T) {
	const (
		username = "diagnostic-admin-secret"
		password = "diagnostic-password-secret"
	)
	tests := []struct {
		name        string
		cookie      string
		formToken   string
		host        string
		origin      string
		wantReason  string
		wantCookies float64
	}{
		{
			name:       "missing cookie",
			formToken:  "missing-cookie-form-token-secret",
			wantReason: loginCSRFCookieMissing,
		},
		{
			name:        "token mismatch",
			cookie:      "mismatch-cookie-token-secret",
			formToken:   "mismatch-form-token-secret--",
			wantReason:  loginCSRFTokenMismatch,
			wantCookies: 1,
		},
		{
			name:        "origin host mismatch",
			cookie:      "origin-cookie-token-secret",
			formToken:   "origin-cookie-token-secret",
			host:        "internal-host-secret:8080",
			origin:      "https://public-origin-secret.example",
			wantReason:  loginCSRFOriginHostMismatch,
			wantCookies: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newHTTPTestEnv(t)
			var logs bytes.Buffer
			env.server.logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

			host := test.host
			if host == "" {
				host = "login-host-secret.example"
			}
			form := url.Values{
				"csrf_token": {test.formToken},
				"username":   {username},
				"password":   {password},
			}
			request := httptest.NewRequest(http.MethodPost, "http://"+host+"/admin/login", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if test.cookie != "" {
				request.AddCookie(&http.Cookie{Name: loginCSRFCookie, Value: test.cookie})
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			env.router.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("login CSRF failure status = %d, want %d", response.Code, http.StatusForbidden)
			}

			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("parse login CSRF log: %v; output=%s", err, logs.String())
			}
			if got := entry["reason"]; got != test.wantReason {
				t.Fatalf("login CSRF log reason = %#v, want %q", got, test.wantReason)
			}
			if got := entry["cookie_count"]; got != test.wantCookies {
				t.Fatalf("login CSRF log cookie_count = %#v, want %.0f", got, test.wantCookies)
			}
			if got, want := entry["request_id"], response.Header().Get("X-Request-ID"); got != want {
				t.Fatalf("login CSRF log request_id = %#v, want %q", got, want)
			}

			output := logs.String()
			for _, secret := range []string{username, password, test.formToken, test.cookie, host, test.origin} {
				if secret != "" && strings.Contains(output, secret) {
					t.Fatalf("login CSRF log exposed secret %q: %s", secret, output)
				}
			}
			if strings.Contains(response.Body.String(), test.wantReason) {
				t.Fatal("login CSRF response exposed internal failure reason")
			}
		})
	}
}

func TestSuccessfulLoginDeletesExpiredSessions(t *testing.T) {
	env := newHTTPTestEnv(t)
	const (
		username = "cleanup-admin"
		password = "correct horse battery staple"
	)
	admin := env.createAdmin(t, username, password)
	expiredHash := secure.HashToken("expired-session")
	if err := env.store.CreateSession(context.Background(), expiredHash, domain.Session{
		AdminID:         admin.ID,
		Username:        admin.Username,
		PasswordVersion: admin.PasswordVersion,
		CSRF:            "expired-csrf",
		ExpiresAt:       time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create expired session: %v", err)
	}

	csrfCookie := env.loginCSRFCookie(t)
	response := env.request(t, http.MethodPost, "/admin/login", url.Values{
		"csrf_token": {csrfCookie.Value},
		"username":   {username},
		"password":   {password},
	}, []*http.Cookie{csrfCookie})
	if response.Code != http.StatusFound {
		t.Fatalf("valid login status = %d, want %d", response.Code, http.StatusFound)
	}

	var count int
	if err := env.store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM admin_sessions WHERE token_hash = ?`, expiredHash,
	).Scan(&count); err != nil {
		t.Fatalf("count expired sessions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expired session count = %d, want 0", count)
	}
}

func TestAPIKeyReturnsOnlyOwnedLatestMessageAndIgnoresAliasQuery(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	target := "/api/v1/mail/latest?alias=" + url.QueryEscape(mailboxes.aliasB.Address) +
		"&mailbox=" + url.QueryEscape(mailboxes.accountB.Email)

	response := env.apiRequest(t, target, mailboxes.keyA)
	if response.Code != http.StatusOK {
		t.Fatalf("latest mail status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	body := response.Body.Bytes()
	for _, forbidden := range []string{mailboxes.aliasB.Address, "B private subject", "B private body"} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("key A response exposed B data %q: %s", forbidden, body)
		}
	}

	data := decodeObjectField(t, body, "data")
	if got := decodeStringField(t, data, "alias"); got != mailboxes.aliasA.Address {
		t.Fatalf("response alias = %q, want %q", got, mailboxes.aliasA.Address)
	}
	message := decodeNestedObjectField(t, data, "message")
	if got := decodeStringField(t, message, "subject"); got != "A newest subject" {
		t.Fatalf("response subject = %q, want newest A message", got)
	}
	if got := decodeStringField(t, message, "id"); got != "101-12" {
		t.Fatalf("response message id = %q, want %q", got, "101-12")
	}
	if _, exists := data["messages"]; exists {
		t.Fatal("latest endpoint returned a messages collection instead of one message object")
	}
}

func TestHealthyAliasAPIStillSucceedsWhenSiblingIsUnknown(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createSiblingMailboxFixture(t)
	now := time.Now().UTC()
	if err := env.store.UpdateAliasSyncStatus(
		context.Background(), mailboxes.aliasA.ID, domain.SyncStatusError,
		"latest message status could not be confirmed", &now,
	); err != nil {
		t.Fatalf("mark alias A unknown: %v", err)
	}
	if err := env.store.UpdateAccountSyncStatus(
		context.Background(), mailboxes.accountA.ID, domain.SyncStatusError,
		"one alias is unknown", &now,
	); err != nil {
		t.Fatalf("mark shared account degraded: %v", err)
	}

	response := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyB)
	if response.Code != http.StatusOK {
		t.Fatalf("healthy sibling status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	data := decodeObjectField(t, response.Body.Bytes(), "data")
	if got := decodeStringField(t, data, "alias"); got != mailboxes.aliasB.Address {
		t.Fatalf("healthy sibling response alias = %q, want %q", got, mailboxes.aliasB.Address)
	}
}

func TestUnknownAliasAPIReturnsServiceUnavailable(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createSiblingMailboxFixture(t)
	now := time.Now().UTC()
	if err := env.store.UpdateAliasSyncStatus(
		context.Background(), mailboxes.aliasA.ID, domain.SyncStatusError,
		"latest message status could not be confirmed", &now,
	); err != nil {
		t.Fatalf("mark alias A unknown: %v", err)
	}

	response := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
	assertAPIError(t, response, http.StatusServiceUnavailable, "SYNC_UNAVAILABLE")
	if strings.Contains(response.Body.String(), "A retained snapshot") {
		t.Fatal("unknown alias response exposed its retained snapshot")
	}
}

func TestLatestMailResponseContainsSingleObjects(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)

	response := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
	if response.Code != http.StatusOK {
		t.Fatalf("latest mail status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response envelope: %v", err)
	}
	dataRaw := bytes.TrimSpace(envelope["data"])
	if len(dataRaw) == 0 || dataRaw[0] != '{' {
		t.Fatalf("data JSON = %s, want one object", dataRaw)
	}
	data := decodeObjectField(t, response.Body.Bytes(), "data")
	messageRaw := bytes.TrimSpace(data["message"])
	if len(messageRaw) == 0 || messageRaw[0] != '{' {
		t.Fatalf("message JSON = %s, want one object", messageRaw)
	}
}

func TestLatestMailRejectsInvalidAndDisabledKeys(t *testing.T) {
	t.Run("invalid key", func(t *testing.T) {
		env := newHTTPTestEnv(t)
		env.createMailboxFixture(t)
		for name, key := range map[string]string{
			"missing":   "",
			"malformed": "not-an-icloud-key",
			"unknown":   testAPIKey(0x7f),
		} {
			t.Run(name, func(t *testing.T) {
				response := env.apiRequest(t, "/api/v1/mail/latest", key)
				assertAPIError(t, response, http.StatusUnauthorized, "INVALID_API_KEY")
			})
		}
	})

	t.Run("disabled alias", func(t *testing.T) {
		env := newHTTPTestEnv(t)
		mailboxes := env.createMailboxFixture(t)
		mailboxes.aliasA.Enabled = false
		if _, err := env.store.UpdateAlias(context.Background(), mailboxes.aliasA); err != nil {
			t.Fatalf("disable alias A: %v", err)
		}

		response := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
		assertAPIError(t, response, http.StatusUnauthorized, "INVALID_API_KEY")
	})

	t.Run("disabled account", func(t *testing.T) {
		env := newHTTPTestEnv(t)
		mailboxes := env.createMailboxFixture(t)
		mailboxes.accountA.Enabled = false
		if _, err := env.store.UpdateAccount(context.Background(), mailboxes.accountA); err != nil {
			t.Fatalf("disable account A: %v", err)
		}

		response := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
		assertAPIError(t, response, http.StatusUnauthorized, "INVALID_API_KEY")
	})
}

func TestLatestMailReportsDatabaseFailureSeparatelyFromInvalidKey(t *testing.T) {
	env := newHTTPTestEnv(t)
	fixture := env.createMailboxFixture(t)
	if err := env.store.Close(); err != nil {
		t.Fatalf("close test database: %v", err)
	}

	response := env.apiRequest(t, "/api/v1/mail/latest", fixture.keyA)
	assertAPIError(t, response, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE")
}

func TestAPIKeyFormatIsStrictBeforeMailboxBindingLookup(t *testing.T) {
	invalidKeys := map[string]string{
		"secret too short":        "icm_" + strings.Repeat("A", 42),
		"secret too long":         "icm_" + strings.Repeat("A", 44),
		"standard base64 +":       "icm_" + strings.Repeat("A", 42) + "+",
		"standard base64 /":       "icm_" + strings.Repeat("A", 42) + "/",
		"padding":                 "icm_" + strings.Repeat("A", 42) + "=",
		"punctuation":             "icm_" + strings.Repeat("A", 42) + ".",
		"non-canonical base64url": "icm_" + strings.Repeat("A", 42) + "B",
	}
	for name, key := range invalidKeys {
		t.Run(name, func(t *testing.T) {
			env := newHTTPTestEnv(t)
			mailboxes := env.createMailboxFixture(t)
			if _, err := env.store.RotateAliasAPIKey(
				context.Background(), mailboxes.aliasA.ID, secure.HashToken(key), "invalid-key",
			); err != nil {
				t.Fatalf("store malformed matching key: %v", err)
			}

			response := env.apiRequest(t, "/api/v1/mail/latest", key)
			assertAPIError(t, response, http.StatusUnauthorized, "INVALID_API_KEY")
			alias, err := env.store.GetAlias(context.Background(), mailboxes.aliasA.ID)
			if err != nil {
				t.Fatalf("reload alias after malformed key request: %v", err)
			}
			if alias.LastAccessedAt != nil {
				t.Fatalf("malformed key reached mailbox handler; last_accessed_at=%s", alias.LastAccessedAt)
			}
		})
	}
}

func TestWindowLimiterBoundsHighCardinalityKeys(t *testing.T) {
	limiter := newWindowLimiter(1, time.Hour)
	for index := 0; index < limiter.maxItems; index++ {
		if !limiter.Allow(fmt.Sprintf("source-%d", index)) {
			t.Fatalf("source %d was rejected before capacity", index)
		}
	}
	if limiter.Allow("one-source-too-many") {
		t.Fatal("new source was accepted after capacity")
	}
	if len(limiter.items) != limiter.maxItems {
		t.Fatalf("limiter item count = %d, want %d", len(limiter.items), limiter.maxItems)
	}
	if limiter.Allow("source-0") {
		t.Fatal("existing source exceeded its request limit")
	}
}

func TestReenablingAliasClearsSnapshotAndSetsPending(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	cookie := env.createAdminSession(t)
	target := fmt.Sprintf("/admin/aliases/%d/toggle", mailboxes.aliasA.ID)
	form := url.Values{"csrf_token": {testSessionCSRF}}

	response := env.request(t, http.MethodPost, target, form, []*http.Cookie{cookie})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("disable alias status = %d, want %d; body=%s", response.Code, http.StatusSeeOther, response.Body.String())
	}
	response = env.request(t, http.MethodPost, target, form, []*http.Cookie{cookie})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("re-enable alias status = %d, want %d; body=%s", response.Code, http.StatusSeeOther, response.Body.String())
	}

	alias, err := env.store.GetAlias(context.Background(), mailboxes.aliasA.ID)
	if err != nil {
		t.Fatalf("reload re-enabled alias: %v", err)
	}
	if !alias.Enabled || alias.LastSyncStatus != domain.SyncStatusPending || alias.LastSyncError != "" || alias.LastSyncedAt != nil {
		t.Fatalf("re-enabled alias state = %#v, want enabled pending with no previous sync metadata", alias)
	}
	if _, err := env.store.GetLatestMessage(context.Background(), mailboxes.aliasA.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("re-enabled alias retained old snapshot: %v", err)
	}
	apiResponse := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
	assertAPIError(t, apiResponse, http.StatusServiceUnavailable, "SYNC_UNAVAILABLE")
	if strings.Contains(apiResponse.Body.String(), "A newest subject") {
		t.Fatal("re-enabled alias API exposed its cleared snapshot")
	}
}

func TestUpdatingIMAPPasswordSetsEveryAliasPending(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createSiblingMailboxFixture(t)
	cookie := env.createAdminSession(t)
	const newPassword = "new-imap-app-password"
	response := env.request(t, http.MethodPost,
		fmt.Sprintf("/admin/accounts/%d", mailboxes.accountA.ID),
		url.Values{
			"csrf_token":    {testSessionCSRF},
			"name":          {mailboxes.accountA.Name},
			"email":         {mailboxes.accountA.Email},
			"imap_username": {mailboxes.accountA.IMAPUsername},
			"imap_password": {newPassword},
			"enabled":       {"1"},
		}, []*http.Cookie{cookie})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("update IMAP password status = %d, want %d; body=%s", response.Code, http.StatusSeeOther, response.Body.String())
	}

	aliases, err := env.store.ListAliasesByAccount(context.Background(), mailboxes.accountA.ID)
	if err != nil {
		t.Fatalf("list aliases after password update: %v", err)
	}
	if len(aliases) != 2 {
		t.Fatalf("alias count after password update = %d, want 2", len(aliases))
	}
	for _, alias := range aliases {
		if alias.LastSyncStatus != domain.SyncStatusPending || alias.LastSyncError != "" || alias.LastSyncedAt != nil {
			t.Errorf("alias %q state after password update = %#v, want pending", alias.Address, alias)
		}
		if _, err := env.store.GetLatestMessage(context.Background(), alias.ID); err != nil {
			t.Errorf("alias %q lost its pending same-generation snapshot after password update: %v", alias.Address, err)
		}
	}
	account, err := env.store.GetAccount(context.Background(), mailboxes.accountA.ID)
	if err != nil {
		t.Fatalf("reload account after password update: %v", err)
	}
	if account.LastSyncStatus != domain.SyncStatusPending || account.LastSyncError != "" || account.LastSyncedAt != nil {
		t.Fatalf("account sync state after password update = %#v, want pending", account)
	}
	decrypted, err := env.cipher.Decrypt(account.PasswordCiphertext)
	if err != nil {
		t.Fatalf("decrypt updated IMAP password: %v", err)
	}
	if decrypted != newPassword {
		t.Fatalf("updated IMAP password = %q, want %q", decrypted, newPassword)
	}
	for _, key := range []string{mailboxes.keyA, mailboxes.keyB} {
		apiResponse := env.apiRequest(t, "/api/v1/mail/latest", key)
		assertAPIError(t, apiResponse, http.StatusServiceUnavailable, "SYNC_UNAVAILABLE")
		if strings.Contains(apiResponse.Body.String(), "snapshot") {
			t.Fatal("API exposed a retained internal snapshot while IMAP resync was pending")
		}
	}
}

func TestAdminPagesDoNotEchoIMAPPassword(t *testing.T) {
	env := newHTTPTestEnv(t)
	const plaintext = "imap-app-password-must-stay-secret"
	ciphertext, err := env.cipher.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt fixture password: %v", err)
	}
	account := env.createAccount(t, "Primary", "primary@icloud.com", ciphertext)
	sessionCookie := env.createAdminSession(t)

	for _, target := range []string{
		fmt.Sprintf("/admin/accounts/%d", account.ID),
		fmt.Sprintf("/admin/accounts/%d/edit", account.ID),
	} {
		t.Run(target, func(t *testing.T) {
			response := env.request(t, http.MethodGet, target, nil, []*http.Cookie{sessionCookie})
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d; body=%s", target, response.Code, http.StatusOK, response.Body.String())
			}
			body := response.Body.String()
			if strings.Contains(body, plaintext) {
				t.Fatalf("GET %s echoed plaintext IMAP password", target)
			}
			if strings.Contains(body, ciphertext) {
				t.Fatalf("GET %s echoed stored IMAP credential", target)
			}
			if strings.HasSuffix(target, "/edit") {
				passwordInput := regexp.MustCompile(`<input[^>]*name="imap_password"[^>]*>`).FindString(body)
				if passwordInput == "" {
					t.Fatalf("GET %s did not render the password input", target)
				}
				if regexp.MustCompile(`\svalue\s*=`).MatchString(passwordInput) {
					t.Fatalf("GET %s prefilled the password input: %s", target, passwordInput)
				}
			}
		})
	}
}

type httpTestEnv struct {
	store  *store.Store
	cipher *secure.Cipher
	server *Server
	router http.Handler
}

const testSessionCSRF = "test-session-csrf"

func newHTTPTestEnv(t *testing.T) *httpTestEnv {
	return newHTTPTestEnvWithWebRoot(t, "")
}

func newAdminSPAHTTPTestEnv(t *testing.T) *httpTestEnv {
	t.Helper()
	webRoot := t.TempDir()
	assetsRoot := filepath.Join(webRoot, "assets")
	if err := os.MkdirAll(assetsRoot, 0o700); err != nil {
		t.Fatalf("create SPA assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte(testAdminSPAIndex), 0o600); err != nil {
		t.Fatalf("write SPA index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(assetsRoot, "app-test.js"), []byte("window.__SPA_TEST__ = true;\n"), 0o600); err != nil {
		t.Fatalf("write SPA asset: %v", err)
	}
	return newHTTPTestEnvWithWebRoot(t, webRoot)
}

func newHTTPTestEnvWithWebRoot(t *testing.T, webRoot string) *httpTestEnv {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "httpserver-test.db")
	db, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("open temporary SQLite store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close temporary SQLite store: %v", err)
		}
	})

	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatalf("create test cipher: %v", err)
	}
	server, err := New(db, cipher, config.Config{
		CookieSecure: false,
		SessionTTL:   time.Hour,
		PollInterval: time.Minute,
		GinMode:      "test",
		WebRoot:      webRoot,
		OAuthToken:   "external-oauth-test-token-0123456789abcdef",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("create HTTP server: %v", err)
	}
	router, err := server.Router()
	if err != nil {
		t.Fatalf("create HTTP router: %v", err)
	}
	return &httpTestEnv{store: db, cipher: cipher, server: server, router: router}
}

func (e *httpTestEnv) request(
	t *testing.T,
	method string,
	target string,
	form url.Values,
	cookies []*http.Cookie,
) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request := httptest.NewRequest(method, target, body)
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	e.router.ServeHTTP(response, request)
	return response
}

func (e *httpTestEnv) apiRequest(t *testing.T, target, key string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response := httptest.NewRecorder()
	e.router.ServeHTTP(response, request)
	return response
}

func (e *httpTestEnv) createAdmin(t *testing.T, username, password string) domain.Admin {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash admin password: %v", err)
	}
	admin, err := e.store.CreateAdmin(context.Background(), username, string(hash))
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	return admin
}

func (e *httpTestEnv) loginCSRFCookie(t *testing.T) *http.Cookie {
	t.Helper()
	response := e.request(t, http.MethodGet, "/admin/login", nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /admin/login status = %d, want %d", response.Code, http.StatusOK)
	}
	return requireResponseCookie(t, response, loginCSRFCookie)
}

func (e *httpTestEnv) createAdminSession(t *testing.T) *http.Cookie {
	t.Helper()
	admin := e.createAdmin(t, "page-admin", "unused-test-password")
	const rawToken = "test-admin-session-token"
	if err := e.store.CreateSession(context.Background(), secure.HashToken(rawToken), domain.Session{
		AdminID:         admin.ID,
		Username:        admin.Username,
		PasswordVersion: admin.PasswordVersion,
		CSRF:            testSessionCSRF,
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	return &http.Cookie{Name: sessionCookie, Value: rawToken, Path: "/admin"}
}

func (e *httpTestEnv) createAccount(t *testing.T, name, email, ciphertext string) domain.Account {
	t.Helper()
	syncedAt := time.Now().UTC()
	account, err := e.store.CreateAccount(context.Background(), domain.Account{
		Name:               name,
		Email:              email,
		IMAPHost:           "imap.mail.me.com",
		IMAPPort:           993,
		IMAPUsername:       email,
		PasswordCiphertext: ciphertext,
		Enabled:            true,
		LastSyncStatus:     domain.SyncStatusOK,
		LastSyncedAt:       &syncedAt,
	})
	if err != nil {
		t.Fatalf("create account %q: %v", email, err)
	}
	return account
}

type mailboxFixture struct {
	keyA     string
	keyB     string
	accountA domain.Account
	accountB domain.Account
	aliasA   domain.Alias
	aliasB   domain.Alias
}

func (e *httpTestEnv) createMailboxFixture(t *testing.T) mailboxFixture {
	t.Helper()
	ctx := context.Background()
	fixture := mailboxFixture{
		keyA: testAPIKey(0x11),
		keyB: testAPIKey(0xfb),
	}
	fixture.accountA = e.createAccount(t, "Primary A", "primary-a@icloud.com", "encrypted-a")
	fixture.accountB = e.createAccount(t, "Primary B", "primary-b@icloud.com", "encrypted-b")
	aliasSyncedAt := time.Now().UTC()

	var err error
	fixture.aliasA, err = e.store.CreateAlias(ctx, domain.Alias{
		AccountID:      fixture.accountA.ID,
		Address:        "relay-a@icloud.com",
		Label:          "Alias A",
		APIKeyHash:     secure.HashToken(fixture.keyA),
		APIKeyPrefix:   fixture.keyA[:12],
		Enabled:        true,
		LastSyncStatus: domain.SyncStatusOK,
		LastSyncedAt:   &aliasSyncedAt,
	})
	if err != nil {
		t.Fatalf("create alias A: %v", err)
	}
	fixture.aliasB, err = e.store.CreateAlias(ctx, domain.Alias{
		AccountID:      fixture.accountB.ID,
		Address:        "relay-b@icloud.com",
		Label:          "Alias B",
		APIKeyHash:     secure.HashToken(fixture.keyB),
		APIKeyPrefix:   fixture.keyB[:12],
		Enabled:        true,
		LastSyncStatus: domain.SyncStatusOK,
		LastSyncedAt:   &aliasSyncedAt,
	})
	if err != nil {
		t.Fatalf("create alias B: %v", err)
	}

	baseTime := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	e.upsertMessage(t, domain.LatestMessage{
		AliasID: fixture.aliasA.ID, UIDValidity: 101, UID: 11,
		MessageID: "a-old@example.test", InternalDate: baseTime.Add(-time.Minute),
		Subject: "A older subject", TextBody: "A older body", SyncedAt: baseTime.Add(-time.Minute),
	})
	e.upsertMessage(t, domain.LatestMessage{
		AliasID: fixture.aliasA.ID, UIDValidity: 101, UID: 12,
		MessageID: "a-new@example.test", InternalDate: baseTime,
		From:    []domain.MailAddress{{Name: "Sender A", Email: "sender-a@example.test"}},
		To:      []domain.MailAddress{{Email: fixture.aliasA.Address}},
		Subject: "A newest subject", TextBody: "A newest body", SyncedAt: baseTime,
	})
	e.upsertMessage(t, domain.LatestMessage{
		AliasID: fixture.aliasB.ID, UIDValidity: 202, UID: 99,
		MessageID: "b-private@example.test", InternalDate: baseTime,
		To:      []domain.MailAddress{{Email: fixture.aliasB.Address}},
		Subject: "B private subject", TextBody: "B private body", SyncedAt: baseTime,
	})
	return fixture
}

func (e *httpTestEnv) createSiblingMailboxFixture(t *testing.T) mailboxFixture {
	t.Helper()
	ctx := context.Background()
	fixture := mailboxFixture{
		keyA: testAPIKey(0x33),
		keyB: testAPIKey(0xff),
	}
	account := e.createAccount(t, "Shared Primary", "shared-primary@icloud.com", "encrypted-shared")
	fixture.accountA, fixture.accountB = account, account
	syncedAt := time.Now().UTC()
	var err error
	fixture.aliasA, err = e.store.CreateAlias(ctx, domain.Alias{
		AccountID:      account.ID,
		Address:        "shared-relay-a@icloud.com",
		Label:          "Shared alias A",
		APIKeyHash:     secure.HashToken(fixture.keyA),
		APIKeyPrefix:   fixture.keyA[:12],
		Enabled:        true,
		LastSyncStatus: domain.SyncStatusOK,
		LastSyncedAt:   &syncedAt,
	})
	if err != nil {
		t.Fatalf("create shared alias A: %v", err)
	}
	fixture.aliasB, err = e.store.CreateAlias(ctx, domain.Alias{
		AccountID:      account.ID,
		Address:        "shared-relay-b@icloud.com",
		Label:          "Shared alias B",
		APIKeyHash:     secure.HashToken(fixture.keyB),
		APIKeyPrefix:   fixture.keyB[:12],
		Enabled:        true,
		LastSyncStatus: domain.SyncStatusOK,
		LastSyncedAt:   &syncedAt,
	})
	if err != nil {
		t.Fatalf("create shared alias B: %v", err)
	}
	messageTime := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	e.upsertMessage(t, domain.LatestMessage{
		AliasID: fixture.aliasA.ID, UIDValidity: 303, UID: 10,
		InternalDate: messageTime, Subject: "A retained snapshot", SyncedAt: messageTime,
	})
	e.upsertMessage(t, domain.LatestMessage{
		AliasID: fixture.aliasB.ID, UIDValidity: 303, UID: 20,
		InternalDate: messageTime, Subject: "B healthy snapshot", SyncedAt: messageTime,
	})
	return fixture
}

func testAPIKey(fill byte) string {
	return "icm_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func (e *httpTestEnv) upsertMessage(t *testing.T, message domain.LatestMessage) {
	t.Helper()
	changed, err := e.store.UpsertLatestMessage(context.Background(), message)
	if err != nil {
		t.Fatalf("upsert latest message: %v", err)
	}
	if !changed {
		t.Fatal("latest message fixture was unexpectedly ignored")
	}
}

func requireResponseCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response did not set cookie %q", name)
	return nil
}

func assertNoResponseCookie(t *testing.T, response *httptest.ResponseRecorder, name string) {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name && cookie.Value != "" {
			t.Fatalf("response unexpectedly set cookie %q", name)
		}
	}
}

func decodeObjectField(t *testing.T, body []byte, field string) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode JSON object: %v; body=%s", err, body)
	}
	raw, ok := object[field]
	if !ok {
		t.Fatalf("JSON object is missing field %q: %s", field, body)
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		t.Fatalf("field %q is not an object: %v; value=%s", field, err, raw)
	}
	return nested
}

func decodeStringField(t *testing.T, object map[string]json.RawMessage, field string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(object[field], &value); err != nil {
		t.Fatalf("field %q is not a string: %v; value=%s", field, err, object[field])
	}
	return value
}

func decodeNestedObjectField(
	t *testing.T,
	object map[string]json.RawMessage,
	field string,
) map[string]json.RawMessage {
	t.Helper()
	raw, ok := object[field]
	if !ok {
		t.Fatalf("JSON object is missing field %q", field)
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		t.Fatalf("field %q is not an object: %v; value=%s", field, err, raw)
	}
	return nested
}

func assertAPIError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("API status = %d, want %d; body=%s", response.Code, status, response.Body.String())
	}
	errorObject := decodeObjectField(t, response.Body.Bytes(), "error")
	if got := decodeStringField(t, errorObject, "code"); got != code {
		t.Fatalf("API error code = %q, want %q", got, code)
	}
}

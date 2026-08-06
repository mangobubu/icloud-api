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
		if _, err := env.store.GetLatestMessage(context.Background(), alias.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("alias %q retained old snapshot after password update: %v", alias.Address, err)
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
			t.Fatal("API exposed a cleared snapshot after IMAP password update")
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
	router http.Handler
}

const testSessionCSRF = "test-session-csrf"

func newHTTPTestEnv(t *testing.T) *httpTestEnv {
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
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("create HTTP server: %v", err)
	}
	router, err := server.Router()
	if err != nil {
		t.Fatalf("create HTTP router: %v", err)
	}
	return &httpTestEnv{store: db, cipher: cipher, router: router}
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

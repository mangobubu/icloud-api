package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

func TestRecentMailDirectLinkReturnsCompactOwnedMessageInConfiguredTimezone(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Date(2026, time.August, 7, 5, 15, 0, 0, time.UTC)
	receivedAt := now.Add(-59 * time.Minute)
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	env.server.now = func() time.Time { return now }
	env.server.cfg.Timezone = location
	setAliasSyncedAt(t, env, mailboxes.aliasA, now)
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: receivedAt,
		TextBody:     "A compact body",
		HTMLBody:     "<p>A compact body</p>",
		SyncedAt:     now,
	})

	query := url.Values{
		"api_key": {mailboxes.keyA},
		"email":   {mailboxes.aliasB.Address},
		"alias":   {mailboxes.aliasB.Address},
	}
	response := directMailRequest(t, env, query)
	if response.Code != http.StatusOK {
		t.Fatalf("recent mail status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode compact response: %v", err)
	}
	if len(payload) != 3 {
		t.Fatalf("compact response fields = %v, want exactly email/content/time", payload)
	}
	if got := decodeStringField(t, payload, "email"); got != mailboxes.aliasA.Address {
		t.Fatalf("email = %q, want owned alias %q", got, mailboxes.aliasA.Address)
	}
	if got := decodeStringField(t, payload, "content"); got != "A compact body" {
		t.Fatalf("content = %q, want plain-text body", got)
	}
	if got := decodeStringField(t, payload, "time"); got != receivedAt.In(location).Format(time.RFC3339) {
		t.Fatalf("time = %q, want %q", got, receivedAt.In(location).Format(time.RFC3339))
	}
	for _, forbidden := range []string{mailboxes.aliasB.Address, "B private body", "A newest subject", "<p>A compact body</p>"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("compact response exposed forbidden value %q: %s", forbidden, response.Body.String())
		}
	}
}

func TestRecentMailDirectLinkExtractsPlainTextFromHTMLBody(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	env.server.now = func() time.Time { return now }
	setAliasSyncedAt(t, env, mailboxes.aliasA, now)
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: now.Add(-time.Minute),
		TextBody:     "  \n",
		HTMLBody: `<!doctype html><html><head>
			<title>Ignored email title</title>
			<style>.code { color: red }</style>
			</head><body>
			<div>您的临时 <strong>ChatGPT</strong> 登录代码</div>
			<p class="code">739638</p>
			<script>window.secret = "ignored"</script>
			<p>请勿与他人分享 &amp; 使用。</p>
			</body></html>`,
		SyncedAt: now,
	})

	response := directMailRequest(t, env, url.Values{"api_key": {mailboxes.keyA}})
	if response.Code != http.StatusOK {
		t.Fatalf("recent mail status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode compact response: %v", err)
	}
	wanted := "您的临时 ChatGPT 登录代码\n739638\n请勿与他人分享 & 使用。"
	content := decodeStringField(t, payload, "content")
	if content != wanted {
		t.Fatalf("content = %q, want extracted plain text %q", content, wanted)
	}
	for _, forbidden := range []string{"<html", "<style", "color: red", "window.secret", "Ignored email title"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("plain-text content contains HTML metadata %q: %q", forbidden, content)
		}
	}
}

func TestRecentMailDirectLinkEnforcesOneHourWindow(t *testing.T) {
	tests := []struct {
		name       string
		age        time.Duration
		wantStatus int
	}{
		{name: "inside window", age: 59*time.Minute + 59*time.Second, wantStatus: http.StatusOK},
		{name: "exact cutoff", age: time.Hour, wantStatus: http.StatusOK},
		{name: "outside window", age: time.Hour + time.Nanosecond, wantStatus: http.StatusNotFound},
		{name: "future timestamp", age: -time.Nanosecond, wantStatus: http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newHTTPTestEnv(t)
			mailboxes := env.createMailboxFixture(t)
			now := time.Date(2026, time.August, 7, 6, 0, 0, 123456789, time.UTC)
			env.server.now = func() time.Time { return now }
			setAliasSyncedAt(t, env, mailboxes.aliasA, now)
			env.upsertMessage(t, domain.LatestMessage{
				AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
				InternalDate: now.Add(-test.age), TextBody: "window body", SyncedAt: now,
			})

			direct := directMailRequest(t, env, url.Values{"api_key": {mailboxes.keyA}})
			if direct.Code != test.wantStatus {
				t.Fatalf("direct endpoint status = %d, want %d; body=%s", direct.Code, test.wantStatus, direct.Body.String())
			}
			if test.wantStatus == http.StatusNotFound {
				assertAPIError(t, direct, http.StatusNotFound, "MAIL_NOT_FOUND")
				if strings.Contains(direct.Body.String(), "window body") {
					t.Fatal("expired message body was exposed")
				}
			}
		})
	}
}

func TestLatestMailBearerKeepsExistingSnapshotOutsideRecentWindow(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Date(2026, time.August, 7, 6, 0, 0, 0, time.UTC)
	env.server.now = func() time.Time { return now }
	setAliasSyncedAt(t, env, mailboxes.aliasA, now)
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: now.Add(-24 * time.Hour), TextBody: "retained latest body", SyncedAt: now,
	})

	direct := directMailRequest(t, env, url.Values{"api_key": {mailboxes.keyA}})
	assertAPIError(t, direct, http.StatusNotFound, "MAIL_NOT_FOUND")
	bearer := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
	if bearer.Code != http.StatusOK || !strings.Contains(bearer.Body.String(), "retained latest body") {
		t.Fatalf("Bearer latest response = %d body=%s, want retained snapshot", bearer.Code, bearer.Body.String())
	}
}

func TestRecentMailDirectLinkTrailingSlashDoesNotRedirectQueryKey(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	env.server.now = func() time.Time { return now }
	setAliasSyncedAt(t, env, mailboxes.aliasA, now)
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: now.Add(-time.Minute), TextBody: "trailing slash body", SyncedAt: now,
	})

	target := "/api/v1/mail/recent/?api_key=" + url.QueryEscape(mailboxes.keyA)
	response := env.request(t, http.MethodGet, target, nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("trailing-slash recent status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Fatalf("trailing-slash recent unexpectedly redirected to %q", location)
	}
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("trailing-slash recent Cache-Control = %q, want no-store", cacheControl)
	}
}

func TestRecentMailDirectLinkRejectsInvalidQueryKeys(t *testing.T) {
	tests := []struct {
		name  string
		query url.Values
	}{
		{name: "missing", query: url.Values{}},
		{name: "empty", query: url.Values{"api_key": {""}}},
		{name: "malformed", query: url.Values{"api_key": {"not-an-api-key"}}},
		{name: "unknown", query: url.Values{"api_key": {testAPIKey(0x7f)}}},
		{name: "duplicate", query: url.Values{"api_key": {testAPIKey(0x11), testAPIKey(0xfb)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newHTTPTestEnv(t)
			env.createMailboxFixture(t)
			response := directMailRequest(t, env, test.query)
			assertAPIError(t, response, http.StatusUnauthorized, "INVALID_API_KEY")
		})
	}

	t.Run("malformed duplicate is not discarded", func(t *testing.T) {
		env := newHTTPTestEnv(t)
		mailboxes := env.createMailboxFixture(t)
		target := "/api/v1/mail/recent?api_key=" + url.QueryEscape(mailboxes.keyA) + "&api_key=%ZZ"
		response := env.request(t, http.MethodGet, target, nil, nil)
		assertAPIError(t, response, http.StatusUnauthorized, "INVALID_API_KEY")
	})
}

func TestMailEndpointsValidateSyncFreshnessWindow(t *testing.T) {
	staleAfter := 3 * time.Minute
	tests := []struct {
		name       string
		syncedAt   func(time.Time) time.Time
		wantStatus int
	}{
		{name: "inside freshness window", syncedAt: func(now time.Time) time.Time { return now.Add(-staleAfter + time.Nanosecond) }, wantStatus: http.StatusOK},
		{name: "exact freshness cutoff", syncedAt: func(now time.Time) time.Time { return now.Add(-staleAfter) }, wantStatus: http.StatusOK},
		{name: "stale sync", syncedAt: func(now time.Time) time.Time { return now.Add(-staleAfter - time.Nanosecond) }, wantStatus: http.StatusServiceUnavailable},
		{name: "future sync", syncedAt: func(now time.Time) time.Time { return now.Add(time.Nanosecond) }, wantStatus: http.StatusServiceUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newHTTPTestEnv(t)
			mailboxes := env.createMailboxFixture(t)
			now := time.Date(2026, time.August, 7, 6, 0, 0, 123456789, time.UTC)
			env.server.now = func() time.Time { return now }
			setAliasSyncedAt(t, env, mailboxes.aliasA, test.syncedAt(now))
			env.upsertMessage(t, domain.LatestMessage{
				AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
				InternalDate: now.Add(-time.Minute), TextBody: "fresh body", SyncedAt: now,
			})

			direct := directMailRequest(t, env, url.Values{"api_key": {mailboxes.keyA}})
			bearer := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
			for name, response := range map[string]*httptest.ResponseRecorder{"direct": direct, "Bearer": bearer} {
				if response.Code != test.wantStatus {
					t.Fatalf("%s endpoint status = %d, want %d; body=%s", name, response.Code, test.wantStatus, response.Body.String())
				}
				if test.wantStatus == http.StatusServiceUnavailable {
					assertAPIError(t, response, http.StatusServiceUnavailable, "SYNC_UNAVAILABLE")
				}
			}
		})
	}
}

func TestRecentMailDirectLinkRejectsDisabledAliasAndSyncFailure(t *testing.T) {
	t.Run("disabled alias", func(t *testing.T) {
		env := newHTTPTestEnv(t)
		mailboxes := env.createMailboxFixture(t)
		mailboxes.aliasA.Enabled = false
		if _, err := env.store.UpdateAlias(context.Background(), mailboxes.aliasA); err != nil {
			t.Fatalf("disable alias: %v", err)
		}
		response := directMailRequest(t, env, url.Values{"api_key": {mailboxes.keyA}})
		assertAPIError(t, response, http.StatusUnauthorized, "INVALID_API_KEY")
	})

	t.Run("sync failure", func(t *testing.T) {
		env := newHTTPTestEnv(t)
		mailboxes := env.createMailboxFixture(t)
		now := time.Now().UTC()
		if err := env.store.UpdateAliasSyncStatus(
			context.Background(), mailboxes.aliasA.ID, domain.SyncStatusError, "sync failed", &now,
		); err != nil {
			t.Fatalf("mark alias sync failed: %v", err)
		}
		response := directMailRequest(t, env, url.Values{"api_key": {mailboxes.keyA}})
		assertAPIError(t, response, http.StatusServiceUnavailable, "SYNC_UNAVAILABLE")
		if strings.Contains(response.Body.String(), "A newest body") {
			t.Fatal("sync failure exposed retained message")
		}
	})
}

func TestRecentMailDirectLinkAcceptsDerivedCredentialOnlyInQueryAndTracksRotation(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	env.server.now = func() time.Time { return now }
	setAliasSyncedAt(t, env, mailboxes.aliasA, now)
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: now.Add(-time.Minute), TextBody: "derived direct body", SyncedAt: now,
	})

	derived, err := env.server.cipher.DirectLinkToken(mailboxes.aliasA.ID, mailboxes.aliasA.APIKeyHash)
	if err != nil {
		t.Fatalf("derive direct-link credential: %v", err)
	}
	direct := directMailRequest(t, env, url.Values{"api_key": {derived}})
	if direct.Code != http.StatusOK || !strings.Contains(direct.Body.String(), "derived direct body") {
		t.Fatalf("derived direct-link response = %d; body=%s", direct.Code, direct.Body.String())
	}

	bearer := env.apiRequest(t, "/api/v1/mail/latest", derived)
	assertAPIError(t, bearer, http.StatusUnauthorized, "INVALID_API_KEY")

	_, rotatedHash, rotatedPrefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatalf("generate rotated API key: %v", err)
	}
	if _, err := env.store.RotateAliasAPIKey(context.Background(), mailboxes.aliasA.ID, rotatedHash, rotatedPrefix); err != nil {
		t.Fatalf("rotate API key fixture: %v", err)
	}
	oldAfterRotation := directMailRequest(t, env, url.Values{"api_key": {derived}})
	assertAPIError(t, oldAfterRotation, http.StatusUnauthorized, "INVALID_API_KEY")

	rotatedDerived, err := env.server.cipher.DirectLinkToken(mailboxes.aliasA.ID, rotatedHash)
	if err != nil {
		t.Fatalf("derive rotated direct-link credential: %v", err)
	}
	current := directMailRequest(t, env, url.Values{"api_key": {rotatedDerived}})
	if current.Code != http.StatusOK || !strings.Contains(current.Body.String(), "derived direct body") {
		t.Fatalf("rotated derived direct-link response = %d; body=%s", current.Code, current.Body.String())
	}

	if err := env.store.SetAliasEnabled(context.Background(), mailboxes.aliasA.ID, false); err != nil {
		t.Fatalf("disable alias fixture: %v", err)
	}
	disabled := directMailRequest(t, env, url.Values{"api_key": {rotatedDerived}})
	assertAPIError(t, disabled, http.StatusUnauthorized, "INVALID_API_KEY")
}

func TestRecentMailDirectLinkReportsDatabaseFailure(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	if err := env.store.Close(); err != nil {
		t.Fatalf("close test database: %v", err)
	}
	response := directMailRequest(t, env, url.Values{"api_key": {mailboxes.keyA}})
	assertAPIError(t, response, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE")
}

func TestRecoveryDoesNotLogQueryAPIKey(t *testing.T) {
	env := newHTTPTestEnv(t)
	var logs strings.Builder
	env.server.logger = slog.New(slog.NewTextHandler(&logs, nil))
	router := gin.New()
	router.Use(env.server.requestContext(), env.server.recovery())
	router.GET("/panic", func(c *gin.Context) {
		panic("test panic")
	})

	secret := testAPIKey(0x44)
	request := httptest.NewRequest(http.MethodGet, "/panic?api_key="+url.QueryEscape(secret), nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("panic response status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "api_key=") {
		t.Fatalf("recovery log exposed query credentials: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "path=/panic") {
		t.Fatalf("recovery log omitted sanitized path: %s", logs.String())
	}
}

func directMailRequest(t *testing.T, env *httpTestEnv, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/mail/recent"
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	return env.request(t, http.MethodGet, target, nil, nil)
}

func setAliasSyncedAt(t *testing.T, env *httpTestEnv, alias domain.Alias, syncedAt time.Time) {
	t.Helper()
	if err := env.store.UpdateAliasSyncStatus(
		context.Background(), alias.ID, domain.SyncStatusOK, "", &syncedAt,
	); err != nil {
		t.Fatalf("set alias sync time: %v", err)
	}
}

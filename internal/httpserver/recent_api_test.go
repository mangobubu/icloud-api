package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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
	sentAt := receivedAt.Add(-2 * time.Minute)
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
		HeaderDate:   &sentAt,
		Subject:      "Your ChatGPT verification code",
		TextBody:     "  A compact\r\n\tbody  ",
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

	data := decodeRecentMailData(t, response)
	if got := decodeStringField(t, data, "address"); got != mailboxes.aliasA.Address {
		t.Fatalf("address = %q, want owned alias %q", got, mailboxes.aliasA.Address)
	}
	if got := decodeStringField(t, data, "subject"); got != "Your ChatGPT verification code" {
		t.Fatalf("subject = %q, want message subject", got)
	}
	if got := decodeStringField(t, data, "snippet"); got != "A compact body" {
		t.Fatalf("snippet = %q, want plain-text body", got)
	}
	if got := decodeStringField(t, data, "sent_at"); got != sentAt.In(location).Format(time.RFC3339) {
		t.Fatalf("sent_at = %q, want %q", got, sentAt.In(location).Format(time.RFC3339))
	}
	for _, forbidden := range []string{mailboxes.aliasB.Address, "B private body", "A newest subject", "<p>A compact body</p>"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("compact response exposed forbidden value %q: %s", forbidden, response.Body.String())
		}
	}
}

func TestRecentMailDirectLinkConsumesEachSnapshotOnce(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Date(2026, time.August, 7, 5, 15, 0, 0, time.UTC)
	env.server.now = func() time.Time { return now }
	setAliasSyncedAt(t, env, mailboxes.aliasA, now)
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: now.Add(-2 * time.Minute), Subject: "first consumable message",
		TextBody: "first consumable body", SyncedAt: now,
	})

	var notifications atomic.Int32
	env.server.SetSeenNotifier(func() { notifications.Add(1) })
	query := url.Values{"api_key": {mailboxes.keyA}}

	first := directMailRequest(t, env, query)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "first consumable body") {
		t.Fatalf("first direct response = %d; body=%s", first.Code, first.Body.String())
	}
	second := directMailRequest(t, env, query)
	assertAPIError(t, second, http.StatusNotFound, "MAIL_NOT_FOUND")
	if got := notifications.Load(); got != 1 {
		t.Fatalf("notifications after repeated snapshot = %d, want 1", got)
	}

	bearer := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
	if bearer.Code != http.StatusOK || !strings.Contains(bearer.Body.String(), "first consumable body") {
		t.Fatalf("Bearer latest after direct consumption = %d; body=%s", bearer.Code, bearer.Body.String())
	}

	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 13,
		InternalDate: now.Add(-time.Minute), Subject: "next consumable message",
		TextBody: "next consumable body", SyncedAt: now,
	})
	third := directMailRequest(t, env, query)
	if third.Code != http.StatusOK || !strings.Contains(third.Body.String(), "next consumable body") {
		t.Fatalf("new UID direct response = %d; body=%s", third.Code, third.Body.String())
	}
	if got := notifications.Load(); got != 2 {
		t.Fatalf("notifications after two consumed UIDs = %d, want 2", got)
	}

	if err := env.store.ReplaceLatestMessage(context.Background(), domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: now.Add(-30 * time.Second), Subject: "returned consumed message",
		TextBody: "returned consumed body", SyncedAt: now,
	}); err != nil {
		t.Fatalf("replace latest message with consumed UID: %v", err)
	}
	returned := directMailRequest(t, env, query)
	assertAPIError(t, returned, http.StatusNotFound, "MAIL_NOT_FOUND")
	if got := notifications.Load(); got != 2 {
		t.Fatalf("notifications after returning to consumed UID = %d, want 2", got)
	}
	returnedBearer := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
	if returnedBearer.Code != http.StatusOK || !strings.Contains(returnedBearer.Body.String(), "returned consumed body") {
		t.Fatalf("Bearer latest after returning to consumed UID = %d; body=%s", returnedBearer.Code, returnedBearer.Body.String())
	}
	assertSeenPersistenceCounts(t, env, mailboxes.aliasA.ID, mailboxes.accountA.ID, 2, 2)
}

func TestRecentMailDirectLinkConcurrentRequestsConsumeOnce(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Date(2026, time.August, 7, 5, 15, 0, 0, time.UTC)
	env.server.now = func() time.Time { return now }
	setAliasSyncedAt(t, env, mailboxes.aliasA, now)
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: now.Add(-time.Minute), TextBody: "concurrent body", SyncedAt: now,
	})

	var notifications atomic.Int32
	env.server.SetSeenNotifier(func() { notifications.Add(1) })
	const requestCount = 12
	start := make(chan struct{})
	statuses := make(chan int, requestCount)
	var requests sync.WaitGroup
	for range requestCount {
		requests.Add(1)
		go func() {
			defer requests.Done()
			<-start
			request := httptest.NewRequest(
				http.MethodGet,
				"/api/v1/mail/recent?api_key="+url.QueryEscape(mailboxes.keyA),
				nil,
			)
			response := httptest.NewRecorder()
			env.router.ServeHTTP(response, request)
			statuses <- response.Code
		}()
	}
	close(start)
	requests.Wait()
	close(statuses)

	counts := make(map[int]int)
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusNotFound] != requestCount-1 || len(counts) != 2 {
		t.Fatalf("concurrent direct statuses = %v, want one 200 and %d 404 responses", counts, requestCount-1)
	}
	if got := notifications.Load(); got != 1 {
		t.Fatalf("concurrent notifications = %d, want 1", got)
	}
	assertSeenPersistenceCounts(t, env, mailboxes.aliasA.ID, mailboxes.accountA.ID, 1, 1)
}

func TestRecentMailDirectLinkExtractsPlainTextFromHTMLBody(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	env.server.now = func() time.Time { return now }
	env.server.cfg.Timezone = time.UTC
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
	data := decodeRecentMailData(t, response)
	wanted := "您的临时 ChatGPT 登录代码 739638 请勿与他人分享 & 使用。"
	snippet := decodeStringField(t, data, "snippet")
	if snippet != wanted {
		t.Fatalf("snippet = %q, want extracted plain text %q", snippet, wanted)
	}
	if got := decodeStringField(t, data, "sent_at"); got != now.Add(-time.Minute).Format(time.RFC3339) {
		t.Fatalf("sent_at fallback = %q, want IMAP receive time %q", got, now.Add(-time.Minute).Format(time.RFC3339))
	}
	if strings.ContainsAny(snippet, "\r\n") {
		t.Fatalf("snippet contains line breaks: %q", snippet)
	}
	for _, forbidden := range []string{"<html", "<style", "color: red", "window.secret", "Ignored email title"} {
		if strings.Contains(snippet, forbidden) {
			t.Fatalf("plain-text snippet contains HTML metadata %q: %q", forbidden, snippet)
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
			syncedAt := test.syncedAt(now)
			setAliasSyncedAt(t, env, mailboxes.aliasA, syncedAt)
			env.upsertMessage(t, domain.LatestMessage{
				AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
				InternalDate: now.Add(-time.Minute), TextBody: "fresh body", SyncedAt: syncedAt,
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

func TestMailEndpointWakesBackgroundSyncWithCooldown(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Date(2026, time.August, 7, 6, 0, 0, 0, time.UTC)
	env.server.now = func() time.Time { return now }
	setAliasSyncedAt(t, env, mailboxes.aliasA, now)
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: now.Add(-time.Minute), TextBody: "wake body", SyncedAt: now,
	})

	wakes := make(chan int64, 4)
	env.server.sync = func(accountID int64) error {
		wakes <- accountID
		return nil
	}
	first := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
	if first.Code != http.StatusOK {
		t.Fatalf("first latest response = %d; body=%s", first.Code, first.Body.String())
	}
	select {
	case accountID := <-wakes:
		if accountID != mailboxes.accountA.ID {
			t.Fatalf("wake account ID = %d, want %d", accountID, mailboxes.accountA.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("latest endpoint did not wake background sync")
	}

	second := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
	if second.Code != http.StatusOK {
		t.Fatalf("second latest response = %d; body=%s", second.Code, second.Body.String())
	}
	select {
	case accountID := <-wakes:
		t.Fatalf("cooldown did not coalesce wake for account %d", accountID)
	case <-time.After(50 * time.Millisecond):
	}

	env.server.now = func() time.Time { return now.Add(10 * time.Second) }
	third := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
	if third.Code != http.StatusOK {
		t.Fatalf("third latest response = %d; body=%s", third.Code, third.Body.String())
	}
	select {
	case accountID := <-wakes:
		if accountID != mailboxes.accountA.ID {
			t.Fatalf("second wake account ID = %d, want %d", accountID, mailboxes.accountA.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("latest endpoint did not wake after cooldown")
	}
}

func TestMailEndpointsKeepSnapshotFreshDuringConfiguredSyncBudget(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Date(2026, time.August, 7, 6, 0, 0, 0, time.UTC)
	env.server.now = func() time.Time { return now }
	env.server.cfg.PollInterval = 10 * time.Second
	env.server.cfg.SyncTimeout = 70 * time.Second
	setAliasSyncedAt(t, env, mailboxes.aliasA, now.Add(-90*time.Second))
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: now.Add(-time.Minute), TextBody: "budget body", SyncedAt: now.Add(-90 * time.Second),
	})

	fresh := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
	if fresh.Code != http.StatusOK {
		t.Fatalf("snapshot at configured freshness boundary = %d; body=%s", fresh.Code, fresh.Body.String())
	}

	staleAt := now.Add(-90*time.Second - time.Nanosecond)
	if err := env.store.UpdateAliasSyncStatus(
		context.Background(), mailboxes.aliasA.ID, domain.SyncStatusOK, "", &staleAt,
	); err != nil {
		t.Fatalf("move snapshot past freshness boundary: %v", err)
	}
	stale := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
	assertAPIError(t, stale, http.StatusServiceUnavailable, "SYNC_UNAVAILABLE")
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

	rotatedKey, rotatedHash, rotatedPrefix, err := secure.NewAPIKey()
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
	assertAPIError(t, current, http.StatusNotFound, "MAIL_NOT_FOUND")
	if strings.Contains(current.Body.String(), "derived direct body") {
		t.Fatalf("rotated direct-link credential exposed consumed snapshot: %s", current.Body.String())
	}
	latestAfterRotation := env.apiRequest(t, "/api/v1/mail/latest", rotatedKey)
	if latestAfterRotation.Code != http.StatusOK || !strings.Contains(latestAfterRotation.Body.String(), "derived direct body") {
		t.Fatalf("Bearer latest after credential rotation = %d; body=%s", latestAfterRotation.Code, latestAfterRotation.Body.String())
	}

	if err := env.store.SetAliasEnabled(context.Background(), mailboxes.aliasA.ID, false); err != nil {
		t.Fatalf("disable alias fixture: %v", err)
	}
	disabled := directMailRequest(t, env, url.Values{"api_key": {rotatedDerived}})
	assertAPIError(t, disabled, http.StatusUnauthorized, "INVALID_API_KEY")
}

func TestRecentMailDirectLinkRejectsBindingChangedAfterAuthentication(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *httpTestEnv, mailboxFixture, time.Time)
	}{
		{
			name: "API key rotated",
			mutate: func(t *testing.T, env *httpTestEnv, mailboxes mailboxFixture, _ time.Time) {
				rotated := testAPIKey(0x72)
				if _, err := env.store.RotateAliasAPIKey(
					context.Background(), mailboxes.aliasA.ID, secure.HashToken(rotated), rotated[:12],
				); err != nil {
					t.Fatalf("rotate API key after authentication: %v", err)
				}
			},
		},
		{
			name: "sync marked unavailable",
			mutate: func(t *testing.T, env *httpTestEnv, mailboxes mailboxFixture, now time.Time) {
				failedAt := now.Add(time.Second)
				if err := env.store.UpdateAliasSyncStatus(
					context.Background(), mailboxes.aliasA.ID, domain.SyncStatusError, "sync failed", &failedAt,
				); err != nil {
					t.Fatalf("mark sync unavailable after authentication: %v", err)
				}
			},
		},
		{
			name: "same UID republished",
			mutate: func(t *testing.T, env *httpTestEnv, mailboxes mailboxFixture, now time.Time) {
				refreshedAt := now.Add(time.Second)
				if err := env.store.ReplaceLatestMessage(context.Background(), domain.LatestMessage{
					AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
					InternalDate: now.Add(-time.Minute), TextBody: "republished body", SyncedAt: refreshedAt,
				}); err != nil {
					t.Fatalf("republish same UID after authentication: %v", err)
				}
				setAliasSyncedAt(t, env, mailboxes.aliasA, refreshedAt)
			},
		},
		{
			name: "alias deleted",
			mutate: func(t *testing.T, env *httpTestEnv, mailboxes mailboxFixture, _ time.Time) {
				if err := env.store.DeleteAlias(context.Background(), mailboxes.aliasA.ID); err != nil {
					t.Fatalf("delete alias after authentication: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newHTTPTestEnv(t)
			mailboxes := env.createMailboxFixture(t)
			now := time.Date(2026, time.August, 7, 5, 15, 0, 0, time.UTC)
			env.server.now = func() time.Time { return now }
			setAliasSyncedAt(t, env, mailboxes.aliasA, now)
			env.upsertMessage(t, domain.LatestMessage{
				AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
				InternalDate: now.Add(-time.Minute), TextBody: "authenticated body", SyncedAt: now,
			})

			var notifications atomic.Int32
			env.server.SetSeenNotifier(func() { notifications.Add(1) })
			const route = "/test/recent-binding-cas"
			router := gin.New()
			router.Use(env.server.requestContext(), env.server.securityHeaders(), env.server.recovery())
			router.GET(route, env.server.apiKeyQueryAuth(), func(c *gin.Context) {
				test.mutate(t, env, mailboxes, now)
				env.server.recentMail(c)
			})
			request := httptest.NewRequest(
				http.MethodGet, route+"?api_key="+url.QueryEscape(mailboxes.keyA), nil,
			)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assertAPIError(t, response, http.StatusNotFound, "MAIL_NOT_FOUND")
			if strings.Contains(response.Body.String(), "authenticated body") ||
				strings.Contains(response.Body.String(), "republished body") {
				t.Fatalf("stale binding response exposed mail body: %s", response.Body.String())
			}
			if got := notifications.Load(); got != 0 {
				t.Fatalf("notifications after rejected CAS = %d, want 0", got)
			}
			assertSeenPersistenceCounts(t, env, mailboxes.aliasA.ID, mailboxes.accountA.ID, 0, 0)
		})
	}
}

func TestLatestMailRemainsRepeatableWithoutConsumptionSideEffects(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Date(2026, time.August, 7, 5, 15, 0, 0, time.UTC)
	env.server.now = func() time.Time { return now }
	setAliasSyncedAt(t, env, mailboxes.aliasA, now)
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: now.Add(-time.Minute), TextBody: "repeatable latest body", SyncedAt: now,
	})

	for attempt := 1; attempt <= 2; attempt++ {
		response := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "repeatable latest body") {
			t.Fatalf("latest attempt %d = %d; body=%s", attempt, response.Code, response.Body.String())
		}
	}
	assertSeenPersistenceCounts(t, env, mailboxes.aliasA.ID, mailboxes.accountA.ID, 0, 0)
}

func TestRecentMailConsumesRetainedSnapshotAfterNoNewMailSync(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Date(2026, time.August, 7, 5, 15, 0, 0, time.UTC)
	messageSyncedAt := now.Add(-time.Minute)
	env.server.now = func() time.Time { return now }
	setAliasSyncedAt(t, env, mailboxes.aliasA, now)
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: now.Add(-time.Minute), TextBody: "retained snapshot body", SyncedAt: messageSyncedAt,
	})

	response := directMailRequest(t, env, url.Values{"api_key": {mailboxes.keyA}})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "retained snapshot body") {
		t.Fatalf("retained snapshot response = %d; body=%s", response.Code, response.Body.String())
	}
	assertSeenPersistenceCounts(t, env, mailboxes.aliasA.ID, mailboxes.accountA.ID, 1, 1)
}

func TestRecentMailDirectLinkReportsDatabaseFailure(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	env.server.now = func() time.Time { return now }
	setAliasSyncedAt(t, env, mailboxes.aliasA, now)
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
		InternalDate: now.Add(-time.Minute), TextBody: "database failure body", SyncedAt: now,
	})
	var notifications atomic.Int32
	env.server.SetSeenNotifier(func() { notifications.Add(1) })
	if _, err := env.store.DB().ExecContext(context.Background(), `DROP TABLE consumed_messages`); err != nil {
		t.Fatalf("remove consumption table fixture: %v", err)
	}
	response := directMailRequest(t, env, url.Values{"api_key": {mailboxes.keyA}})
	assertAPIError(t, response, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE")
	if got := notifications.Load(); got != 0 {
		t.Fatalf("notifications after failed consumption = %d, want 0", got)
	}
	var queued int
	if err := env.store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM imap_seen_tasks WHERE account_id = ?`, mailboxes.accountA.ID,
	).Scan(&queued); err != nil {
		t.Fatalf("count queued seen tasks after failed consumption: %v", err)
	}
	if queued != 0 {
		t.Fatalf("queued seen tasks after failed consumption = %d, want 0", queued)
	}
	bearer := env.apiRequest(t, "/api/v1/mail/latest", mailboxes.keyA)
	if bearer.Code != http.StatusOK || !strings.Contains(bearer.Body.String(), "database failure body") {
		t.Fatalf("Bearer latest after failed consumption = %d; body=%s", bearer.Code, bearer.Body.String())
	}
}

func TestRecentMailDirectLinkDoesNotConsumeOrNotifyRejectedRequests(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*testing.T, *httpTestEnv, mailboxFixture, time.Time)
		key        func(mailboxFixture) string
		wantStatus int
		wantCode   string
	}{
		{
			name: "expired message",
			prepare: func(t *testing.T, env *httpTestEnv, mailboxes mailboxFixture, now time.Time) {
				setAliasSyncedAt(t, env, mailboxes.aliasA, now)
				env.upsertMessage(t, domain.LatestMessage{
					AliasID: mailboxes.aliasA.ID, UIDValidity: 101, UID: 12,
					InternalDate: now.Add(-time.Hour - time.Nanosecond), TextBody: "expired body", SyncedAt: now,
				})
			},
			key:        func(mailboxes mailboxFixture) string { return mailboxes.keyA },
			wantStatus: http.StatusNotFound,
			wantCode:   "MAIL_NOT_FOUND",
		},
		{
			name: "sync unavailable",
			prepare: func(t *testing.T, env *httpTestEnv, mailboxes mailboxFixture, now time.Time) {
				if err := env.store.UpdateAliasSyncStatus(
					context.Background(), mailboxes.aliasA.ID, domain.SyncStatusError, "sync failed", &now,
				); err != nil {
					t.Fatalf("mark alias sync failed: %v", err)
				}
			},
			key:        func(mailboxes mailboxFixture) string { return mailboxes.keyA },
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "SYNC_UNAVAILABLE",
		},
		{
			name:       "invalid credential",
			prepare:    func(*testing.T, *httpTestEnv, mailboxFixture, time.Time) {},
			key:        func(mailboxFixture) string { return testAPIKey(0x7f) },
			wantStatus: http.StatusUnauthorized,
			wantCode:   "INVALID_API_KEY",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newHTTPTestEnv(t)
			mailboxes := env.createMailboxFixture(t)
			now := time.Date(2026, time.August, 7, 5, 15, 0, 0, time.UTC)
			env.server.now = func() time.Time { return now }
			test.prepare(t, env, mailboxes, now)
			var notifications atomic.Int32
			env.server.SetSeenNotifier(func() { notifications.Add(1) })

			response := directMailRequest(t, env, url.Values{"api_key": {test.key(mailboxes)}})
			assertAPIError(t, response, test.wantStatus, test.wantCode)
			if got := notifications.Load(); got != 0 {
				t.Fatalf("notifications = %d, want 0", got)
			}
			assertSeenPersistenceCounts(t, env, mailboxes.aliasA.ID, mailboxes.accountA.ID, 0, 0)
		})
	}
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

func decodeRecentMailData(t *testing.T, response *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode recent mail response: %v", err)
	}
	if len(envelope) != 1 {
		t.Fatalf("recent mail response fields = %v, want exactly data", envelope)
	}
	rawData, ok := envelope["data"]
	if !ok {
		t.Fatalf("recent mail response omitted data: %v", envelope)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(rawData, &data); err != nil {
		t.Fatalf("decode recent mail data: %v", err)
	}
	if len(data) != 4 {
		t.Fatalf("recent mail data fields = %v, want exactly address/subject/snippet/sent_at", data)
	}
	return data
}

func setAliasSyncedAt(t *testing.T, env *httpTestEnv, alias domain.Alias, syncedAt time.Time) {
	t.Helper()
	if err := env.store.UpdateAliasSyncStatus(
		context.Background(), alias.ID, domain.SyncStatusOK, "", &syncedAt,
	); err != nil {
		t.Fatalf("set alias sync time: %v", err)
	}
}

func assertSeenPersistenceCounts(
	t *testing.T,
	env *httpTestEnv,
	aliasID, accountID int64,
	wantConsumed, wantQueued int,
) {
	t.Helper()
	var consumed int
	if err := env.store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM consumed_messages WHERE alias_id = ?`, aliasID,
	).Scan(&consumed); err != nil {
		t.Fatalf("count consumed messages: %v", err)
	}
	if consumed != wantConsumed {
		t.Fatalf("consumed message rows = %d, want %d", consumed, wantConsumed)
	}
	var queued int
	if err := env.store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM imap_seen_tasks WHERE account_id = ?`, accountID,
	).Scan(&queued); err != nil {
		t.Fatalf("count queued seen tasks: %v", err)
	}
	if queued != wantQueued {
		t.Fatalf("queued seen task rows = %d, want %d", queued, wantQueued)
	}
}

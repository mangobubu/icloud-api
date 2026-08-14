package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

func TestLegacyMailRecentTrailingSlashServesWithoutRedirect(t *testing.T) {
	now := time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC)
	fixture := newLegacyMailBoundaryFixture(t, now, now, now.Add(-time.Minute))

	response := serveV2Request(
		fixture.router,
		http.MethodGet,
		"/api/v1/mail/recent/?api_key="+url.QueryEscape(fixture.directToken),
		"",
		nil,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("trailing-slash recent status = %d, want %d; body=%s",
			response.Code, http.StatusOK, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Fatalf("trailing-slash recent unexpectedly redirected to %q", location)
	}
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("trailing-slash recent Cache-Control = %q, want no-store", cacheControl)
	}
}

func TestLegacyMailRecentRejectsInvalidQueryCredentials(t *testing.T) {
	now := time.Date(2026, 8, 13, 4, 10, 0, 0, time.UTC)
	unknownKey, _, _, err := secure.NewAPIKey()
	if err != nil {
		t.Fatalf("generate unknown API key: %v", err)
	}

	tests := []struct {
		name   string
		target func(legacyMailBoundaryFixture) string
	}{
		{name: "missing", target: func(legacyMailBoundaryFixture) string {
			return "/api/v1/mail/recent"
		}},
		{name: "empty", target: func(legacyMailBoundaryFixture) string {
			return "/api/v1/mail/recent?api_key="
		}},
		{name: "malformed", target: func(legacyMailBoundaryFixture) string {
			return "/api/v1/mail/recent?api_key=not-an-api-key"
		}},
		{name: "unknown", target: func(legacyMailBoundaryFixture) string {
			return "/api/v1/mail/recent?api_key=" + url.QueryEscape(unknownKey)
		}},
		{name: "duplicate", target: func(fixture legacyMailBoundaryFixture) string {
			key := url.QueryEscape(fixture.directToken)
			return "/api/v1/mail/recent?api_key=" + key + "&api_key=" + key
		}},
		{name: "malformed duplicate", target: func(fixture legacyMailBoundaryFixture) string {
			return "/api/v1/mail/recent?api_key=" + url.QueryEscape(fixture.directToken) + "&api_key=%ZZ"
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLegacyMailBoundaryFixture(t, now, now, now.Add(-time.Minute))
			response := serveV2Request(fixture.router, http.MethodGet, test.target(fixture), "", nil)
			assertLegacyMailBoundaryAPIError(t, response, http.StatusUnauthorized, "INVALID_API_KEY")
		})
	}
}

func TestLegacyMailRecentEnforcesMessageFreshnessBoundary(t *testing.T) {
	tests := []struct {
		name       string
		messageAt  func(time.Time) time.Time
		wantStatus int
	}{
		{
			name:       "exact one hour cutoff",
			messageAt:  func(now time.Time) time.Time { return now.Add(-time.Hour) },
			wantStatus: http.StatusOK,
		},
		{
			name:       "older than one hour",
			messageAt:  func(now time.Time) time.Time { return now.Add(-time.Hour - time.Nanosecond) },
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "future message",
			messageAt:  func(now time.Time) time.Time { return now.Add(time.Nanosecond) },
			wantStatus: http.StatusNotFound,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 13, 4, 20, 0, 123456789, time.UTC)
			fixture := newLegacyMailBoundaryFixture(t, now, now, test.messageAt(now))
			response := fixture.recent(t)
			if test.wantStatus == http.StatusOK {
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), legacyMailBoundaryBody) {
					t.Fatalf("recent freshness status = %d, want %d; body=%s",
						response.Code, http.StatusOK, response.Body.String())
				}
				return
			}
			assertLegacyMailBoundaryAPIError(t, response, test.wantStatus, "MAIL_NOT_FOUND")
			if strings.Contains(response.Body.String(), legacyMailBoundaryBody) {
				t.Fatalf("rejected freshness response exposed message body: %s", response.Body.String())
			}
		})
	}
}

func TestLegacyMailEndpointsEnforceSyncFreshnessBoundary(t *testing.T) {
	const (
		pollInterval = 10 * time.Second
		syncTimeout  = 70 * time.Second
	)
	staleAfter := syncTimeout + 2*pollInterval
	tests := []struct {
		name       string
		syncedAt   func(time.Time) time.Time
		wantStatus int
	}{
		{
			name:       "exact configured cutoff",
			syncedAt:   func(now time.Time) time.Time { return now.Add(-staleAfter) },
			wantStatus: http.StatusOK,
		},
		{
			name:       "past configured cutoff",
			syncedAt:   func(now time.Time) time.Time { return now.Add(-staleAfter - time.Nanosecond) },
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "future sync",
			syncedAt:   func(now time.Time) time.Time { return now.Add(time.Nanosecond) },
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 13, 4, 30, 0, 123456789, time.UTC)
			fixture := newLegacyMailBoundaryFixture(t, now, test.syncedAt(now), now.Add(-time.Minute))
			fixture.env.server.cfg.PollInterval = pollInterval
			fixture.env.server.cfg.SyncTimeout = syncTimeout

			responses := map[string]*httptest.ResponseRecorder{
				"latest": fixture.latest(t, fixture.rawKey),
				"recent": fixture.recent(t),
			}
			for endpoint, response := range responses {
				if test.wantStatus == http.StatusOK {
					if response.Code != http.StatusOK {
						t.Fatalf("%s sync freshness status = %d, want %d; body=%s",
							endpoint, response.Code, http.StatusOK, response.Body.String())
					}
					continue
				}
				assertLegacyMailBoundaryAPIError(t, response, test.wantStatus, "SYNC_UNAVAILABLE")
			}
		})
	}
}

func TestLegacyMailAPIKeyRotationInvalidatesOldKeyAndDirectLink(t *testing.T) {
	now := time.Date(2026, 8, 13, 4, 40, 0, 0, time.UTC)
	fixture := newLegacyMailBoundaryFixture(t, now, now, now.Add(-time.Minute))

	before := fixture.latest(t, fixture.rawKey)
	if before.Code != http.StatusOK {
		t.Fatalf("old API key before rotation status = %d; body=%s", before.Code, before.Body.String())
	}

	newKey, newHash, newPrefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatalf("generate rotated API key: %v", err)
	}
	rotated, err := fixture.env.store.RotateAliasAPIKeyWithRawKey(
		context.Background(), fixture.alias.ID, newHash, newPrefix, newKey,
	)
	if err != nil {
		t.Fatalf("rotate legacy API key: %v", err)
	}
	newDirectToken, err := fixture.env.cipher.DirectLinkToken(rotated.ID, rotated.APIKeyHash)
	if err != nil {
		t.Fatalf("derive rotated direct-link token: %v", err)
	}

	oldBearer := fixture.latest(t, fixture.rawKey)
	assertLegacyMailBoundaryAPIError(t, oldBearer, http.StatusUnauthorized, "INVALID_API_KEY")
	oldDirect := serveV2Request(
		fixture.router,
		http.MethodGet,
		"/api/v1/mail/recent?api_key="+url.QueryEscape(fixture.directToken),
		"",
		nil,
	)
	assertLegacyMailBoundaryAPIError(t, oldDirect, http.StatusUnauthorized, "INVALID_API_KEY")

	newBearer := fixture.latest(t, newKey)
	if newBearer.Code != http.StatusOK || !strings.Contains(newBearer.Body.String(), legacyMailBoundaryBody) {
		t.Fatalf("rotated API key status = %d, want %d; body=%s",
			newBearer.Code, http.StatusOK, newBearer.Body.String())
	}
	newDirect := serveV2Request(
		fixture.router,
		http.MethodGet,
		"/api/v1/mail/recent?api_key="+url.QueryEscape(newDirectToken),
		"",
		nil,
	)
	if newDirect.Code != http.StatusOK || !strings.Contains(newDirect.Body.String(), legacyMailBoundaryBody) {
		t.Fatalf("rotated direct link status = %d, want %d; body=%s",
			newDirect.Code, http.StatusOK, newDirect.Body.String())
	}
}

func TestLegacyMailRecentConcurrentRequestsConsumeExactlyOnce(t *testing.T) {
	now := time.Date(2026, 8, 13, 4, 50, 0, 0, time.UTC)
	fixture := newLegacyMailBoundaryFixture(t, now, now, now.Add(-time.Minute))

	var notifications atomic.Int32
	fixture.env.server.SetSeenNotifier(func() { notifications.Add(1) })
	const requestCount = 16
	type requestResult struct {
		status int
		body   string
	}
	start := make(chan struct{})
	results := make(chan requestResult, requestCount)
	var requests sync.WaitGroup
	for attempt := 0; attempt < requestCount; attempt++ {
		requests.Add(1)
		go func() {
			defer requests.Done()
			<-start
			response := fixture.recent(t)
			results <- requestResult{status: response.Code, body: response.Body.String()}
		}()
	}
	close(start)
	requests.Wait()
	close(results)

	statuses := make(map[int]int)
	var unexpectedBodies []string
	for result := range results {
		statuses[result.status]++
		if result.status != http.StatusOK && result.status != http.StatusNotFound {
			unexpectedBodies = append(unexpectedBodies, result.body)
		}
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusNotFound] != requestCount-1 || len(statuses) != 2 {
		t.Fatalf("concurrent recent statuses = %v, want one 200 and %d 404 responses; unexpected bodies=%q",
			statuses, requestCount-1, unexpectedBodies)
	}
	if got := notifications.Load(); got != 1 {
		t.Fatalf("concurrent seen notifications = %d, want 1", got)
	}
	fixture.assertConsumptionState(t, 1, 1)
}

const legacyMailBoundaryBody = "legacy mail boundary body"

type legacyMailBoundaryFixture struct {
	env         *adminAPITestEnv
	router      http.Handler
	account     domain.Account
	alias       domain.Alias
	rawKey      string
	directToken string
}

func newLegacyMailBoundaryFixture(
	t *testing.T,
	now, lastSyncedAt, internalDate time.Time,
) legacyMailBoundaryFixture {
	t.Helper()
	env := newAdminAPITestEnv(t)
	env.server.sync = nil
	env.server.now = func() time.Time { return now }
	env.server.cfg.Timezone = time.UTC
	env.store.ConfigureAliasCredentialFactory(nil)

	passwordCiphertext, err := env.cipher.Encrypt("legacy-mail-boundary-password")
	if err != nil {
		t.Fatalf("encrypt legacy mail account password: %v", err)
	}
	account, err := env.store.CreateAccount(context.Background(), domain.Account{
		Name:               "Legacy mail boundary",
		Email:              "legacy-mail-boundary@icloud.com",
		IMAPHost:           "imap.mail.me.com",
		IMAPPort:           993,
		IMAPUsername:       "legacy-mail-boundary@icloud.com",
		PasswordCiphertext: passwordCiphertext,
		Enabled:            true,
	})
	if err != nil {
		t.Fatalf("create legacy mail account: %v", err)
	}
	rawKey, apiKeyHash, apiKeyPrefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatalf("generate legacy mail API key: %v", err)
	}
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID:      account.ID,
		Address:        "legacy-mail-boundary-alias@icloud.com",
		Label:          "Legacy mail boundary",
		APIKeyHash:     apiKeyHash,
		APIKeyPrefix:   apiKeyPrefix,
		CredentialMode: domain.AliasCredentialModeLegacy,
		Enabled:        true,
	})
	if err != nil {
		t.Fatalf("create legacy mail alias: %v", err)
	}
	if err := env.store.UpdateAliasSyncStatus(
		context.Background(), alias.ID, domain.SyncStatusOK, "", &lastSyncedAt,
	); err != nil {
		t.Fatalf("mark legacy alias synced: %v", err)
	}
	if _, err := env.store.UpsertLatestMessage(context.Background(), domain.LatestMessage{
		AliasID:      alias.ID,
		UIDValidity:  713,
		UID:          41,
		MessageID:    "<legacy-mail-boundary@example.test>",
		InternalDate: internalDate,
		Subject:      "Legacy mail boundary subject",
		TextBody:     legacyMailBoundaryBody,
		SyncedAt:     lastSyncedAt,
	}); err != nil {
		t.Fatalf("publish legacy latest message: %v", err)
	}
	directToken, err := env.cipher.DirectLinkToken(alias.ID, alias.APIKeyHash)
	if err != nil {
		t.Fatalf("derive legacy direct-link token: %v", err)
	}
	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build legacy mail router: %v", err)
	}
	return legacyMailBoundaryFixture{
		env: env, router: router, account: account, alias: alias,
		rawKey: rawKey, directToken: directToken,
	}
}

func (fixture legacyMailBoundaryFixture) latest(t *testing.T, key string) *httptest.ResponseRecorder {
	t.Helper()
	return serveV2Request(fixture.router, http.MethodGet, "/api/v1/mail/latest", "", map[string]string{
		"Authorization": "Bearer " + key,
	})
}

func (fixture legacyMailBoundaryFixture) recent(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return serveV2Request(
		fixture.router,
		http.MethodGet,
		"/api/v1/mail/recent?api_key="+url.QueryEscape(fixture.directToken),
		"",
		nil,
	)
}

func (fixture legacyMailBoundaryFixture) assertConsumptionState(
	t *testing.T,
	wantConsumed, wantSeen int,
) {
	t.Helper()
	var consumed int
	if err := fixture.env.store.DB().QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM consumed_messages
		WHERE alias_id = ? AND uid_validity = 713 AND uid = 41`, fixture.alias.ID,
	).Scan(&consumed); err != nil {
		t.Fatalf("count consumed legacy messages: %v", err)
	}
	if consumed != wantConsumed {
		t.Fatalf("consumed legacy messages = %d, want %d", consumed, wantConsumed)
	}
	var seen int
	if err := fixture.env.store.DB().QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM imap_seen_tasks
		WHERE account_id = ? AND uid_validity = 713 AND uid = 41`, fixture.account.ID,
	).Scan(&seen); err != nil {
		t.Fatalf("count legacy seen tasks: %v", err)
	}
	if seen != wantSeen {
		t.Fatalf("legacy seen tasks = %d, want %d", seen, wantSeen)
	}
}

func assertLegacyMailBoundaryAPIError(
	t *testing.T,
	response *httptest.ResponseRecorder,
	wantStatus int,
	wantCode string,
) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("legacy mail error status = %d, want %d; body=%s",
			response.Code, wantStatus, response.Body.String())
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode legacy mail error: %v; body=%s", err, response.Body.String())
	}
	if payload.Error.Code != wantCode {
		t.Fatalf("legacy mail error code = %q, want %q; body=%s",
			payload.Error.Code, wantCode, response.Body.String())
	}
}

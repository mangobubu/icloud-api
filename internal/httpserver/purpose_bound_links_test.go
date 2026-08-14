package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

func TestV2URLTokensArePurposeBoundAtRouter(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.sync = nil
	now := time.Date(2026, 8, 14, 3, 0, 0, 0, time.UTC)
	env.server.now = func() time.Time { return now }
	env.server.cfg.Timezone = time.UTC
	account := adminAPITestCreateAccount(t, env, "purpose-bound@icloud.com")
	alias, credentials := createV2AliasFixture(t, env, account.ID, "purpose-bound-alias@icloud.com")

	if _, err := env.store.UpsertLatestMessage(context.Background(), domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 814, UID: 9,
		MessageID: "<purpose-bound@example.test>", InternalDate: now.Add(-time.Minute),
		Subject: "purpose-bound subject", TextBody: "purpose-bound body", SyncedAt: now,
	}); err != nil {
		t.Fatalf("publish purpose-bound latest message: %v", err)
	}
	if err := env.store.UpdateAliasSyncStatus(
		context.Background(), alias.ID, domain.SyncStatusOK, "", &now,
	); err != nil {
		t.Fatalf("mark purpose-bound alias synced: %v", err)
	}

	dto, err := env.server.adminAPIAliasFromDomain(alias)
	if err != nil {
		t.Fatalf("build v2 alias DTO: %v", err)
	}
	otpToken := purposeBoundTokenFromPath(t, dto.OTPURLPath, "token")
	recentToken := purposeBoundTokenFromPath(t, dto.DirectLinkPath, "api_key")
	if otpToken == recentToken {
		t.Fatal("admin DTO reused one token for OTP and recent mail")
	}
	if !env.cipher.VerifyOTPToken(otpToken, alias.ID, alias.APIKeyHash) ||
		!env.cipher.VerifyRecentMailToken(recentToken, alias.ID, alias.APIKeyHash) {
		t.Fatal("admin DTO returned a token that does not verify for its declared purpose")
	}
	legacyV1Token, err := env.cipher.DirectLinkToken(alias.ID, alias.APIKeyHash)
	if err != nil {
		t.Fatalf("derive predecessor v1 token: %v", err)
	}

	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build purpose-bound router: %v", err)
	}
	for name, target := range map[string]string{
		"recent token on OTP":          "/api/v1/otp?token=" + url.QueryEscape(recentToken),
		"OTP token on recent":          "/api/v1/mail/recent?api_key=" + url.QueryEscape(otpToken),
		"legacy v1 token on v2 recent": "/api/v1/mail/recent?api_key=" + url.QueryEscape(legacyV1Token),
	} {
		t.Run(name, func(t *testing.T) {
			response := serveV2Request(router, http.MethodGet, target, "", nil)
			if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"INVALID_API_KEY"`) {
				t.Fatalf("cross-purpose status = %d; body=%s", response.Code, response.Body.String())
			}
		})
	}

	otp := serveV2Request(router, http.MethodGet, dto.OTPURLPath, "", nil)
	if otp.Code != http.StatusOK || strings.TrimSpace(otp.Body.String()) != "[]" {
		t.Fatalf("purpose-bound OTP link status = %d; body=%s", otp.Code, otp.Body.String())
	}
	otpByAPIKey := serveV2Request(router, http.MethodGet, "/api/v1/otp", "", map[string]string{
		"Authorization": "Bearer " + credentials.APIKey,
	})
	if otpByAPIKey.Code != http.StatusOK || strings.TrimSpace(otpByAPIKey.Body.String()) != "[]" {
		t.Fatalf("existing API Key OTP status = %d; body=%s", otpByAPIKey.Code, otpByAPIKey.Body.String())
	}
	recent := serveV2Request(router, http.MethodGet, dto.DirectLinkPath, "", nil)
	if recent.Code != http.StatusOK || !strings.Contains(recent.Body.String(), "purpose-bound body") {
		t.Fatalf("purpose-bound recent link status = %d; body=%s", recent.Code, recent.Body.String())
	}
	latestByAPIKey := serveV2Request(router, http.MethodGet, "/api/v1/mail/latest", "", map[string]string{
		"Authorization": "Bearer " + credentials.APIKey,
	})
	if latestByAPIKey.Code != http.StatusOK || !strings.Contains(latestByAPIKey.Body.String(), "purpose-bound body") {
		t.Fatalf("existing API Key latest-mail status = %d; body=%s", latestByAPIKey.Code, latestByAPIKey.Body.String())
	}
	if _, err := env.store.UpsertLatestMessage(context.Background(), domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 814, UID: 10,
		MessageID: "<purpose-bound-api-key@example.test>", InternalDate: now.Add(-30 * time.Second),
		Subject: "API Key compatibility", TextBody: "API Key recent body", SyncedAt: now,
	}); err != nil {
		t.Fatalf("publish API Key compatibility message: %v", err)
	}
	recentByAPIKey := serveV2Request(
		router,
		http.MethodGet,
		"/api/v1/mail/recent?api_key="+url.QueryEscape(credentials.APIKey),
		"",
		nil,
	)
	if recentByAPIKey.Code != http.StatusOK || !strings.Contains(recentByAPIKey.Body.String(), "API Key recent body") {
		t.Fatalf("existing API Key recent-mail status = %d; body=%s", recentByAPIKey.Code, recentByAPIKey.Body.String())
	}
}

func TestLegacyV1DirectTokenStillAuthorizesLegacyRecentMail(t *testing.T) {
	now := time.Date(2026, 8, 14, 3, 30, 0, 0, time.UTC)
	fixture := newLegacyMailBoundaryFixture(t, now, now, now.Add(-time.Minute))
	if !fixture.env.cipher.VerifyDirectLinkToken(
		fixture.directToken, fixture.alias.ID, fixture.alias.APIKeyHash,
	) {
		t.Fatal("saved legacy v1 direct token no longer verifies")
	}
	response := fixture.recent(t)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), legacyMailBoundaryBody) {
		t.Fatalf("saved legacy v1 direct link status = %d; body=%s", response.Code, response.Body.String())
	}
}

func purposeBoundTokenFromPath(t *testing.T, path, parameter string) string {
	t.Helper()
	parsed, err := url.Parse(path)
	if err != nil {
		t.Fatalf("parse purpose-bound path %q: %v", path, err)
	}
	token := parsed.Query().Get(parameter)
	if token == "" {
		t.Fatalf("purpose-bound path %q omitted %q", path, parameter)
	}
	return token
}

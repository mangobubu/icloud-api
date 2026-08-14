package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

// Legacy credentials remain valid for the restored v1 mail handlers, but they
// must never be interpreted as the v2 OTP credential bundle.
func TestLegacyCredentialsCannotAccessV2OTP(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.sync = nil
	env.store.ConfigureAliasCredentialFactory(nil)
	account := adminAPITestCreateAccount(t, env, "legacy-otp-boundary@icloud.com")
	rawKey := legacyCompatAPIKey(0x71)
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID:      account.ID,
		Address:        "legacy-otp-boundary-alias@icloud.com",
		APIKeyHash:     secure.HashToken(rawKey),
		APIKeyPrefix:   rawKey[:12],
		CredentialMode: domain.AliasCredentialModeLegacy,
		Enabled:        true,
	})
	if err != nil {
		t.Fatalf("create legacy alias: %v", err)
	}
	directToken, err := env.cipher.DirectLinkToken(alias.ID, alias.APIKeyHash)
	if err != nil {
		t.Fatalf("derive legacy direct link token: %v", err)
	}
	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build router: %v", err)
	}

	for name, request := range map[string]struct {
		target  string
		headers map[string]string
	}{
		"legacy bearer": {
			target:  "/api/v1/otp",
			headers: map[string]string{"Authorization": "Bearer " + rawKey},
		},
		"legacy derived link": {
			target: "/api/v1/otp?token=" + url.QueryEscape(directToken),
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := serveV2Request(router, http.MethodGet, request.target, "", request.headers)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("legacy v2 OTP status = %d; body=%s", response.Code, response.Body.String())
			}
		})
	}
}

// A legacy row must not gain v2 OAuth semantics merely because old/stale
// credential columns are populated during an upgrade.
func TestLegacyCredentialModeCannotExchangeOAuthRefreshToken(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.store.ConfigureAliasCredentialFactory(nil)
	account := adminAPITestCreateAccount(t, env, "legacy-oauth-boundary@icloud.com")
	rawKey := legacyCompatAPIKey(0x72)
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID:      account.ID,
		Address:        "legacy-oauth-boundary-alias@icloud.com",
		APIKeyHash:     secure.HashToken(rawKey),
		APIKeyPrefix:   rawKey[:12],
		CredentialMode: domain.AliasCredentialModeLegacy,
		Enabled:        true,
	})
	if err != nil {
		t.Fatalf("create legacy alias: %v", err)
	}
	const clientID = "icl_legacy_boundary_client"
	const refreshToken = "icr_legacy_boundary_refresh"
	if _, err := env.store.DB().ExecContext(context.Background(), `
		UPDATE aliases
		SET oauth_client_id = ?, refresh_token_hash = ?
		WHERE id = ?`, clientID, secure.HashToken(refreshToken), alias.ID); err != nil {
		t.Fatalf("seed stale OAuth columns: %v", err)
	}
	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}.Encode()
	response := serveV2Request(router, http.MethodPost, "/oauth2/v2.0/token", form, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("legacy OAuth exchange status = %d; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"invalid_client"`) {
		t.Fatalf("legacy OAuth exchange error = %s", response.Body.String())
	}
}

package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

func TestAdminAliasDTOKeepsLegacyDirectLinkWithoutInventingSecrets(t *testing.T) {
	env := newAdminAPITestEnv(t)
	account := adminAPITestCreateAccount(t, env, "legacy-admin@icloud.com")
	rawKey := legacyCompatAPIKey(0x63)
	env.store.ConfigureAliasCredentialFactory(nil)
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID: account.ID, Address: "legacy-admin-alias@icloud.com",
		APIKeyHash: secure.HashToken(rawKey), APIKeyPrefix: rawKey[:12],
		CredentialMode: domain.AliasCredentialModeLegacy, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	dto, err := env.server.adminAPIAliasFromDomain(alias)
	if err != nil {
		t.Fatalf("convert legacy alias: %v", err)
	}
	if dto.APIKey != "" || dto.IMAPPassword != "" || dto.ClientID != "" || dto.RefreshToken != "" {
		t.Fatalf("legacy DTO invented complete credentials: %#v", dto)
	}
	if dto.APIKeyPrefix != rawKey[:12] || dto.CredentialMode != domain.AliasCredentialModeLegacy ||
		dto.OTPURLPath == "" || dto.OTPURLPath != dto.LegacyDirectLink {
		t.Fatalf("legacy DTO omitted compatibility metadata: %#v", dto)
	}
	parsed, err := url.Parse(dto.LegacyDirectLink)
	if err != nil || parsed.Path != "/api/v1/mail/recent" {
		t.Fatalf("legacy direct link = %q, error=%v", dto.LegacyDirectLink, err)
	}
	token := parsed.Query().Get("api_key")
	if !env.cipher.VerifyDirectLinkToken(token, alias.ID, alias.APIKeyHash) {
		t.Fatal("legacy DTO direct-link token does not verify against existing key hash")
	}
}

func TestAdminJSONRendersLegacyAliasWithoutV2Ciphertext(t *testing.T) {
	env := newAdminAPITestEnv(t)
	account := adminAPITestCreateAccount(t, env, "legacy-render@icloud.com")
	rawKey := legacyCompatAPIKey(0x64)
	env.store.ConfigureAliasCredentialFactory(nil)
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID: account.ID, Address: "legacy-render-alias@icloud.com",
		APIKeyHash: secure.HashToken(rawKey), APIKeyPrefix: rawKey[:12],
		CredentialMode: domain.AliasCredentialModeLegacy, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie, _, _ := env.createSession(t, "legacy-render-admin", "unused-password")

	jsonResponse := env.request(t, http.MethodGet,
		"/admin/api/v1/aliases/"+strconvFormatInt(alias.ID), nil, "", []*http.Cookie{cookie}, "")
	if jsonResponse.Code != http.StatusOK {
		t.Fatalf("legacy admin JSON status=%d body=%s", jsonResponse.Code, jsonResponse.Body.String())
	}
	var payload struct {
		Data adminAPIAliasDTO `json:"data"`
	}
	if err := json.Unmarshal(jsonResponse.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.APIKey != "" || payload.Data.APIKeyPrefix != rawKey[:12] ||
		!strings.HasPrefix(payload.Data.LegacyDirectLink, "/api/v1/mail/recent?api_key=") {
		t.Fatalf("legacy admin JSON payload=%#v", payload.Data)
	}

}

func TestAdminAliasJSONKeepsLegacySummaryFieldsWhenEmpty(t *testing.T) {
	dto := adminAPIAliasDTO{ID: 41, Address: "pending-fields@icloud.com"}
	payload, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal alias DTO: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decode alias DTO fields: %v", err)
	}
	for _, name := range []string{"api_key_prefix", "direct_link_path"} {
		value, exists := fields[name]
		if !exists {
			t.Fatalf("legacy field %q omitted from alias JSON: %s", name, payload)
		}
		if string(value) != `""` {
			t.Fatalf("legacy field %q = %s, want empty string", name, value)
		}
	}
}

func TestLegacyAdminDTOIgnoresStaleV2CredentialColumns(t *testing.T) {
	env := newAdminAPITestEnv(t)
	account := adminAPITestCreateAccount(t, env, "legacy-stale-columns@icloud.com")
	rawKey := legacyCompatAPIKey(0x65)
	env.store.ConfigureAliasCredentialFactory(nil)
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID: account.ID, Address: "legacy-stale-columns-alias@icloud.com",
		APIKeyHash: secure.HashToken(rawKey), APIKeyPrefix: rawKey[:12],
		CredentialMode: domain.AliasCredentialModeLegacy, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, material, err := secure.NewAliasCredentialMaterial(env.cipher, alias.ID, 7)
	if err != nil {
		t.Fatalf("create stale v2 material: %v", err)
	}
	if _, err := env.store.DB().ExecContext(context.Background(), `
		UPDATE aliases
		SET credential_ciphertext = ?, credential_version = ?,
		    imap_password_hash = ?, oauth_client_id = ?, refresh_token_hash = ?
		WHERE id = ?`, material.Ciphertext, material.Version,
		material.IMAPPasswordHash, material.OAuthClientID, material.RefreshTokenHash, alias.ID); err != nil {
		t.Fatalf("seed stale v2 columns: %v", err)
	}
	stored, err := env.store.GetAlias(context.Background(), alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	dto, err := env.server.adminAPIAliasFromDomain(stored)
	if err != nil {
		t.Fatalf("convert legacy alias with stale columns: %v", err)
	}
	if dto.APIKey != "" || dto.IMAPPassword != "" || dto.ClientID != "" || dto.RefreshToken != "" {
		t.Fatalf("legacy DTO exposed stale v2 credentials: %#v", dto)
	}
	if dto.CredentialMode != domain.AliasCredentialModeLegacy || dto.OTPURLPath != dto.LegacyDirectLink {
		t.Fatalf("legacy compatibility metadata changed: %#v", dto)
	}
}

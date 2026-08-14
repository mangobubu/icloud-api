package httpserver

import (
	"context"
	"net/http"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

func TestAdminAPIRotateCredentialsRejectsLegacyAliasWithoutMutation(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "legacy-rotate-boundary", "unused-password")
	account := adminAPITestCreateAccount(t, env, "legacy-rotate-boundary@icloud.com")
	rawKey := legacyCompatAPIKey(0x7a)
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID:  account.ID,
		Address:    "legacy-rotate-boundary-alias@icloud.com",
		APIKeyHash: secure.HashToken(rawKey), APIKeyPrefix: rawKey[:12],
		CredentialMode: domain.AliasCredentialModeLegacy, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := env.store.GetAlias(context.Background(), alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	response := env.request(t, http.MethodPost,
		"/admin/api/v1/aliases/"+strconvFormatInt(alias.ID)+"/rotate-credentials",
		nil, "", []*http.Cookie{sessionCookie}, csrf)
	if response.Code != http.StatusConflict || adminAPITestErrorCode(t, response) != "ALIAS_CREDENTIAL_MODE_UNSUPPORTED" {
		t.Fatalf("legacy rotate-credentials response = %d %s", response.Code, response.Body.String())
	}
	after, err := env.store.GetAlias(context.Background(), alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameAliasCredentialState(before, after) {
		t.Fatalf("legacy alias changed after rejected HTTP rotation: before=%#v after=%#v", before, after)
	}
}

func TestAdminAPIRotateCredentialsRejectsConfirmationPendingAlias(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "pending-rotate-boundary", "unused-password")
	account := adminAPITestCreateAccount(t, env, "pending-rotate-boundary@icloud.com")
	alias, _ := createV2AliasFixture(t, env, account.ID, "pending-rotate-boundary-alias@icloud.com")
	if _, err := env.store.DB().ExecContext(context.Background(),
		`UPDATE aliases SET enabled = FALSE, last_sync_error = ? WHERE id = ?`,
		domain.AppleAliasConfirmationPending, alias.ID); err != nil {
		t.Fatal(err)
	}
	before, err := env.store.GetAlias(context.Background(), alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	response := env.request(t, http.MethodPost,
		"/admin/api/v1/aliases/"+strconvFormatInt(alias.ID)+"/rotate-credentials",
		nil, "", []*http.Cookie{sessionCookie}, csrf)
	if response.Code != http.StatusConflict || adminAPITestErrorCode(t, response) != "ALIAS_CONFIRMATION_PENDING" {
		t.Fatalf("pending rotate-credentials response = %d %s", response.Code, response.Body.String())
	}
	after, err := env.store.GetAlias(context.Background(), alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameAliasCredentialState(before, after) {
		t.Fatalf("pending alias changed after rejected HTTP rotation: before=%#v after=%#v", before, after)
	}
}

func sameAliasCredentialState(before, after domain.Alias) bool {
	return before.ID == after.ID && before.CredentialMode == after.CredentialMode &&
		before.CredentialVersion == after.CredentialVersion &&
		before.CredentialCiphertext == after.CredentialCiphertext &&
		before.APIKeyPrefix == after.APIKeyPrefix &&
		secure.HashEqual(before.APIKeyHash, after.APIKeyHash) &&
		secure.HashEqual(before.IMAPPasswordHash, after.IMAPPasswordHash) &&
		before.OAuthClientID == after.OAuthClientID &&
		secure.HashEqual(before.RefreshTokenHash, after.RefreshTokenHash) &&
		before.Enabled == after.Enabled && before.LastSyncError == after.LastSyncError
}

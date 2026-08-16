package httpserver

import (
	"context"
	"net/http"
	"testing"

	"icloud-api/internal/domain"
)

func TestAdminAPIMapsAliasIdentityConflictToAccountExists(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "account-identity-admin", "unused-account-identity-password")
	owner := adminAPITestCreateAccount(t, env, "admin-identity-owner@icloud.com")
	const reservedAddress = "admin-reserved@example.test"
	if _, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID: owner.ID,
		Address:   reservedAddress,
		Label:     "Reserved account identity",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("create reserved alias: %v", err)
	}

	create := env.request(t, http.MethodPost, "/admin/api/v1/accounts", adminAPITestJSON(t, map[string]any{
		"name":          "Conflicting account",
		"mailbox_type":  "icloud",
		"email":         reservedAddress,
		"imap_username": reservedAddress,
		"imap_password": "app-specific-password",
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if create.Code != http.StatusConflict || adminAPITestErrorCode(t, create) != "ACCOUNT_EXISTS" {
		t.Fatalf("conflicting account creation response = %d; body=%s", create.Code, create.Body.String())
	}

	target := adminAPITestCreateAccount(t, env, "admin-identity-target@icloud.com")
	update := env.request(t, http.MethodPut, "/admin/api/v1/accounts/"+strconvFormatInt(target.ID), adminAPITestJSON(t, map[string]any{
		"name":          target.Name,
		"mailbox_type":  "icloud",
		"email":         reservedAddress,
		"imap_username": target.IMAPUsername,
		"imap_password": "",
		"enabled":       true,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if update.Code != http.StatusConflict || adminAPITestErrorCode(t, update) != "ACCOUNT_EXISTS" {
		t.Fatalf("conflicting account update response = %d; body=%s", update.Code, update.Body.String())
	}
}

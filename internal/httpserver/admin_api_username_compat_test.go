package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

func TestAdminAPIAccountWithAliasesAllowsIMAPUsernameButLocksEmail(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "identity-update-admin", "unused-identity-update-password")
	account := adminAPITestCreateAccount(t, env, "identity-update@icloud.com")
	if _, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID:    account.ID,
		Address:      "identity-update-alias@privaterelay.appleid.com",
		Label:        "Identity update alias",
		APIKeyHash:   secure.HashToken("identity-update-alias-key"),
		APIKeyPrefix: "icm_identity",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create identity update alias: %v", err)
	}

	accountPath := legacyAdminAPIBasePath + "/accounts/" + strconvFormatInt(account.ID)
	updatedUsername := "new-login@icloud.com"
	usernameUpdate := env.request(t, http.MethodPut, accountPath, adminAPITestJSON(t, gin.H{
		"name":          account.Name,
		"email":         account.Email,
		"imap_username": updatedUsername,
		"imap_password": "",
		"enabled":       account.Enabled,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if usernameUpdate.Code != http.StatusOK {
		t.Fatalf("IMAP username update status = %d; body=%s", usernameUpdate.Code, usernameUpdate.Body.String())
	}
	var usernamePayload struct {
		Data adminAPIAccountDTO `json:"data"`
	}
	if err := json.Unmarshal(usernameUpdate.Body.Bytes(), &usernamePayload); err != nil {
		t.Fatalf("decode IMAP username update: %v; body=%s", err, usernameUpdate.Body.String())
	}
	if usernamePayload.Data.IMAPUsername != updatedUsername {
		t.Fatalf("updated IMAP username = %q, want %q", usernamePayload.Data.IMAPUsername, updatedUsername)
	}
	stored, err := env.store.GetAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("reload account after IMAP username update: %v", err)
	}
	if stored.IMAPUsername != updatedUsername {
		t.Fatalf("stored IMAP username = %q, want %q", stored.IMAPUsername, updatedUsername)
	}

	emailUpdate := env.request(t, http.MethodPut, accountPath, adminAPITestJSON(t, gin.H{
		"name":          account.Name,
		"email":         "changed-identity-update@icloud.com",
		"imap_username": updatedUsername,
		"imap_password": "",
		"enabled":       account.Enabled,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if emailUpdate.Code != http.StatusConflict || adminAPITestErrorCode(t, emailUpdate) != "ACCOUNT_IDENTITY_LOCKED" {
		t.Fatalf("primary email update response = %d; body=%s", emailUpdate.Code, emailUpdate.Body.String())
	}
	if !strings.Contains(emailUpdate.Body.String(), "已有隐私邮箱时不能修改主号邮箱") ||
		strings.Contains(emailUpdate.Body.String(), "IMAP 用户名") {
		t.Fatalf("primary email lock message = %s", emailUpdate.Body.String())
	}
}

package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestAdminAPICreateCustomAccountAndRandomAliases(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "custom-mailbox-admin", "unused-password")
	create := env.request(t, http.MethodPost, "/admin/api/v1/accounts", adminAPITestJSON(t, map[string]any{
		"name":          "custom mailbox",
		"mailbox_type":  "custom",
		"email_suffix":  "Example.Test",
		"imap_username": "reader@example.test",
		"imap_password": "  imap-secret  ",
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if create.Code != http.StatusCreated {
		t.Fatalf("custom account status = %d; body=%s", create.Code, create.Body.String())
	}
	type createdPayload struct {
		ID          int64  `json:"id"`
		MailboxType string `json:"mailbox_type"`
		Suffix      string `json:"email_suffix"`
	}
	var envelope struct {
		Data createdPayload `json:"data"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode custom account: %v", err)
	}
	if envelope.Data.ID < 1 || envelope.Data.MailboxType != domain.MailboxTypeCustom || envelope.Data.Suffix != "example.test" {
		t.Fatalf("custom account payload = %#v", envelope.Data)
	}
	second := env.request(t, http.MethodPost, "/admin/api/v1/accounts", adminAPITestJSON(t, map[string]any{
		"name":          "second custom mailbox",
		"mailbox_type":  "custom",
		"email_suffix":  "Other.Test",
		"imap_username": "reader@example.test",
		"imap_password": "second-secret",
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if second.Code != http.StatusCreated {
		t.Fatalf("same IMAP username with another suffix status = %d; body=%s", second.Code, second.Body.String())
	}
	duplicateSuffix := env.request(t, http.MethodPost, "/admin/api/v1/accounts", adminAPITestJSON(t, map[string]any{
		"name":          "duplicate custom mailbox",
		"mailbox_type":  "custom",
		"email_suffix":  "example.test",
		"imap_username": "another-login",
		"imap_password": "duplicate-secret",
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if duplicateSuffix.Code != http.StatusConflict || adminAPITestErrorCode(t, duplicateSuffix) != "ACCOUNT_EXISTS" {
		t.Fatalf("duplicate custom suffix response = %d; body=%s", duplicateSuffix.Code, duplicateSuffix.Body.String())
	}
	switchWithoutEmail := env.request(t, http.MethodPut, fmt.Sprintf("/admin/api/v1/accounts/%d", envelope.Data.ID), adminAPITestJSON(t, map[string]any{
		"name":          "custom mailbox",
		"mailbox_type":  "icloud",
		"imap_username": "reader@example.test",
		"imap_password": "",
		"enabled":       true,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if switchWithoutEmail.Code != http.StatusBadRequest {
		t.Fatalf("custom-to-iCloud switch without email = %d; body=%s", switchWithoutEmail.Code, switchWithoutEmail.Body.String())
	}
	overrideSuffix := env.request(t, http.MethodPost, fmt.Sprintf("/admin/api/v1/accounts/%d/aliases/random", envelope.Data.ID), adminAPITestJSON(t, map[string]any{
		"count":  1,
		"suffix": "other.test",
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if overrideSuffix.Code != http.StatusBadRequest {
		t.Fatalf("random suffix override status = %d; body=%s", overrideSuffix.Code, overrideSuffix.Body.String())
	}

	random := env.request(t, http.MethodPost, fmt.Sprintf("/admin/api/v1/accounts/%d/aliases/random", envelope.Data.ID), adminAPITestJSON(t, map[string]any{
		"count": 5,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if random.Code != http.StatusCreated {
		t.Fatalf("random alias status = %d; body=%s", random.Code, random.Body.String())
	}
	type result struct {
		Created []struct {
			Alias struct {
				Address string `json:"address"`
			} `json:"alias"`
			APIKey string `json:"api_key"`
		} `json:"created"`
		Count int `json:"count"`
	}
	var randomEnvelope struct {
		Data result `json:"data"`
	}
	if err := json.Unmarshal(random.Body.Bytes(), &randomEnvelope); err != nil {
		t.Fatalf("decode random aliases: %v", err)
	}
	if randomEnvelope.Data.Count != 5 || len(randomEnvelope.Data.Created) != 5 {
		t.Fatalf("random alias result = %#v", randomEnvelope.Data)
	}
	addressPattern := regexp.MustCompile(`^[a-z0-9]{8,12}@example\.test$`)
	seen := make(map[string]struct{}, 5)
	for _, item := range randomEnvelope.Data.Created {
		if !addressPattern.MatchString(item.Alias.Address) || item.APIKey == "" {
			t.Fatalf("invalid generated alias = %#v", item)
		}
		if _, duplicate := seen[item.Alias.Address]; duplicate {
			t.Fatalf("duplicate generated alias %q", item.Alias.Address)
		}
		seen[item.Alias.Address] = struct{}{}
	}
	// The strict import path used by the random generator must reject an
	// already-owned address without committing a duplicate or a partial batch.
	before, err := env.store.ListAliasesByAccount(context.Background(), envelope.Data.ID)
	if err != nil {
		t.Fatalf("list generated aliases before duplicate check: %v", err)
	}
	_, _, duplicateErr := env.store.ImportAliasesWithCredentialsStrict(
		context.Background(),
		envelope.Data.ID,
		[]domain.AliasImportCandidate{{Address: randomEnvelope.Data.Created[0].Alias.Address, Active: true}},
	)
	if !errors.Is(duplicateErr, store.ErrAliasOwnershipConflict) {
		t.Fatalf("duplicate import error = %v, want ownership conflict", duplicateErr)
	}
	after, err := env.store.ListAliasesByAccount(context.Background(), envelope.Data.ID)
	if err != nil {
		t.Fatalf("list generated aliases after duplicate check: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("duplicate import changed alias count from %d to %d", len(before), len(after))
	}
	deleteTarget := randomEnvelope.Data.Created[0].Alias.Address
	alias, err := env.store.GetAliasByAddress(context.Background(), deleteTarget)
	if err != nil {
		t.Fatalf("read generated alias before delete: %v", err)
	}
	deleteResponse := env.request(t, http.MethodDelete, fmt.Sprintf("/admin/api/v1/aliases/%d", alias.ID), nil, "", []*http.Cookie{sessionCookie}, csrf)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete custom alias status = %d; body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if _, err := env.store.GetAliasByAddress(context.Background(), deleteTarget); err == nil {
		t.Fatalf("custom alias %q remains after local delete", deleteTarget)
	}
	account, err := env.store.GetAccount(context.Background(), envelope.Data.ID)
	if err != nil {
		t.Fatalf("read custom account: %v", err)
	}
	if account.MailboxType != domain.MailboxTypeCustom || account.EmailSuffix != "example.test" {
		t.Fatalf("stored mailbox settings = %#v", account)
	}
	password, err := env.cipher.Decrypt(account.PasswordCiphertext)
	if err != nil || password != "  imap-secret  " {
		t.Fatalf("stored custom IMAP password preserved=%t err=%v", password == "  imap-secret  ", err)
	}
}

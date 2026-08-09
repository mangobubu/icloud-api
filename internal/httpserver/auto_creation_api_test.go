package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"icloud-api/internal/domain"
)

// testAliasAutoCreationService is only used to get the PUT endpoint past its
// optional-service guard. These error paths must return before either method
// is invoked, so an unexpected call is deliberately treated as a test failure.
type testAliasAutoCreationService struct {
	t *testing.T
}

func (s *testAliasAutoCreationService) GetSchedule(context.Context, int64) (domain.AliasCreationSchedule, error) {
	s.t.Fatal("GetSchedule should not be called for this request")
	return domain.AliasCreationSchedule{}, nil
}

func (s *testAliasAutoCreationService) SetEnabled(context.Context, int64, bool) (domain.AliasCreationSchedule, error) {
	s.t.Fatal("SetEnabled should not be called for this request")
	return domain.AliasCreationSchedule{}, nil
}

func TestAdminAPISetAliasAutoCreationRejectsDisabledAccount(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.autoCreate = &testAliasAutoCreationService{t: t}
	sessionCookie, csrf, _ := env.createSession(t, "auto-create-disabled-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "auto-create-disabled@icloud.com")
	account.Enabled = false
	if _, err := env.store.UpdateAccount(context.Background(), account); err != nil {
		t.Fatalf("disable account for auto-creation test: %v", err)
	}

	response := env.request(
		t,
		http.MethodPut,
		fmt.Sprintf("/admin/api/v1/accounts/%d/aliases/auto-create", account.ID),
		adminAPITestJSON(t, map[string]bool{"enabled": true}),
		"application/json",
		[]*http.Cookie{sessionCookie},
		csrf,
	)
	if response.Code != http.StatusConflict {
		t.Fatalf("disabled account auto-creation status = %d, want %d; body=%s", response.Code, http.StatusConflict, response.Body.String())
	}
	if code := adminAPITestErrorCode(t, response); code != "ACCOUNT_DISABLED" {
		t.Fatalf("disabled account auto-creation code = %q, want ACCOUNT_DISABLED", code)
	}
}

func TestAdminAPISetAliasAutoCreationReturnsNotFoundForMissingAccount(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.autoCreate = &testAliasAutoCreationService{t: t}
	sessionCookie, csrf, _ := env.createSession(t, "auto-create-missing-admin", "unused-password")
	const missingAccountID int64 = 987654321

	response := env.request(
		t,
		http.MethodPut,
		fmt.Sprintf("/admin/api/v1/accounts/%d/aliases/auto-create", missingAccountID),
		adminAPITestJSON(t, map[string]bool{"enabled": true}),
		"application/json",
		[]*http.Cookie{sessionCookie},
		csrf,
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing account auto-creation status = %d, want %d; body=%s", response.Code, http.StatusNotFound, response.Body.String())
	}
	if code := adminAPITestErrorCode(t, response); code != "NOT_FOUND" {
		t.Fatalf("missing account auto-creation code = %q, want NOT_FOUND", code)
	}
}

func TestAdminAPIAcknowledgeAliasAutoCreationKeysEnforcesBatchLimit(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "auto-create-key-ack-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "auto-create-key-ack@icloud.com")
	target := fmt.Sprintf("/admin/api/v1/accounts/%d/aliases/auto-create/keys", account.ID)
	aliasIDs := make([]int64, domain.MaxEnabledAliasesPerAccount+1)
	for index := range aliasIDs {
		aliasIDs[index] = int64(index + 1)
	}

	tooLarge := env.request(
		t,
		http.MethodDelete,
		target,
		adminAPITestJSON(t, map[string]any{"alias_ids": aliasIDs}),
		"application/json",
		[]*http.Cookie{sessionCookie},
		csrf,
	)
	if tooLarge.Code != http.StatusBadRequest || adminAPITestErrorCode(t, tooLarge) != "VALIDATION_FAILED" {
		t.Fatalf("oversized key acknowledgement = %d; body=%s", tooLarge.Code, tooLarge.Body.String())
	}

	allowed := env.request(
		t,
		http.MethodDelete,
		target,
		adminAPITestJSON(t, map[string]any{"alias_ids": aliasIDs[:domain.MaxEnabledAliasesPerAccount]}),
		"application/json",
		[]*http.Cookie{sessionCookie},
		csrf,
	)
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("maximum-sized key acknowledgement = %d; body=%s", allowed.Code, allowed.Body.String())
	}
}

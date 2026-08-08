package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

func TestLegacyAliasesPageFiltersByAccount(t *testing.T) {
	env := newHTTPTestEnv(t)
	accountA := env.createAccount(t, "Primary A", "legacy-filter-a@icloud.com", "encrypted-a")
	accountB := env.createAccount(t, "Primary B", "legacy-filter-b@icloud.com", "encrypted-b")
	accountEmpty := env.createAccount(t, "Primary Empty", "legacy-filter-empty@icloud.com", "encrypted-empty")
	createLegacyFilterAlias(t, env, accountA.ID, "legacy-filter-a-alias@icloud.com")
	createLegacyFilterAlias(t, env, accountB.ID, "legacy-filter-b-alias@icloud.com")
	session := env.createAdminSession(t)

	allResponse := env.request(t, http.MethodGet, "/admin/aliases", nil, []*http.Cookie{session})
	if allResponse.Code != http.StatusOK {
		t.Fatalf("GET all aliases status = %d, want %d; body=%s", allResponse.Code, http.StatusOK, allResponse.Body.String())
	}
	allBody := allResponse.Body.String()
	assertLegacyAliasFilterFragment(t, allBody, `name="account_id"`)
	assertLegacyAliasFilterFragment(t, allBody, `value="" selected>全部主号`)
	assertLegacyAliasFilterFragment(t, allBody, fmt.Sprintf(`value="%d">legacy-filter-a@icloud.com`, accountA.ID))
	assertLegacyAliasFilterFragment(t, allBody, fmt.Sprintf(`value="%d">legacy-filter-b@icloud.com`, accountB.ID))
	assertLegacyAliasFilterFragment(t, allBody, `data-sync-poll-endpoint="/admin/api/v1/aliases"`)
	assertLegacyAliasFilterFragment(t, allBody, "legacy-filter-a-alias@icloud.com")
	assertLegacyAliasFilterFragment(t, allBody, "legacy-filter-b-alias@icloud.com")

	filteredTarget := fmt.Sprintf("/admin/aliases?account_id=%d", accountA.ID)
	filteredResponse := env.request(t, http.MethodGet, filteredTarget, nil, []*http.Cookie{session})
	if filteredResponse.Code != http.StatusOK {
		t.Fatalf("GET filtered aliases status = %d, want %d; body=%s", filteredResponse.Code, http.StatusOK, filteredResponse.Body.String())
	}
	filteredBody := filteredResponse.Body.String()
	assertLegacyAliasFilterFragment(t, filteredBody, fmt.Sprintf(`data-sync-poll-endpoint="/admin/api/v1/aliases?account_id=%d"`, accountA.ID))
	assertLegacyAliasFilterFragment(t, filteredBody, fmt.Sprintf(`value="%d" selected>legacy-filter-a@icloud.com`, accountA.ID))
	assertLegacyAliasFilterFragment(t, filteredBody, "legacy-filter-a-alias@icloud.com")
	if strings.Contains(filteredBody, "legacy-filter-b-alias@icloud.com") {
		t.Fatalf("filtered page rendered an alias belonging to another account: %s", filteredBody)
	}

	emptyTarget := fmt.Sprintf("/admin/aliases?account_id=%d", accountEmpty.ID)
	emptyResponse := env.request(t, http.MethodGet, emptyTarget, nil, []*http.Cookie{session})
	if emptyResponse.Code != http.StatusOK {
		t.Fatalf("GET empty filtered aliases status = %d, want %d; body=%s", emptyResponse.Code, http.StatusOK, emptyResponse.Body.String())
	}
	emptyBody := emptyResponse.Body.String()
	assertLegacyAliasFilterFragment(t, emptyBody, "这个主号还没有隐私邮箱")
	assertLegacyAliasFilterFragment(t, emptyBody, fmt.Sprintf(`href="/admin/accounts/%d">管理主号`, accountEmpty.ID))
	if strings.Contains(emptyBody, "legacy-filter-a-alias@icloud.com") || strings.Contains(emptyBody, "legacy-filter-b-alias@icloud.com") {
		t.Fatalf("empty filtered page rendered aliases from another account: %s", emptyBody)
	}
}

func TestLegacyAliasesPageFallsBackForInvalidAccountFilter(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Primary", "legacy-invalid-filter@icloud.com", "encrypted")
	createLegacyFilterAlias(t, env, account.ID, "legacy-invalid-filter-alias@icloud.com")
	session := env.createAdminSession(t)

	for _, rawAccountID := range []string{"not-a-number", "0", "999999999"} {
		t.Run(rawAccountID, func(t *testing.T) {
			response := env.request(t, http.MethodGet, "/admin/aliases?account_id="+rawAccountID, nil, []*http.Cookie{session})
			if response.Code != http.StatusOK {
				t.Fatalf("GET invalid filtered aliases status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
			}
			body := response.Body.String()
			assertLegacyAliasFilterFragment(t, body, "legacy-invalid-filter-alias@icloud.com")
			assertLegacyAliasFilterFragment(t, body, `data-sync-poll-endpoint="/admin/api/v1/aliases"`)
			if strings.Contains(body, ` selected>legacy-invalid-filter@icloud.com`) {
				t.Fatalf("invalid account filter remained selected: %s", body)
			}
		})
	}
}

func createLegacyFilterAlias(t *testing.T, env *httpTestEnv, accountID int64, address string) domain.Alias {
	t.Helper()
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID:    accountID,
		Address:      address,
		APIKeyHash:   secure.HashToken(address + "-key"),
		APIKeyPrefix: "legacy-filter-key",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create legacy filter alias %q: %v", address, err)
	}
	return alias
}

func assertLegacyAliasFilterFragment(t *testing.T, body, fragment string) {
	t.Helper()
	if !strings.Contains(body, fragment) {
		t.Fatalf("legacy aliases page is missing %q: %s", fragment, body)
	}
}

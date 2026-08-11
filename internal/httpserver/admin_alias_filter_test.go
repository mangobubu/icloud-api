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

func TestLegacyAliasesPageSearchesAddressAndLabel(t *testing.T) {
	env := newHTTPTestEnv(t)
	accountA := env.createAccount(t, "Primary A", "legacy-search-a@icloud.com", "encrypted-a")
	accountB := env.createAccount(t, "Primary B", "legacy-search-b@icloud.com", "encrypted-b")
	createLegacyFilterAlias(t, env, accountA.ID, "alpha-legacy-search@icloud.com", "Checkout Inbox")
	createLegacyFilterAlias(t, env, accountB.ID, "bravo-legacy-search@icloud.com", "Personal")
	session := env.createAdminSession(t)

	labelSearch := env.request(t, http.MethodGet, "/admin/aliases?query=CHECKOUT", nil, []*http.Cookie{session})
	if labelSearch.Code != http.StatusOK {
		t.Fatalf("GET searched aliases status = %d, want %d; body=%s", labelSearch.Code, http.StatusOK, labelSearch.Body.String())
	}
	labelBody := labelSearch.Body.String()
	assertLegacyAliasFilterFragment(t, labelBody, `name="query" value="CHECKOUT"`)
	assertLegacyAliasFilterFragment(t, labelBody, `data-sync-poll-endpoint="/admin/api/v1/aliases?query=CHECKOUT"`)
	assertLegacyAliasFilterFragment(t, labelBody, "alpha-legacy-search@icloud.com")
	if strings.Contains(labelBody, "bravo-legacy-search@icloud.com") {
		t.Fatalf("label search rendered an unrelated alias: %s", labelBody)
	}

	combinedTarget := fmt.Sprintf("/admin/aliases?account_id=%d&query=BRAVO", accountA.ID)
	combined := env.request(t, http.MethodGet, combinedTarget, nil, []*http.Cookie{session})
	if combined.Code != http.StatusOK {
		t.Fatalf("GET combined alias filters status = %d, want %d; body=%s", combined.Code, http.StatusOK, combined.Body.String())
	}
	combinedBody := combined.Body.String()
	assertLegacyAliasFilterFragment(t, combinedBody, "没有匹配的隐私邮箱")
	assertLegacyAliasFilterFragment(t, combinedBody, fmt.Sprintf(`href="/admin/aliases?account_id=%d">清除搜索`, accountA.ID))
	assertLegacyAliasFilterFragment(t, combinedBody, fmt.Sprintf(`data-sync-poll-endpoint="/admin/api/v1/aliases?account_id=%d&amp;query=BRAVO"`, accountA.ID))
}

func createLegacyFilterAlias(t *testing.T, env *httpTestEnv, accountID int64, address string, labels ...string) domain.Alias {
	t.Helper()
	label := ""
	if len(labels) > 0 {
		label = labels[0]
	}
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID:    accountID,
		Address:      address,
		Label:        label,
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

package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
)

type adminAPIAccountDetailTestEnvelope struct {
	Data struct {
		Account struct {
			AliasCount int `json:"alias_count"`
		} `json:"account"`
		Aliases    []adminAPIAliasDTO `json:"aliases"`
		Pagination struct {
			Limit   int  `json:"limit"`
			Offset  int  `json:"offset"`
			Total   int  `json:"total"`
			HasMore bool `json:"has_more"`
		} `json:"pagination"`
	} `json:"data"`
}

func TestAdminAPIAccountDetailPaginatesAliasesAndSyncDefaultsAreBounded(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "account-page-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "account-page@icloud.com")
	adminAPITestCreateAliases(t, env, account.ID, 27, "detail")
	path := fmt.Sprintf("/admin/api/v1/accounts/%d", account.ID)

	defaultResponse := env.request(t, http.MethodGet, path, nil, "", []*http.Cookie{cookie}, "")
	defaultPayload := decodeAdminAPIAccountDetailTestEnvelope(t, defaultResponse)
	assertAdminAPIAccountAliasPage(t, defaultPayload, 20, 20, 0, 27, true, 27)
	if got := defaultPayload.Data.Aliases[0].Address; got != "detail-000@icloud.com" {
		t.Fatalf("default first alias = %q, want detail-000@icloud.com", got)
	}

	pageResponse := env.request(t, http.MethodGet, path+"?limit=5&offset=20", nil, "", []*http.Cookie{cookie}, "")
	pagePayload := decodeAdminAPIAccountDetailTestEnvelope(t, pageResponse)
	assertAdminAPIAccountAliasPage(t, pagePayload, 5, 5, 20, 27, true, 27)
	if got := pagePayload.Data.Aliases[0].Address; got != "detail-020@icloud.com" {
		t.Fatalf("offset page first alias = %q, want detail-020@icloud.com", got)
	}

	lastResponse := env.request(t, http.MethodGet, path+"?limit=5&offset=25", nil, "", []*http.Cookie{cookie}, "")
	lastPayload := decodeAdminAPIAccountDetailTestEnvelope(t, lastResponse)
	assertAdminAPIAccountAliasPage(t, lastPayload, 2, 5, 25, 27, false, 27)

	syncResponse := env.request(t, http.MethodPost, path+"/sync", nil, "", []*http.Cookie{cookie}, csrf)
	syncPayload := decodeAdminAPIAccountDetailTestEnvelope(t, syncResponse)
	assertAdminAPIAccountAliasPage(t, syncPayload, 20, 20, 0, 27, true, 27)
}

func TestAdminAPIAccountDetailUsesAliasListPaginationValidation(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, _, _ := env.createSession(t, "account-page-validation-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "account-page-validation@icloud.com")
	path := fmt.Sprintf("/admin/api/v1/accounts/%d", account.ID)

	for _, query := range []string{
		"limit=0",
		"limit=1001",
		"limit=invalid",
		"offset=-1",
		"offset=1000001",
		"offset=invalid",
	} {
		t.Run(query, func(t *testing.T) {
			response := env.request(t, http.MethodGet, path+"?"+query, nil, "", []*http.Cookie{cookie}, "")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("GET account with %s status = %d, body=%s", query, response.Code, response.Body.String())
			}
			if code := adminAPITestErrorCode(t, response); code != "VALIDATION_FAILED" {
				t.Fatalf("GET account with %s error = %q, want VALIDATION_FAILED", query, code)
			}
		})
	}
}

func TestAdminAPIAppleSyncKeepsCreatedCredentialsOutsideBoundedAliasPage(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "apple-page-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "apple-page@icloud.com")
	adminAPITestCreateAliases(t, env, account.ID, 25, "apple-page")

	var createdID int64
	env.server.SetHMESyncService(&fakeHMESyncService{
		syncAliases: func(ctx context.Context, accountID int64) (hmesync.SyncResult, error) {
			created, err := env.store.CreateAlias(ctx, domain.Alias{
				AccountID: accountID,
				Address:   "zzzz-created@privaterelay.appleid.com",
				Enabled:   true,
			})
			if err != nil {
				return hmesync.SyncResult{}, err
			}
			createdID = created.ID
			return hmesync.SyncResult{
				Summary: hmesync.SyncSummary{Total: 26, CreatedCount: 1, ExistingCount: 25},
				Created: []hmesync.CreatedAlias{{Alias: created}},
				Session: hmesync.SessionInfo{Status: hmesync.StatusAuthenticated},
			}, nil
		},
	})

	path := fmt.Sprintf("/admin/api/v1/accounts/%d/aliases/sync", account.ID)
	response := env.request(t, http.MethodPost, path, nil, "", []*http.Cookie{cookie}, csrf)
	if response.Code != http.StatusOK {
		t.Fatalf("Apple sync status = %d, body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Account struct {
				AliasCount int `json:"alias_count"`
			} `json:"account"`
			Aliases    []adminAPIAliasDTO             `json:"aliases"`
			Created    []adminAPIAppleCreatedAliasDTO `json:"created"`
			Pagination struct {
				Limit   int  `json:"limit"`
				Offset  int  `json:"offset"`
				Total   int  `json:"total"`
				HasMore bool `json:"has_more"`
			} `json:"pagination"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode Apple sync response: %v; body=%s", err, response.Body.String())
	}
	if payload.Data.Account.AliasCount != 26 || len(payload.Data.Aliases) != 20 ||
		payload.Data.Pagination.Limit != 20 || payload.Data.Pagination.Offset != 0 ||
		payload.Data.Pagination.Total != 26 || !payload.Data.Pagination.HasMore {
		t.Fatalf("Apple sync bounded detail = %#v", payload.Data)
	}
	if len(payload.Data.Created) != 1 || payload.Data.Created[0].Alias.ID != createdID ||
		payload.Data.Created[0].Alias.APIKey == "" {
		t.Fatalf("Apple sync created credential envelope = %#v", payload.Data.Created)
	}
	for _, alias := range payload.Data.Aliases {
		if alias.ID == createdID {
			t.Fatalf("page-outside created alias %d was appended to bounded aliases", createdID)
		}
	}
}

func adminAPITestCreateAliases(t *testing.T, env *adminAPITestEnv, accountID int64, count int, prefix string) {
	t.Helper()
	for index := 0; index < count; index++ {
		if _, err := env.store.CreateAlias(context.Background(), domain.Alias{
			AccountID: accountID,
			Address:   fmt.Sprintf("%s-%03d@icloud.com", prefix, index),
			Enabled:   true,
		}); err != nil {
			t.Fatalf("create alias fixture %d: %v", index, err)
		}
	}
}

func decodeAdminAPIAccountDetailTestEnvelope(
	t *testing.T,
	response *httptest.ResponseRecorder,
) adminAPIAccountDetailTestEnvelope {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("account detail status = %d, body=%s", response.Code, response.Body.String())
	}
	var payload adminAPIAccountDetailTestEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode account detail: %v; body=%s", err, response.Body.String())
	}
	return payload
}

func assertAdminAPIAccountAliasPage(
	t *testing.T,
	payload adminAPIAccountDetailTestEnvelope,
	wantLength int,
	wantLimit int,
	wantOffset int,
	wantTotal int,
	wantHasMore bool,
	wantAliasCount int,
) {
	t.Helper()
	if len(payload.Data.Aliases) != wantLength ||
		payload.Data.Pagination.Limit != wantLimit ||
		payload.Data.Pagination.Offset != wantOffset ||
		payload.Data.Pagination.Total != wantTotal ||
		payload.Data.Pagination.HasMore != wantHasMore ||
		payload.Data.Account.AliasCount != wantAliasCount {
		t.Fatalf("account alias page = %#v", payload.Data)
	}
}

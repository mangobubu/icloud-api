package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"icloud-api/internal/domain"
)

func TestAdminMailGroupLifecycleAndAliasMove(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "groups-admin", "groups-password")
	account := adminAPITestCreateAccount(t, env, "groups-owner@icloud.com")
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID: account.ID,
		Address:   "groups-alias@icloud.com",
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}

	createdResponse := env.request(t, http.MethodPost, "/admin/api/v1/groups",
		adminAPITestJSON(t, map[string]any{"name": "工作"}), "application/json", []*http.Cookie{cookie}, csrf)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create group status = %d, body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created struct {
		Data adminAPIMailGroupDTO `json:"data"`
	}
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created group: %v", err)
	}
	if created.Data.Name != "工作" || created.Data.ID < 1 {
		t.Fatalf("created group = %#v", created.Data)
	}

	movedResponse := env.request(t, http.MethodPatch,
		"/admin/api/v1/aliases/"+strconv.FormatInt(alias.ID, 10)+"/group",
		adminAPITestJSON(t, map[string]any{"group_id": created.Data.ID}), "application/json", []*http.Cookie{cookie}, csrf)
	if movedResponse.Code != http.StatusOK {
		t.Fatalf("move alias status = %d, body=%s", movedResponse.Code, movedResponse.Body.String())
	}
	var moved struct {
		Data adminAPIAliasDTO `json:"data"`
	}
	if err := json.Unmarshal(movedResponse.Body.Bytes(), &moved); err != nil {
		t.Fatalf("decode moved alias: %v", err)
	}
	if moved.Data.GroupID == nil || *moved.Data.GroupID != created.Data.ID || moved.Data.GroupName != "工作" {
		t.Fatalf("moved alias = %#v", moved.Data)
	}

	listResponse := env.request(t, http.MethodGet,
		"/admin/api/v1/aliases?group_id="+strconv.FormatInt(created.Data.ID, 10)+"&limit=20&offset=0",
		nil, "", []*http.Cookie{cookie}, csrf)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list grouped aliases status = %d, body=%s", listResponse.Code, listResponse.Body.String())
	}

	deleteResponse := env.request(t, http.MethodDelete,
		"/admin/api/v1/groups/"+strconv.FormatInt(created.Data.ID, 10), nil, "", []*http.Cookie{cookie}, csrf)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete group status = %d, body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	ungrouped, err := env.store.GetAlias(context.Background(), alias.ID)
	if err != nil {
		t.Fatalf("read alias after group delete: %v", err)
	}
	if ungrouped.GroupID != nil || ungrouped.GroupName != "" {
		t.Fatalf("alias after group delete = %#v", ungrouped)
	}

	noneResponse := env.request(t, http.MethodGet,
		"/admin/api/v1/aliases?group_id=none&limit=20&offset=0",
		nil, "", []*http.Cookie{cookie}, csrf)
	if noneResponse.Code != http.StatusOK {
		t.Fatalf("list ungrouped aliases status = %d, body=%s", noneResponse.Code, noneResponse.Body.String())
	}
}

func TestAdminMailGroupValidationAndBatchMoveRollback(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "groups-validation-admin", "groups-validation-password")
	account := adminAPITestCreateAccount(t, env, "groups-validation-owner@icloud.com")
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID: account.ID,
		Address:   "groups-validation-alias@icloud.com",
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}

	createdResponse := env.request(t, http.MethodPost, "/admin/api/v1/groups",
		adminAPITestJSON(t, map[string]any{"name": "Équipe"}),
		"application/json", []*http.Cookie{cookie}, csrf)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create Unicode group status = %d, body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created struct {
		Data adminAPIMailGroupDTO `json:"data"`
	}
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created Unicode group: %v", err)
	}

	duplicateResponse := env.request(t, http.MethodPost, "/admin/api/v1/groups",
		adminAPITestJSON(t, map[string]any{"name": "e\u0301QUIPE"}),
		"application/json", []*http.Cookie{cookie}, csrf)
	if duplicateResponse.Code != http.StatusConflict || adminAPITestErrorCode(t, duplicateResponse) != "GROUP_EXISTS" {
		t.Fatalf("duplicate group status = %d, body=%s", duplicateResponse.Code, duplicateResponse.Body.String())
	}

	tooLongResponse := env.request(t, http.MethodPost, "/admin/api/v1/groups",
		adminAPITestJSON(t, map[string]any{"name": strings.Repeat("组", 101)}),
		"application/json", []*http.Cookie{cookie}, csrf)
	if tooLongResponse.Code != http.StatusBadRequest || adminAPITestErrorCode(t, tooLongResponse) != "VALIDATION_FAILED" {
		t.Fatalf("long group name status = %d, body=%s", tooLongResponse.Code, tooLongResponse.Body.String())
	}
	invalidAliasIDResponse := env.request(t, http.MethodPatch, "/admin/api/v1/aliases/group",
		adminAPITestJSON(t, map[string]any{
			"alias_ids": []int64{0},
			"group_id":  nil,
		}), "application/json", []*http.Cookie{cookie}, csrf)
	if invalidAliasIDResponse.Code != http.StatusBadRequest || adminAPITestErrorCode(t, invalidAliasIDResponse) != "VALIDATION_FAILED" {
		t.Fatalf("invalid batch alias ID status = %d, body=%s", invalidAliasIDResponse.Code, invalidAliasIDResponse.Body.String())
	}

	missingGroupID := created.Data.ID + 1000
	missingGroupResponse := env.request(t, http.MethodPatch,
		"/admin/api/v1/aliases/"+strconv.FormatInt(alias.ID, 10)+"/group",
		adminAPITestJSON(t, map[string]any{"group_id": missingGroupID}),
		"application/json", []*http.Cookie{cookie}, csrf)
	if missingGroupResponse.Code != http.StatusNotFound || adminAPITestErrorCode(t, missingGroupResponse) != "GROUP_NOT_FOUND" {
		t.Fatalf("missing group move status = %d, body=%s", missingGroupResponse.Code, missingGroupResponse.Body.String())
	}

	groupID := created.Data.ID
	if _, err := env.store.SetAliasGroup(context.Background(), alias.ID, &groupID); err != nil {
		t.Fatalf("put alias in rollback fixture group: %v", err)
	}
	batchResponse := env.request(t, http.MethodPatch, "/admin/api/v1/aliases/group",
		adminAPITestJSON(t, map[string]any{
			"alias_ids": []int64{alias.ID, alias.ID + 1000},
			"group_id":  nil,
		}), "application/json", []*http.Cookie{cookie}, csrf)
	if batchResponse.Code != http.StatusNotFound || adminAPITestErrorCode(t, batchResponse) != "NOT_FOUND" {
		t.Fatalf("batch move with missing alias status = %d, body=%s", batchResponse.Code, batchResponse.Body.String())
	}
	got, err := env.store.GetAlias(context.Background(), alias.ID)
	if err != nil {
		t.Fatalf("read alias after failed batch move: %v", err)
	}
	if got.GroupID == nil || *got.GroupID != groupID {
		t.Fatalf("failed batch move partially changed alias: %#v", got)
	}
}

package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

func TestAdminAliasPagesRenderPerAliasSyncStatus(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Status Account", "status-account@icloud.com", "encrypted")
	session := env.createAdminSession(t)
	syncedAt := time.Date(2026, time.August, 7, 9, 8, 0, 0, time.UTC)
	errorMessage := "  IMAP 连接失败\n" + strings.Repeat("详细原因", 30) + "  "

	aliases := []domain.Alias{
		{
			AccountID:      account.ID,
			Address:        "pending-status@icloud.com",
			APIKeyHash:     secure.HashToken("pending-status-key"),
			APIKeyPrefix:   "pending-key",
			Enabled:        true,
			LastSyncStatus: domain.SyncStatusPending,
		},
		{
			AccountID:      account.ID,
			Address:        "ok-status@icloud.com",
			APIKeyHash:     secure.HashToken("ok-status-key"),
			APIKeyPrefix:   "ok-key",
			Enabled:        true,
			LastSyncStatus: domain.SyncStatusOK,
			LastSyncedAt:   &syncedAt,
		},
		{
			AccountID:      account.ID,
			Address:        "error-status@icloud.com",
			APIKeyHash:     secure.HashToken("error-status-key"),
			APIKeyPrefix:   "error-key",
			Enabled:        true,
			LastSyncStatus: domain.SyncStatusError,
			LastSyncError:  errorMessage,
			LastSyncedAt:   &syncedAt,
		},
		{
			AccountID:      account.ID,
			Address:        "disabled-status@icloud.com",
			APIKeyHash:     secure.HashToken("disabled-status-key"),
			APIKeyPrefix:   "disabled-key",
			Enabled:        false,
			LastSyncStatus: domain.SyncStatusOK,
			LastSyncedAt:   &syncedAt,
		},
	}
	for _, alias := range aliases {
		if _, err := env.store.CreateAlias(context.Background(), alias); err != nil {
			t.Fatalf("create alias %q: %v", alias.Address, err)
		}
	}

	accountsResponse := env.request(t, http.MethodGet, "/admin", nil, []*http.Cookie{session})
	if accountsResponse.Code != http.StatusOK {
		t.Fatalf("GET /admin status = %d, want %d; body=%s", accountsResponse.Code, http.StatusOK, accountsResponse.Body.String())
	}
	accountsBody := accountsResponse.Body.String()
	if !strings.Contains(accountsBody, `data-sync-poll-page="accounts" data-sync-poll-endpoint="/admin/api/v1/accounts"`) {
		t.Fatalf("GET /admin omitted account sync polling metadata: %s", accountsBody)
	}
	accountRow := aliasStatusTableRow(t, accountsBody, account.Email)
	assertAliasStatusFragment(t, accountRow, `data-sync-record data-sync-kind="account"`)
	assertAliasStatusFragment(t, accountRow, `data-sync-status-cell`)
	assertAliasStatusFragment(t, accountRow, `data-sync-primary-time`)

	targets := []string{
		fmt.Sprintf("/admin/accounts/%d", account.ID),
		"/admin/aliases",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			response := env.request(t, http.MethodGet, target, nil, []*http.Cookie{session})
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d; body=%s", target, response.Code, http.StatusOK, response.Body.String())
			}
			body := response.Body.String()
			expectedEndpoint := "/admin/api/v1/aliases"
			if target != "/admin/aliases" {
				expectedEndpoint = fmt.Sprintf("/admin/api/v1/accounts/%d", account.ID)
			}
			if !strings.Contains(body, `data-sync-poll-endpoint="`+expectedEndpoint+`"`) {
				t.Fatalf("GET %s omitted sync polling endpoint %q", target, expectedEndpoint)
			}

			pendingRow := aliasStatusTableRow(t, body, "pending-status@icloud.com")
			assertAliasStatusFragment(t, pendingRow, `status-pending">待同步</span>`)
			assertAliasStatusFragment(t, pendingRow, `data-sync-record data-sync-kind="alias"`)
			if strings.Contains(pendingRow, "同步于") {
				t.Fatalf("pending alias unexpectedly rendered a sync time: %s", pendingRow)
			}
			if strings.Contains(pendingRow, "data-sync-at=") {
				t.Fatalf("pending alias unexpectedly exposed a sync timestamp: %s", pendingRow)
			}

			okRow := aliasStatusTableRow(t, body, "ok-status@icloud.com")
			assertAliasStatusFragment(t, okRow, `status-ok">正常</span>`)
			assertAliasStatusFragment(t, okRow, "同步于 "+formatOptionalTime(&syncedAt))
			assertAliasStatusFragment(t, okRow, fmt.Sprintf(`data-sync-at="%d"`, syncedAt.Unix()))
			assertAliasStatusFragment(t, okRow, `data-sync-status-cell data-sync-details`)

			errorRow := aliasStatusTableRow(t, body, "error-status@icloud.com")
			assertAliasStatusFragment(t, errorRow, `status-error">同步异常</span>`)
			assertAliasStatusFragment(t, errorRow, "错误："+compactSyncError(errorMessage))
			assertAliasStatusFragment(t, errorRow, "尝试于 "+formatOptionalTime(&syncedAt))

			disabledRow := aliasStatusTableRow(t, body, "disabled-status@icloud.com")
			assertAliasStatusFragment(t, disabledRow, `status-muted">已停用</span>`)
			if strings.Contains(disabledRow, ">正常</span>") {
				t.Fatalf("disabled alias also rendered a normal status: %s", disabledRow)
			}
		})
	}
}

func TestLegacyPendingAliasIsLockedUntilDirectoryConfirmation(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Pending Confirmation", "pending-confirmation@icloud.com", "encrypted")
	session := env.createAdminSession(t)
	pending, _, err := env.store.CreateAliasWithPendingAPIKey(
		context.Background(),
		domain.AppleWebSession{
			AccountID:     account.ID,
			Ciphertext:    "as1.legacy-pending-test",
			AppleID:       account.Email,
			Region:        "global",
			Authenticated: true,
		},
		domain.Alias{
			AccountID:    account.ID,
			Address:      "legacy-pending@privaterelay.appleid.com",
			Label:        "自动创建",
			APIKeyHash:   secure.HashToken("legacy-pending-key"),
			APIKeyPrefix: "icm_pending",
			Enabled:      false,
		},
		"ak1.legacy-pending-test",
	)
	if err != nil {
		t.Fatalf("create pending alias: %v", err)
	}

	accountPath := fmt.Sprintf("/admin/accounts/%d", account.ID)
	response := env.request(t, http.MethodGet, accountPath, nil, []*http.Cookie{session})
	if response.Code != http.StatusOK {
		t.Fatalf("GET pending alias page status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	pendingRow := aliasStatusTableRow(t, response.Body.String(), pending.Address)
	assertAliasStatusFragment(t, pendingRow, `status-pending">等待目录确认</span>`)
	assertAliasStatusFragment(t, pendingRow, "确认前不可操作")
	for _, forbidden := range []string{"data-sync-status-cell", "/rotate", "/toggle", "/delete"} {
		if strings.Contains(pendingRow, forbidden) {
			t.Fatalf("pending alias row exposed %q: %s", forbidden, pendingRow)
		}
	}

	form := url.Values{"csrf_token": {testSessionCSRF}}
	for name, suffix := range map[string]string{
		"rotate key": "/rotate",
		"enable":     "/toggle",
		"delete":     "/delete",
	} {
		t.Run(name, func(t *testing.T) {
			response := env.request(t, http.MethodPost, fmt.Sprintf("/admin/aliases/%d%s", pending.ID, suffix), form, []*http.Cookie{session})
			if response.Code != http.StatusConflict {
				t.Fatalf("pending alias mutation status = %d, want %d; body=%s", response.Code, http.StatusConflict, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "正在等待 Apple 目录确认，暂时不能轮换 Key、启用或删除") {
				t.Fatalf("pending alias conflict omitted actionable message: %s", response.Body.String())
			}
		})
	}

	unchanged, err := env.store.GetAlias(context.Background(), pending.ID)
	if err != nil {
		t.Fatalf("reload pending alias: %v", err)
	}
	if unchanged.Enabled || unchanged.LastSyncError != domain.AppleAliasConfirmationPending || string(unchanged.APIKeyHash) != string(pending.APIKeyHash) {
		t.Fatalf("pending alias changed after rejected legacy mutations: before=%#v after=%#v", pending, unchanged)
	}
}

func aliasStatusTableRow(t *testing.T, body, marker string) string {
	t.Helper()
	markerIndex := strings.Index(body, marker)
	if markerIndex < 0 {
		t.Fatalf("response does not contain alias %q", marker)
	}
	start := strings.LastIndex(body[:markerIndex], "<tr")
	endOffset := strings.Index(body[markerIndex:], "</tr>")
	if start < 0 || endOffset < 0 {
		t.Fatalf("response does not contain a complete table row for alias %q", marker)
	}
	return body[start : markerIndex+endOffset+len("</tr>")]
}

func assertAliasStatusFragment(t *testing.T, row, fragment string) {
	t.Helper()
	if !strings.Contains(row, fragment) {
		t.Fatalf("alias row is missing %q: %s", fragment, row)
	}
}

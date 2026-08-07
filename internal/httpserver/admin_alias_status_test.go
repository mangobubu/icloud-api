package httpserver

import (
	"context"
	"fmt"
	"net/http"
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

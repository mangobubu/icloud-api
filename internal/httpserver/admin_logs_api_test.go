package httpserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/applog"
)

type stubApplicationLogSource struct {
	calls  int
	filter applog.Filter
	page   applog.Page
}

func (s *stubApplicationLogSource) List(filter applog.Filter) applog.Page {
	s.calls++
	s.filter = filter
	return s.page
}

func TestAdminAPIApplicationLogsFiltersAndMapsPage(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, _, _ := env.createSession(t, "application-logs-admin", "unused-password")
	loggedAt := time.Date(2026, time.August, 9, 10, 11, 12, 345678901, time.UTC)
	source := &stubApplicationLogSource{page: applog.Page{
		Items: []applog.Entry{
			{
				ID:      99,
				Time:    loggedAt,
				Level:   slog.LevelError,
				Message: "同步主号失败",
				Source:  "manager.go:371",
				Fields: map[string]string{
					"account_id": "42",
					"request_id": "request-123",
					"error":      "IMAP connection closed",
				},
			},
		},
		HasMore:      true,
		NextBeforeID: 99,
	}}
	env.server.SetApplicationLogSource(source)

	response := env.request(
		t,
		http.MethodGet,
		"/admin/api/v1/logs?level=ERROR&query=%20imap%20&keyword=ignored&account_id=42&before_id=100&limit=20",
		nil,
		"",
		[]*http.Cookie{sessionCookie},
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("application logs status = %d; body=%s", response.Code, response.Body.String())
	}
	if source.calls != 1 {
		t.Fatalf("application log source calls = %d, want 1", source.calls)
	}
	if source.filter.Level != "error" || source.filter.Query != "imap" || source.filter.BeforeID != 100 || source.filter.Limit != 20 {
		t.Fatalf("application log filter = %#v", source.filter)
	}
	if source.filter.AccountID == nil || *source.filter.AccountID != 42 {
		t.Fatalf("application log account filter = %#v, want 42", source.filter.AccountID)
	}

	var payload struct {
		Data struct {
			Items        []adminAPIApplicationLogDTO `json:"items"`
			HasMore      bool                        `json:"has_more"`
			NextBeforeID uint64                      `json:"next_before_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode application logs: %v; body=%s", err, response.Body.String())
	}
	if !payload.Data.HasMore || payload.Data.NextBeforeID != 99 || len(payload.Data.Items) != 1 {
		t.Fatalf("application log page = %#v", payload.Data)
	}
	item := payload.Data.Items[0]
	if item.ID != 99 || item.CreatedAt != loggedAt.Format(time.RFC3339Nano) || item.Level != "error" || item.Message != "同步主号失败" || item.Source != "manager.go:371" {
		t.Fatalf("application log item = %#v", item)
	}
	if item.AccountID == nil || *item.AccountID != 42 || item.RequestID != "request-123" {
		t.Fatalf("promoted application log attributes = account:%#v request:%q", item.AccountID, item.RequestID)
	}
	if want := source.page.Items[0].Fields; !reflect.DeepEqual(item.Attributes, want) {
		t.Fatalf("application log attributes = %#v, want %#v", item.Attributes, want)
	}
}

func TestAdminAPIApplicationLogsKeywordCompatibilityAndEmptySource(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, _, _ := env.createSession(t, "legacy-log-query-admin", "unused-password")
	source := &stubApplicationLogSource{page: applog.Page{Items: []applog.Entry{}}}
	env.server.SetApplicationLogSource(source)

	response := env.request(t, http.MethodGet, "/admin/api/v1/logs?keyword=%20legacy%20", nil, "", []*http.Cookie{sessionCookie}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("legacy application log query status = %d; body=%s", response.Code, response.Body.String())
	}
	if source.filter.Query != "legacy" || source.filter.Limit != adminAPIDefaultLogLimit {
		t.Fatalf("legacy application log filter = %#v", source.filter)
	}

	env.server.SetApplicationLogSource(nil)
	response = env.request(t, http.MethodGet, "/admin/api/v1/logs", nil, "", []*http.Cookie{sessionCookie}, "")
	if response.Code != http.StatusOK || response.Body.String() != `{"data":{"has_more":false,"items":[],"next_before_id":0}}` {
		t.Fatalf("unconfigured application logs response = %d; body=%s", response.Code, response.Body.String())
	}
}

func TestAdminAPIApplicationLogsRequireSession(t *testing.T) {
	env := newAdminAPITestEnv(t)
	source := &stubApplicationLogSource{}
	env.server.SetApplicationLogSource(source)

	response := env.request(t, http.MethodGet, "/admin/api/v1/logs", nil, "", nil, "")
	if response.Code != http.StatusUnauthorized || adminAPITestErrorCode(t, response) != "AUTH_REQUIRED" {
		t.Fatalf("unauthenticated application logs response = %d; body=%s", response.Code, response.Body.String())
	}
	if source.calls != 0 {
		t.Fatalf("unauthenticated request reached application log source %d times", source.calls)
	}
}

func TestAdminAPIApplicationLogsRejectInvalidFilters(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, _, _ := env.createSession(t, "invalid-log-filter-admin", "unused-password")
	source := &stubApplicationLogSource{}
	env.server.SetApplicationLogSource(source)

	for _, query := range []string{
		"level=trace",
		"account_id=0",
		"account_id=not-an-id",
		"before_id=-1",
		"before_id=0",
		"before_id=not-an-id",
		"limit=0",
		"limit=201",
	} {
		response := env.request(t, http.MethodGet, "/admin/api/v1/logs?"+query, nil, "", []*http.Cookie{sessionCookie}, "")
		if response.Code != http.StatusBadRequest || adminAPITestErrorCode(t, response) != "VALIDATION_FAILED" {
			t.Fatalf("invalid filter %q response = %d; body=%s", query, response.Code, response.Body.String())
		}
	}
	if source.calls != 0 {
		t.Fatalf("invalid filters reached application log source %d times", source.calls)
	}
}

func TestAdminAPIApplicationLogsValidateUnicodeQueryLength(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, _, _ := env.createSession(t, "unicode-log-query-admin", "unused-password")
	source := &stubApplicationLogSource{page: applog.Page{Items: []applog.Entry{}}}
	env.server.SetApplicationLogSource(source)

	accepted := strings.Repeat("邮", adminAPIMaxLogQueryRunes)
	response := env.request(t, http.MethodGet, adminAPIApplicationLogsPath+"?query="+url.QueryEscape(accepted), nil, "", []*http.Cookie{sessionCookie}, "")
	if response.Code != http.StatusOK || source.filter.Query != accepted {
		t.Fatalf("200-rune query response = %d filter=%q; body=%s", response.Code, source.filter.Query, response.Body.String())
	}

	rejected := strings.Repeat("邮", adminAPIMaxLogQueryRunes+1)
	response = env.request(t, http.MethodGet, adminAPIApplicationLogsPath+"?query="+url.QueryEscape(rejected), nil, "", []*http.Cookie{sessionCookie}, "")
	if response.Code != http.StatusBadRequest || adminAPITestErrorCode(t, response) != "VALIDATION_FAILED" {
		t.Fatalf("201-rune query response = %d; body=%s", response.Code, response.Body.String())
	}
	if source.calls != 1 {
		t.Fatalf("overlong query reached application log source; calls=%d", source.calls)
	}
}

func TestAdminAPIApplicationLogPromotesGroupedIdentifiers(t *testing.T) {
	item := adminAPIApplicationLogFromEntry(applog.Entry{Fields: map[string]string{
		"sync.account_id":   "42",
		"http.request_id":   "grouped-request",
		"worker.request_id": "later-request",
	}})
	if item.AccountID == nil || *item.AccountID != 42 || item.RequestID != "grouped-request" {
		t.Fatalf("grouped identifiers = account:%#v request:%q", item.AccountID, item.RequestID)
	}

	item = adminAPIApplicationLogFromEntry(applog.Entry{Fields: map[string]string{
		"account_id":      "7",
		"sync.account_id": "42",
		"request_id":      "exact-request",
		"http.request_id": "grouped-request",
	}})
	if item.AccountID == nil || *item.AccountID != 7 || item.RequestID != "exact-request" {
		t.Fatalf("exact identifiers did not take precedence = account:%#v request:%q", item.AccountID, item.RequestID)
	}
}

func TestAdminAPIApplicationLogsDoNotRecordSuccessfulPollingRequests(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, _, _ := env.createSession(t, "polling-log-admin", "unused-password")
	captured := applog.New(10)
	env.server.logger = slog.New(captured)
	env.server.SetApplicationLogSource(captured)

	response := env.request(t, http.MethodGet, adminAPIApplicationLogsPath, nil, "", []*http.Cookie{sessionCookie}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("application log poll status = %d; body=%s", response.Code, response.Body.String())
	}
	if page := captured.List(applog.Filter{Limit: 10}); len(page.Items) != 0 {
		t.Fatalf("application log poll recorded itself: %#v", page.Items)
	}

	response = env.request(t, http.MethodGet, adminAPIApplicationLogsPath+"?level=trace", nil, "", []*http.Cookie{sessionCookie}, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid application log poll status = %d; body=%s", response.Code, response.Body.String())
	}
	response = env.request(t, http.MethodGet, "/admin/api/v1/audit", nil, "", []*http.Cookie{sessionCookie}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("audit request status = %d; body=%s", response.Code, response.Body.String())
	}
	response = env.request(t, http.MethodPost, adminAPIApplicationLogsPath, nil, "", nil, "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("POST application log path status = %d, want 404", response.Code)
	}

	page := captured.List(applog.Filter{Limit: 10})
	if len(page.Items) != 3 {
		t.Fatalf("non-successful-polling request logs = %#v, want 3 entries", page.Items)
	}
	if page.Items[0].Fields["method"] != http.MethodPost || page.Items[0].Fields["path"] != adminAPIApplicationLogsPath {
		t.Fatalf("same-path POST log = %#v", page.Items[0])
	}
	if page.Items[1].Fields["method"] != http.MethodGet || page.Items[1].Fields["path"] != "/admin/api/v1/audit" {
		t.Fatalf("other-path GET log = %#v", page.Items[1])
	}
	if page.Items[2].Fields["method"] != http.MethodGet || page.Items[2].Fields["path"] != adminAPIApplicationLogsPath || page.Items[2].Fields["status"] != "400" {
		t.Fatalf("failed application log poll = %#v", page.Items[2])
	}
}

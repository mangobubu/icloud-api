package httpserver

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/applog"
)

const (
	adminAPIApplicationLogsPath = "/admin/api/v1/logs"
	adminAPIDefaultLogLimit     = 100
	adminAPIMaxLogLimit         = 200
	adminAPIMaxLogQueryRunes    = 200
)

// ApplicationLogSource supplies the bounded in-memory application log history.
// Implementations must return entries newest first and apply BeforeID
// exclusively so clients can use NextBeforeID as the next page cursor.
type ApplicationLogSource interface {
	List(applog.Filter) applog.Page
}

// SetApplicationLogSource exposes application logs to authenticated
// administrators. It should be configured before Router starts serving.
func (s *Server) SetApplicationLogSource(source ApplicationLogSource) {
	s.applicationLogs = source
}

type adminAPIApplicationLogDTO struct {
	ID         uint64            `json:"id"`
	CreatedAt  string            `json:"created_at"`
	Level      string            `json:"level"`
	Message    string            `json:"message"`
	Source     string            `json:"source"`
	AccountID  *int64            `json:"account_id,omitempty"`
	RequestID  string            `json:"request_id,omitempty"`
	Attributes map[string]string `json:"attributes"`
}

func (s *Server) adminAPIListApplicationLogs(c *gin.Context) {
	filter, ok := adminAPIApplicationLogFilter(c)
	if !ok {
		return
	}

	page := applog.Page{Items: []applog.Entry{}}
	if s.applicationLogs != nil {
		page = s.applicationLogs.List(filter)
	}

	items := make([]adminAPIApplicationLogDTO, 0, len(page.Items))
	for _, entry := range page.Items {
		items = append(items, adminAPIApplicationLogFromEntry(entry))
	}
	writeAdminAPIData(c, http.StatusOK, gin.H{
		"items":          items,
		"has_more":       page.HasMore,
		"next_before_id": page.NextBeforeID,
	})
}

func adminAPIApplicationLogFilter(c *gin.Context) (applog.Filter, bool) {
	level := strings.ToLower(strings.TrimSpace(c.Query("level")))
	switch level {
	case "", "debug", "info", "warn", "error":
	default:
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "level 参数无效")
		return applog.Filter{}, false
	}

	query := strings.TrimSpace(c.Query("query"))
	if query == "" {
		query = strings.TrimSpace(c.Query("keyword"))
	}
	if len([]rune(query)) > adminAPIMaxLogQueryRunes {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "query 参数不能超过 200 个字符")
		return applog.Filter{}, false
	}

	accountID, ok := adminAPIOptionalPositiveInt64(c, "account_id")
	if !ok {
		return applog.Filter{}, false
	}
	beforeID, ok := adminAPIOptionalUint64(c, "before_id")
	if !ok {
		return applog.Filter{}, false
	}
	limit, ok := adminAPIQueryInt(c, "limit", adminAPIDefaultLogLimit, 1, adminAPIMaxLogLimit)
	if !ok {
		return applog.Filter{}, false
	}

	return applog.Filter{
		Level:     level,
		Query:     query,
		AccountID: accountID,
		BeforeID:  beforeID,
		Limit:     limit,
	}, true
}

func adminAPIOptionalPositiveInt64(c *gin.Context, name string) (*int64, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 1 {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", name+" 参数无效")
		return nil, false
	}
	return &value, true
}

func adminAPIOptionalUint64(c *gin.Context, name string) (uint64, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return 0, true
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value < 1 {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", name+" 参数无效")
		return 0, false
	}
	return value, true
}

func adminAPIApplicationLogFromEntry(entry applog.Entry) adminAPIApplicationLogDTO {
	attributes := make(map[string]string, len(entry.Fields))
	for key, value := range entry.Fields {
		attributes[key] = value
	}
	var accountID *int64
	if value, err := strconv.ParseInt(strings.TrimSpace(adminAPIApplicationLogAttribute(entry.Fields, "account_id")), 10, 64); err == nil && value > 0 {
		accountID = &value
	}
	createdAt := ""
	if !entry.Time.IsZero() {
		createdAt = entry.Time.UTC().Format(time.RFC3339Nano)
	}
	return adminAPIApplicationLogDTO{
		ID:         entry.ID,
		CreatedAt:  createdAt,
		Level:      strings.ToLower(entry.Level.String()),
		Message:    entry.Message,
		Source:     entry.Source,
		AccountID:  accountID,
		RequestID:  adminAPIApplicationLogAttribute(entry.Fields, "request_id"),
		Attributes: attributes,
	}
}

func adminAPIApplicationLogAttribute(fields map[string]string, name string) string {
	if value, ok := fields[name]; ok {
		return value
	}
	matchedKey := ""
	suffix := "." + name
	for key := range fields {
		if strings.HasSuffix(key, suffix) && (matchedKey == "" || key < matchedKey) {
			matchedKey = key
		}
	}
	return fields[matchedKey]
}

package httpserver

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

const (
	requestIDKey    = "request_id"
	sessionKey      = "admin_session"
	bindingKey      = "mailbox_binding"
	sessionCookie   = "icloud_admin_session"
	loginCSRFCookie = "icloud_login_csrf"
)

type limiterEntry struct {
	count int
	reset time.Time
}

type windowLimiter struct {
	mu          sync.Mutex
	limit       int
	window      time.Duration
	maxItems    int
	nextCleanup time.Time
	items       map[string]limiterEntry
}

func newWindowLimiter(limit int, window time.Duration) *windowLimiter {
	return &windowLimiter{limit: limit, window: window, maxItems: 8192, items: make(map[string]limiterEntry)}
}

func (l *windowLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nextCleanup.IsZero() || !now.Before(l.nextCleanup) {
		for itemKey, item := range l.items {
			if !item.reset.After(now) {
				delete(l.items, itemKey)
			}
		}
		cleanupInterval := min(l.window, time.Minute)
		if cleanupInterval <= 0 {
			cleanupInterval = time.Minute
		}
		l.nextCleanup = now.Add(cleanupInterval)
	}
	entry := l.items[key]
	if _, exists := l.items[key]; !exists && len(l.items) >= l.maxItems {
		return false
	}
	if entry.reset.Before(now) {
		entry = limiterEntry{reset: now.Add(l.window)}
	}
	entry.count++
	l.items[key] = entry
	return entry.count <= l.limit
}

func (s *Server) requestContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID, err := secure.RandomToken(12)
		if err != nil {
			requestID = "request-id-unavailable"
		}
		c.Set(requestIDKey, requestID)
		c.Header("X-Request-ID", requestID)
		started := time.Now()
		c.Next()
		s.logger.Info("HTTP 请求", "method", c.Request.Method, "path", c.Request.URL.Path, "status", c.Writer.Status(), "duration_ms", time.Since(started).Milliseconds(), "request_id", requestID)
	}
}

func (s *Server) securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/admin") {
			c.Header("Cache-Control", "no-store, private")
			c.Header("Pragma", "no-cache")
		}
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		c.Next()
	}
}

func (s *Server) loginRequestGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.loginRequestLimiter.Allow(c.ClientIP()) {
			c.String(http.StatusTooManyRequests, "登录请求过于频繁，请稍后再试。请求编号：%s", requestID(c))
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Server) parseAdminForm() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
			c.Next()
			return
		}
		const maxFormBytes = 64 << 10
		if c.Request.ContentLength > maxFormBytes {
			c.String(http.StatusRequestEntityTooLarge, "表单内容过大。请求编号：%s", requestID(c))
			c.Abort()
			return
		}
		contentType := strings.ToLower(c.GetHeader("Content-Type"))
		if !strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
			c.String(http.StatusUnsupportedMediaType, "表单格式不受支持。请求编号：%s", requestID(c))
			c.Abort()
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxFormBytes)
		if err := c.Request.ParseForm(); err != nil {
			c.String(http.StatusRequestEntityTooLarge, "表单内容过大或格式错误。请求编号：%s", requestID(c))
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Server) adminAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := c.Cookie(sessionCookie)
		if err != nil || raw == "" {
			c.Redirect(http.StatusFound, "/admin/login")
			c.Abort()
			return
		}
		session, err := s.store.GetSessionByHash(c.Request.Context(), secure.HashToken(raw))
		if err != nil {
			s.clearSessionCookie(c)
			c.Redirect(http.StatusFound, "/admin/login?notice=session_expired")
			c.Abort()
			return
		}
		c.Set(sessionKey, session)
		c.Next()
	}
}

func (s *Server) csrfGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		session := mustSession(c)
		provided := c.PostForm("csrf_token")
		if len(provided) != len(session.CSRF) || subtle.ConstantTimeCompare([]byte(provided), []byte(session.CSRF)) != 1 || !sameOrigin(c.Request) {
			c.String(http.StatusForbidden, "请求校验失败，请刷新页面后重试。请求编号：%s", requestID(c))
			c.Abort()
			return
		}
		c.Next()
	}
}

func sameOrigin(r *http.Request) bool {
	return originFailureReason(r) == ""
}

func originFailureReason(r *http.Request) string {
	if fetchSite := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); strings.EqualFold(fetchSite, "cross-site") {
		return loginCSRFFetchSiteCrossSite
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return ""
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" ||
		(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) ||
		parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return loginCSRFOriginInvalid
	}
	if !strings.EqualFold(parsed.Host, r.Host) {
		return loginCSRFOriginHostMismatch
	}
	return ""
}

func validAPIKey(token string) bool {
	const (
		prefix       = "icm_"
		secretLength = 43
	)
	if len(token) != len(prefix)+secretLength || !strings.HasPrefix(token, prefix) {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token[len(prefix):])
	return err == nil && len(decoded) == 32
}

func (s *Server) apiKeyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if !s.apiIPLimiter.Allow(c.ClientIP()) {
			s.writeAPIError(c, http.StatusTooManyRequests, "RATE_LIMITED", "请求过于频繁")
			c.Abort()
			return
		}
		authorization := c.GetHeader("Authorization")
		scheme, token, ok := strings.Cut(authorization, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || !validAPIKey(token) {
			s.writeAPIError(c, http.StatusUnauthorized, "INVALID_API_KEY", "API Key 无效")
			c.Abort()
			return
		}
		binding, err := s.store.GetMailboxBindingByAPIKeyHash(c.Request.Context(), secure.HashToken(token))
		if errors.Is(err, store.ErrNotFound) || err == nil && (!binding.Alias.Enabled || !binding.Account.Enabled) {
			s.writeAPIError(c, http.StatusUnauthorized, "INVALID_API_KEY", "API Key 无效")
			c.Abort()
			return
		}
		if err != nil {
			s.logger.Error("查询 API Key 绑定失败", "error", err, "request_id", requestID(c))
			s.writeAPIError(c, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "数据库暂不可用")
			c.Abort()
			return
		}
		if !s.apiLimiter.Allow(string(binding.Alias.APIKeyHash)) {
			s.writeAPIError(c, http.StatusTooManyRequests, "RATE_LIMITED", "请求过于频繁")
			c.Abort()
			return
		}
		c.Set(bindingKey, binding)
		c.Next()
	}
}

func (s *Server) pageData(c *gin.Context, title, active string) PageData {
	session := mustSession(c)
	flash, kind := notice(c.Query("notice"))
	return PageData{Title: title, Active: active, AdminUsername: session.Username, CSRF: session.CSRF, Flash: flash, FlashKind: kind, RequestID: requestID(c)}
}

func mustSession(c *gin.Context) domain.Session {
	value, _ := c.Get(sessionKey)
	session, _ := value.(domain.Session)
	return session
}

func mustBinding(c *gin.Context) domain.MailboxBinding {
	value, _ := c.Get(bindingKey)
	binding, _ := value.(domain.MailboxBinding)
	return binding
}

func requestID(c *gin.Context) string {
	value, _ := c.Get(requestIDKey)
	requestID, _ := value.(string)
	return requestID
}

func (s *Server) writeAPIError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message, "request_id": requestID(c)}})
}

func notice(code string) (string, string) {
	messages := map[string][2]string{
		"session_expired":  {"会话已过期，请重新登录。", "warning"},
		"password_changed": {"密码已更新，请重新登录。", "success"},
		"account_created":  {"主号已添加，后台会在下一轮开始同步。", "success"},
		"account_updated":  {"主号配置已更新。", "success"},
		"account_deleted":  {"主号及其关联数据已删除。", "success"},
		"sync_ok":          {"同步完成。", "success"},
		"sync_error":       {"同步失败，请检查连接状态。", "error"},
		"alias_updated":    {"隐私邮箱状态已更新。", "success"},
		"alias_deleted":    {"隐私邮箱已删除。", "success"},
	}
	if message, ok := messages[code]; ok {
		return message[0], message[1]
	}
	return "", ""
}

var _ = store.ErrNotFound

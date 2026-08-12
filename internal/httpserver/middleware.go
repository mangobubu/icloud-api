package httpserver

import (
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
	requestIDKey  = "request_id"
	sessionKey    = "admin_session"
	bindingKey    = "mailbox_binding"
	sessionCookie = "icloud_admin_session"
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
		if c.Request.Method == http.MethodGet &&
			c.Request.URL.Path == adminAPIApplicationLogsPath &&
			c.Writer.Status() >= http.StatusOK && c.Writer.Status() < http.StatusMultipleChoices {
			return
		}
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
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
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
	return s.apiKeyAuthWithToken(func(c *gin.Context) (string, bool) {
		authorization := c.GetHeader("Authorization")
		scheme, token, ok := strings.Cut(authorization, " ")
		return token, ok && strings.EqualFold(scheme, "Bearer")
	}, false)
}

func (s *Server) apiKeyQueryAuth() gin.HandlerFunc {
	return s.apiKeyAuthWithToken(func(c *gin.Context) (string, bool) {
		values, err := url.ParseQuery(c.Request.URL.RawQuery)
		if err != nil {
			return "", false
		}
		apiKeys, ok := values["api_key"]
		if !ok || len(apiKeys) != 1 {
			return "", false
		}
		return apiKeys[0], true
	}, true)
}

func (s *Server) apiKeyAuthWithToken(
	tokenFromRequest func(*gin.Context) (string, bool),
	allowDirectLinkToken bool,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if !s.apiIPLimiter.Allow(c.ClientIP()) {
			s.writeAPIError(c, http.StatusTooManyRequests, "RATE_LIMITED", "请求过于频繁")
			c.Abort()
			return
		}
		token, ok := tokenFromRequest(c)
		if !ok || !validAPIKey(token) {
			s.writeAPIError(c, http.StatusUnauthorized, "INVALID_API_KEY", "API Key 无效")
			c.Abort()
			return
		}
		binding, err := s.mailboxBindingForToken(c, token, allowDirectLinkToken)
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

func (s *Server) mailboxBindingForToken(
	c *gin.Context,
	token string,
	allowDirectLinkToken bool,
) (domain.MailboxBinding, error) {
	if allowDirectLinkToken && s.cipher != nil {
		if aliasID, candidate := secure.DirectLinkTokenAliasID(token); candidate {
			alias, err := s.store.GetAlias(c.Request.Context(), aliasID)
			switch {
			case err == nil && s.cipher.VerifyDirectLinkToken(token, aliasID, alias.APIKeyHash):
				return s.store.GetMailboxBindingByAPIKeyHash(c.Request.Context(), alias.APIKeyHash)
			case err != nil && !errors.Is(err, store.ErrNotFound):
				return domain.MailboxBinding{}, err
			}
		}
	}
	return s.store.GetMailboxBindingByAPIKeyHash(c.Request.Context(), secure.HashToken(token))
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

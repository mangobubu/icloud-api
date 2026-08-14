package httpserver

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

type otpResponse struct {
	OTP  string `json:"otp"`
	Time string `json:"time"`
}

// otpHistory is deliberately a repeatable cache read. Neither authentication
// form consumes a code or changes upstream/local message flags.
func (s *Server) otpHistory(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !s.apiIPLimiter.Allow(c.ClientIP()) {
		s.writeAPIError(c, http.StatusTooManyRequests, "RATE_LIMITED", "请求过于频繁")
		return
	}
	binding, ok := s.authenticateOTPRequest(c)
	if !ok {
		return
	}
	// OTP history is part of the v2 contract. Legacy aliases must continue to
	// use the original mailbox endpoints even if a stale credential bundle or
	// hash happens to remain on the row.
	if binding.Alias.CredentialMode != domain.AliasCredentialModeV2 ||
		!binding.Alias.Enabled || !binding.Account.Enabled {
		s.writeAPIError(c, http.StatusUnauthorized, "INVALID_API_KEY", "API Key 无效")
		return
	}
	if !s.apiLimiter.Allow(string(binding.Alias.APIKeyHash)) {
		s.writeAPIError(c, http.StatusTooManyRequests, "RATE_LIMITED", "请求过于频繁")
		return
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	s.requestMailboxSync(binding.Account.ID, now)
	if err := s.store.TouchAliasAccess(c.Request.Context(), binding.Alias.ID, now); err != nil {
		s.logger.Warn("更新 API 最近访问时间失败", "alias_id", binding.Alias.ID, "error", err, "request_id", requestID(c))
	}
	records, err := s.store.ListAliasOTPs(c.Request.Context(), binding.Alias.ID, 100)
	if err != nil {
		s.logger.Error("读取验证码归档失败", "alias_id", binding.Alias.ID, "error", err, "request_id", requestID(c))
		s.writeAPIError(c, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "数据库暂不可用")
		return
	}
	location := s.cfg.Timezone
	if location == nil {
		location = time.Local
	}
	result := make([]otpResponse, 0, len(records))
	for _, record := range records {
		result = append(result, otpResponse{OTP: record.OTP, Time: record.Time.In(location).Format(time.RFC3339)})
	}
	c.JSON(http.StatusOK, result)
}

func (s *Server) authenticateOTPRequest(c *gin.Context) (domain.MailboxBinding, bool) {
	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	values, queryErr := url.ParseQuery(c.Request.URL.RawQuery)
	queryTokens := values["token"]
	queryPresent := len(queryTokens) > 0
	if queryErr != nil || len(values) > 1 || len(queryTokens) > 1 || (authorization != "" && queryPresent) {
		s.writeAPIError(c, http.StatusUnauthorized, "INVALID_API_KEY", "API Key 无效")
		return domain.MailboxBinding{}, false
	}

	var binding domain.MailboxBinding
	var err error
	if authorization != "" {
		scheme, token, found := strings.Cut(authorization, " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || !validAPIKey(token) || strings.Contains(token, " ") {
			s.writeAPIError(c, http.StatusUnauthorized, "INVALID_API_KEY", "API Key 无效")
			return domain.MailboxBinding{}, false
		}
		binding, err = s.store.GetMailboxBindingByAPIKeyHash(c.Request.Context(), secure.HashToken(token))
	} else if len(queryTokens) == 1 && validAPIKey(queryTokens[0]) {
		token := queryTokens[0]
		aliasID, candidate := secure.OTPTokenAliasID(token)
		if !candidate {
			err = store.ErrNotFound
		} else {
			alias, lookupErr := s.store.GetAlias(c.Request.Context(), aliasID)
			switch {
			case lookupErr != nil:
				err = lookupErr
			case !s.cipher.VerifyOTPToken(token, aliasID, alias.APIKeyHash):
				err = store.ErrNotFound
			default:
				binding, err = s.store.GetMailboxBindingByAPIKeyHash(c.Request.Context(), alias.APIKeyHash)
			}
		}
	} else {
		err = store.ErrNotFound
	}
	if errors.Is(err, store.ErrNotFound) {
		s.writeAPIError(c, http.StatusUnauthorized, "INVALID_API_KEY", "API Key 无效")
		return domain.MailboxBinding{}, false
	}
	if err != nil {
		s.logger.Error("查询验证码凭证绑定失败", "error", err, "request_id", requestID(c))
		s.writeAPIError(c, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "数据库暂不可用")
		return domain.MailboxBinding{}, false
	}
	return binding, true
}

func (s *Server) issueIMAPAccessToken(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	if !s.externalAPILimiter.Allow(c.ClientIP()) {
		writeOAuthError(c, http.StatusTooManyRequests, "temporarily_unavailable", "请求过于频繁")
		return
	}
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" || c.Request.URL.RawQuery != "" {
		writeOAuthError(c, http.StatusBadRequest, "invalid_request", "请求必须使用表单编码")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	if err := c.Request.ParseForm(); err != nil {
		writeOAuthError(c, http.StatusBadRequest, "invalid_request", "请求表单无效")
		return
	}
	clientIDs := c.Request.PostForm["client_id"]
	refreshTokens := c.Request.PostForm["refresh_token"]
	grantTypes := c.Request.PostForm["grant_type"]
	if len(clientIDs) != 1 || len(refreshTokens) != 1 || len(grantTypes) != 1 ||
		grantTypes[0] != "refresh_token" || len(c.Request.PostForm) != 3 {
		writeOAuthError(c, http.StatusBadRequest, "invalid_request", "client_id、refresh_token 和 grant_type 必须各出现一次")
		return
	}
	clientID := strings.TrimSpace(clientIDs[0])
	refreshToken := refreshTokens[0]
	if clientID == "" || refreshToken == "" || strings.TrimSpace(refreshToken) != refreshToken ||
		strings.ContainsAny(clientID+refreshToken, " \t\r\n") {
		writeOAuthError(c, http.StatusUnauthorized, "invalid_client", "客户端凭证无效")
		return
	}
	binding, err := s.store.GetMailboxBindingByOAuthClientID(c.Request.Context(), clientID)
	if errors.Is(err, store.ErrNotFound) || err == nil &&
		(binding.Alias.CredentialMode != domain.AliasCredentialModeV2 ||
			!binding.Alias.Enabled || !binding.Account.Enabled ||
			!secure.HashEqual(binding.Alias.RefreshTokenHash, secure.HashToken(refreshToken))) {
		writeOAuthError(c, http.StatusUnauthorized, "invalid_client", "客户端凭证无效")
		return
	}
	if err != nil {
		s.logger.Error("查询令牌凭证失败", "error", err, "request_id", requestID(c))
		writeOAuthError(c, http.StatusServiceUnavailable, "temporarily_unavailable", "令牌服务暂不可用")
		return
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	const lifetime = time.Hour
	accessToken, err := s.cipher.IssueAliasAccessToken(
		binding.Alias.ID,
		binding.Alias.CredentialVersion,
		binding.Alias.RefreshTokenHash,
		now.Add(lifetime),
	)
	if err != nil {
		s.logger.Error("签发 IMAPS 访问令牌失败", "error", err, "request_id", requestID(c))
		writeOAuthError(c, http.StatusServiceUnavailable, "temporarily_unavailable", "令牌服务暂不可用")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   int(lifetime.Seconds()),
	})
}

func writeOAuthError(c *gin.Context, status int, code, description string) {
	c.JSON(status, gin.H{"error": code, "error_description": description})
}

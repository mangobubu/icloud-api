package httpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
	"icloud-api/internal/syncer"
)

const (
	adminAPILoginCSRFCookie   = "icloud_api_login_csrf"
	adminAPICSRFHeader        = "X-CSRF-Token"
	adminAPIMaxJSONBytes      = 64 << 10
	adminAPIDefaultPageLimit  = 20
	adminAPIMaxPageLimit      = 1000
	adminAPIMaxPageOffset     = 1_000_000
	adminAPIMaxListQueryRunes = 200
)

const legacyAdminAPIBasePath = "/admin/api/v1"

// registerAdminAPIRoutes attaches the JSON admin API to either the
// installation-specific management path or the fixed compatibility path.
// Authentication, request limits, and CSRF checks apply to both entry points.
func (s *Server) registerAdminAPIRoutes(api *gin.RouterGroup) {
	basePath := api.BasePath()
	auth := api.Group("/auth")
	auth.GET("/csrf", s.adminAPILoginCSRF(basePath))
	auth.POST("/login", s.adminAPILoginRequestGate(), s.adminAPILogin(basePath))

	protected := api.Group("")
	protected.Use(s.adminAPIAuth(), s.adminAPICSRF())
	protected.GET("/auth/session", s.adminAPISession)
	protected.POST("/auth/logout", s.adminAPILogout(basePath))
	protected.PUT("/auth/password", s.adminAPIChangePassword(basePath))

	protected.GET("/accounts", s.adminAPIListAccounts)
	protected.POST("/accounts", s.adminAPICreateAccount(basePath))
	protected.GET("/accounts/:id", s.adminAPIGetAccount)
	protected.PUT("/accounts/:id", s.adminAPIUpdateAccount)
	protected.DELETE("/accounts/:id", s.adminAPIDeleteAccount)
	protected.POST("/accounts/:id/sync", s.adminAPISyncAccount)
	protected.POST("/accounts/:id/apple-auth", s.adminAPIStartAppleAuth)
	protected.POST("/accounts/:id/apple-auth/verify", s.adminAPIVerifyAppleAuth)
	protected.DELETE("/accounts/:id/apple-auth", s.adminAPIClearAppleAuth)
	protected.PUT("/accounts/:id/aliases/auto-create", s.adminAPISetAliasAutoCreation)
	protected.GET("/accounts/:id/aliases/auto-create/keys", s.adminAPIGetAliasAutoCreationKeys)
	protected.DELETE("/accounts/:id/aliases/auto-create/keys", s.adminAPIAcknowledgeAliasAutoCreationKeys)
	protected.POST("/accounts/:id/aliases", s.adminAPICreateAlias(basePath))
	protected.POST("/accounts/:id/aliases/sync", s.adminAPISyncAppleAliases)

	protected.GET("/aliases", s.adminAPIListAliases)
	protected.GET("/aliases/:id", s.adminAPIGetAlias)
	protected.POST("/aliases/:id/rotate-key", s.adminAPIRotateAliasKey)
	protected.POST("/aliases/:id/rotate-credentials", s.adminAPIRotateAliasCredentials)
	protected.PATCH("/aliases/:id", s.adminAPIUpdateAlias)
	protected.DELETE("/aliases/:id", s.adminAPIDeleteAlias)

	protected.GET("/audit", s.adminAPIListAuditLogs)
	protected.GET("/logs", s.adminAPIListApplicationLogs)
}

type adminAPIAdminDTO struct {
	Username string `json:"username"`
}

type adminAPISessionDTO struct {
	Admin     adminAPIAdminDTO `json:"admin"`
	CSRFToken string           `json:"csrf_token"`
	ExpiresAt string           `json:"expires_at"`
}

type adminAPIAccountDTO struct {
	ID               int64                    `json:"id"`
	Name             string                   `json:"name"`
	Email            string                   `json:"email"`
	IMAPHost         string                   `json:"imap_host"`
	IMAPPort         int                      `json:"imap_port"`
	IMAPUsername     string                   `json:"imap_username"`
	Enabled          bool                     `json:"enabled"`
	LastSyncStatus   string                   `json:"last_sync_status"`
	LastSyncError    string                   `json:"last_sync_error"`
	LastSyncErrorLog string                   `json:"last_sync_error_log"`
	LastSyncedAt     *string                  `json:"last_synced_at"`
	CreatedAt        string                   `json:"created_at"`
	UpdatedAt        string                   `json:"updated_at"`
	AliasCount       int                      `json:"alias_count"`
	SyncProgress     *adminAPISyncProgressDTO `json:"sync_progress,omitempty"`
}

type adminAPISyncProgressDTO struct {
	Active     bool   `json:"active"`
	Source     string `json:"source"`
	Stage      string `json:"stage"`
	Percentage int    `json:"percentage"`
	StartedAt  string `json:"started_at"`
	UpdatedAt  string `json:"updated_at"`
}

type adminAPIAliasDTO struct {
	ID                int64   `json:"id"`
	AccountID         int64   `json:"account_id"`
	AccountEmail      string  `json:"account_email"`
	Address           string  `json:"address"`
	Label             string  `json:"label"`
	APIKeyPrefix      string  `json:"api_key_prefix"`
	DirectLinkPath    string  `json:"direct_link_path"`
	APIKey            string  `json:"api_key"`
	IMAPPassword      string  `json:"imap_password"`
	ClientID          string  `json:"client_id"`
	RefreshToken      string  `json:"refresh_token"`
	OTPURLPath        string  `json:"otp_url_path"`
	LegacyDirectLink  string  `json:"legacy_direct_link_path,omitempty"`
	CredentialMode    string  `json:"credential_mode,omitempty"`
	CredentialVersion int64   `json:"credential_version"`
	Enabled           bool    `json:"enabled"`
	LastSyncStatus    string  `json:"last_sync_status"`
	LastSyncError     string  `json:"last_sync_error"`
	LastSyncErrorLog  string  `json:"last_sync_error_log"`
	LastSyncedAt      *string `json:"last_synced_at"`
	LastAccessedAt    *string `json:"last_accessed_at"`
	LatestReceivedAt  *string `json:"latest_received_at"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

type adminAPIAuditLogDTO struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	Action       string `json:"action"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Result       string `json:"result"`
	RequestID    string `json:"request_id"`
	CreatedAt    string `json:"created_at"`
}

type adminAPIAccountDetailDTO struct {
	Account      adminAPIAccountDTO       `json:"account"`
	Aliases      []adminAPIAliasDTO       `json:"aliases"`
	AppleSession *adminAPIAppleSessionDTO `json:"apple_session"`
	AutoCreation *adminAPIAutoCreationDTO `json:"auto_creation"`
	SyncPending  bool                     `json:"sync_pending,omitempty"`
}

type adminAPIAutoCreationDTO struct {
	Enabled          bool     `json:"enabled"`
	Status           string   `json:"status"`
	PlannedAt        *string  `json:"planned_at"`
	PlannedTimes     []string `json:"planned_times"`
	NextRunAt        *string  `json:"next_run_at"`
	LastAttemptedAt  *string  `json:"last_attempted_at"`
	LastCreatedAt    *string  `json:"last_created_at"`
	LastAliasAddress string   `json:"last_alias_address"`
	LastError        string   `json:"last_error"`
	PendingKeyCount  int      `json:"pending_key_count"`
	PendingKeyTotal  int      `json:"pending_auto_created_key_count"`
}

type adminAPIAutoCreationRequest struct {
	Enabled *bool `json:"enabled"`
}

type adminAPIAutoCreationKeysRequest struct {
	AliasIDs []int64 `json:"alias_ids"`
}

type adminAPIOneTimeKeyDTO struct {
	Alias  adminAPIAliasDTO `json:"alias"`
	APIKey string           `json:"api_key"`
}

var (
	errAutoCreationUnavailable     = errors.New("automatic alias creation service is unavailable")
	errAutoCreationAccountDisabled = errors.New("primary account is disabled")
)

type adminAPILoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type adminAPIPasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

// These optional fields retain the distinction between an omitted JSON member
// and an explicitly supplied null; pointers alone map both forms to nil.
type adminAPIOptionalString struct {
	Value   string
	Present bool
	Null    bool
}

func (v *adminAPIOptionalString) UnmarshalJSON(data []byte) error {
	*v = adminAPIOptionalString{Present: true}
	if strings.TrimSpace(string(data)) == "null" {
		v.Null = true
		return nil
	}
	return json.Unmarshal(data, &v.Value)
}

type adminAPIOptionalInt struct {
	Value   int
	Present bool
	Null    bool
}

func (v *adminAPIOptionalInt) UnmarshalJSON(data []byte) error {
	*v = adminAPIOptionalInt{Present: true}
	if strings.TrimSpace(string(data)) == "null" {
		v.Null = true
		return nil
	}
	return json.Unmarshal(data, &v.Value)
}

func adminAPIIMAPEndpointInput(host adminAPIOptionalString, port adminAPIOptionalInt) (*string, *int, string) {
	if host.Null {
		return nil, nil, "IMAP 主机不能为 null"
	}
	if port.Null {
		return nil, nil, "IMAP 端口不能为 null"
	}
	var hostValue *string
	if host.Present {
		hostValue = &host.Value
	}
	var portValue *int
	if port.Present {
		portValue = &port.Value
	}
	return hostValue, portValue, ""
}

type adminAPICreateAccountRequest struct {
	Name         string                 `json:"name"`
	Email        string                 `json:"email"`
	IMAPUsername string                 `json:"imap_username"`
	IMAPPassword string                 `json:"imap_password"`
	IMAPHost     adminAPIOptionalString `json:"imap_host"`
	IMAPPort     adminAPIOptionalInt    `json:"imap_port"`
}

type adminAPIUpdateAccountRequest struct {
	Name         string                 `json:"name"`
	Email        string                 `json:"email"`
	IMAPUsername string                 `json:"imap_username"`
	IMAPPassword string                 `json:"imap_password"`
	IMAPHost     adminAPIOptionalString `json:"imap_host"`
	IMAPPort     adminAPIOptionalInt    `json:"imap_port"`
	Enabled      *bool                  `json:"enabled"`
}

type adminAPICreateAliasRequest struct {
	Address string `json:"address"`
	Label   string `json:"label"`
}

type adminAPIUpdateAliasRequest struct {
	Enabled *bool `json:"enabled"`
}

func adminAPISessionFromDomain(session domain.Session) adminAPISessionDTO {
	return adminAPISessionDTO{
		Admin:     adminAPIAdminDTO{Username: session.Username},
		CSRFToken: session.CSRF,
		ExpiresAt: session.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

func adminAPIAccountFromDomain(account domain.Account) adminAPIAccountDTO {
	return adminAPIAccountDTO{
		ID:               account.ID,
		Name:             account.Name,
		Email:            account.Email,
		IMAPHost:         account.IMAPHost,
		IMAPPort:         account.IMAPPort,
		IMAPUsername:     account.IMAPUsername,
		Enabled:          account.Enabled,
		LastSyncStatus:   account.LastSyncStatus,
		LastSyncError:    adminAPISyncErrorSummary(account.LastSyncError),
		LastSyncErrorLog: account.LastSyncError,
		LastSyncedAt:     adminAPIOptionalTime(account.LastSyncedAt),
		CreatedAt:        adminAPITime(account.CreatedAt),
		UpdatedAt:        adminAPITime(account.UpdatedAt),
		AliasCount:       account.AliasCount,
	}
}

func (s *Server) adminAPIAccountFromDomain(account domain.Account) adminAPIAccountDTO {
	dto := adminAPIAccountFromDomain(account)
	if s.syncProgress == nil {
		return dto
	}
	progress, active := s.syncProgress(account.ID)
	if !active {
		return dto
	}
	dto.SyncProgress = &adminAPISyncProgressDTO{
		Active:     true,
		Source:     string(progress.Trigger),
		Stage:      string(progress.Phase),
		Percentage: progress.Percent,
		StartedAt:  adminAPITime(progress.StartedAt),
		UpdatedAt:  adminAPITime(progress.UpdatedAt),
	}
	return dto
}

func (s *Server) adminAPIAliasFromDomain(alias domain.Alias) (adminAPIAliasDTO, error) {
	dto := adminAPIAliasDTO{
		ID:                alias.ID,
		AccountID:         alias.AccountID,
		AccountEmail:      alias.AccountEmail,
		Address:           alias.Address,
		Label:             alias.Label,
		APIKeyPrefix:      alias.APIKeyPrefix,
		CredentialMode:    alias.CredentialMode,
		CredentialVersion: alias.CredentialVersion,
		Enabled:           alias.Enabled,
		LastSyncStatus:    alias.LastSyncStatus,
		LastSyncError:     adminAPISyncErrorSummary(alias.LastSyncError),
		LastSyncErrorLog:  alias.LastSyncError,
		LastSyncedAt:      adminAPIOptionalTime(alias.LastSyncedAt),
		LastAccessedAt:    adminAPIOptionalTime(alias.LastAccessedAt),
		LatestReceivedAt:  adminAPIOptionalTime(alias.LatestReceivedAt),
		CreatedAt:         adminAPITime(alias.CreatedAt),
		UpdatedAt:         adminAPITime(alias.UpdatedAt),
	}
	if adminAPIAliasConfirmationPending(alias) {
		// Apple has not published this address yet. Do not expose any usable
		// direct link, even if credential material was staged transactionally.
		return dto, nil
	}
	if alias.CredentialMode == domain.AliasCredentialModeLegacy {
		// Credential mode is the compatibility boundary. A legacy row may still
		// contain stale ciphertext or OAuth/hash columns from an interrupted
		// migration; those columns must never turn the row into a v2 credential
		// bundle or expose secrets through the administrator API.
		token, err := s.cipher.DirectLinkToken(alias.ID, alias.APIKeyHash)
		if err != nil {
			return adminAPIAliasDTO{}, fmt.Errorf("生成隐私邮箱兼容直达链接: %w", err)
		}
		query := url.Values{"api_key": {token}}
		legacyDirectLink := (&url.URL{Path: "/api/v1/mail/recent", RawQuery: query.Encode()}).String()
		dto.DirectLinkPath = legacyDirectLink
		dto.LegacyDirectLink = legacyDirectLink
		dto.OTPURLPath = legacyDirectLink
		return dto, nil
	}
	recentToken, err := s.cipher.RecentMailToken(alias.ID, alias.APIKeyHash)
	if err != nil {
		return adminAPIAliasDTO{}, fmt.Errorf("生成隐私邮箱最近邮件链接: %w", err)
	}
	recentQuery := url.Values{"api_key": {recentToken}}
	recentLink := (&url.URL{Path: "/api/v1/mail/recent", RawQuery: recentQuery.Encode()}).String()
	dto.DirectLinkPath = recentLink
	dto.LegacyDirectLink = recentLink

	credentials, err := s.cipher.DecryptAliasCredentials(alias.ID, alias.CredentialCiphertext)
	if err != nil {
		return adminAPIAliasDTO{}, fmt.Errorf("解密隐私邮箱凭证: %w", err)
	}
	otpToken, err := s.cipher.OTPToken(alias.ID, alias.APIKeyHash)
	if err != nil {
		return adminAPIAliasDTO{}, fmt.Errorf("生成隐私邮箱验证码链接: %w", err)
	}
	otpQuery := url.Values{"token": {otpToken}}
	dto.OTPURLPath = (&url.URL{Path: "/api/v1/otp", RawQuery: otpQuery.Encode()}).String()
	dto.APIKey = credentials.APIKey
	dto.IMAPPassword = credentials.IMAPPassword
	dto.ClientID = credentials.ClientID
	dto.RefreshToken = credentials.RefreshToken
	return dto, nil
}

func adminAPISyncErrorSummary(message string) string {
	const maxRunes = 240
	runes := []rune(strings.TrimSpace(message))
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}

func adminAPIAliasConfirmationPending(alias domain.Alias) bool {
	return !alias.Enabled && strings.TrimSpace(alias.LastSyncError) == domain.AppleAliasConfirmationPending
}

func adminAPIAuditLogFromDomain(log domain.AuditLog) adminAPIAuditLogDTO {
	return adminAPIAuditLogDTO{
		ID:           log.ID,
		Username:     log.Username,
		Action:       log.Action,
		ResourceType: log.ResourceType,
		ResourceID:   log.ResourceID,
		Result:       log.Result,
		RequestID:    log.RequestID,
		CreatedAt:    adminAPITime(log.CreatedAt),
	}
}

func adminAPITime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func adminAPIOptionalTime(value *time.Time) *string {
	if value == nil || value.IsZero() {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}

func adminAPIAutoCreationFromSchedule(schedule domain.AliasCreationSchedule, appleStatus string, pendingCount ...int) *adminAPIAutoCreationDTO {
	plannedTimes := make([]string, 0, len(schedule.PlannedAt))
	for _, planned := range schedule.PlannedAt {
		plannedTimes = append(plannedTimes, adminAPITime(planned))
	}
	var plannedAt *string
	if len(plannedTimes) > 0 {
		first := plannedTimes[0]
		plannedAt = &first
	}
	status := "disabled"
	if schedule.Enabled {
		switch {
		case schedule.LastError != "":
			status = "error"
		case appleStatus == hmesync.StatusLoginRequired || appleStatus == hmesync.StatusExpired:
			status = "login_required"
		case schedule.NextRunAt == nil:
			status = "paused"
		default:
			status = "scheduled"
		}
	}
	count := 0
	if len(pendingCount) > 0 {
		count = pendingCount[0]
	}
	return &adminAPIAutoCreationDTO{
		Enabled:          schedule.Enabled,
		Status:           status,
		PlannedAt:        plannedAt,
		PlannedTimes:     plannedTimes,
		NextRunAt:        adminAPIOptionalTime(schedule.NextRunAt),
		LastAttemptedAt:  adminAPIOptionalTime(schedule.LastAttemptedAt),
		LastCreatedAt:    adminAPIOptionalTime(schedule.LastCreatedAt),
		LastAliasAddress: schedule.LastAliasAddress,
		LastError:        schedule.LastError,
		PendingKeyCount:  count,
		PendingKeyTotal:  count,
	}
}

func (s *Server) adminAPIAccountsFromDomain(accounts []domain.Account) []adminAPIAccountDTO {
	result := make([]adminAPIAccountDTO, 0, len(accounts))
	for _, account := range accounts {
		result = append(result, s.adminAPIAccountFromDomain(account))
	}
	return result
}

func (s *Server) adminAPIAliasesFromDomain(aliases []domain.Alias) ([]adminAPIAliasDTO, error) {
	result := make([]adminAPIAliasDTO, 0, len(aliases))
	for _, alias := range aliases {
		dto, err := s.adminAPIAliasFromDomain(alias)
		if err != nil {
			return nil, err
		}
		result = append(result, dto)
	}
	return result, nil
}

func (s *Server) adminAPILoginRequestGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.loginRequestLimiter.Allow(c.ClientIP()) {
			writeAdminAPIError(c, http.StatusTooManyRequests, "RATE_LIMITED", "登录请求过于频繁，请稍后再试")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Server) adminAPIAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := c.Cookie(sessionCookie)
		if err != nil || raw == "" {
			writeAdminAPIError(c, http.StatusUnauthorized, "AUTH_REQUIRED", "请先登录")
			c.Abort()
			return
		}
		session, err := s.store.GetSessionByHash(c.Request.Context(), secure.HashToken(raw))
		if errors.Is(err, store.ErrNotFound) {
			s.clearSessionCookies(c)
			writeAdminAPIError(c, http.StatusUnauthorized, "SESSION_EXPIRED", "登录会话已过期")
			c.Abort()
			return
		}
		if err != nil {
			s.writeAdminAPIInternalError(c, err)
			c.Abort()
			return
		}
		c.Set(sessionKey, session)
		c.Next()
	}
}

func (s *Server) adminAPICSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		session := mustSession(c)
		provided := c.GetHeader(adminAPICSRFHeader)
		validToken := len(provided) == len(session.CSRF) && subtle.ConstantTimeCompare([]byte(provided), []byte(session.CSRF)) == 1
		if !validToken || !sameOrigin(c.Request) {
			writeAdminAPIError(c, http.StatusForbidden, "CSRF_INVALID", "请求校验失败，请刷新页面后重试")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Server) adminAPILoginCSRF(basePath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token, err := c.Cookie(adminAPILoginCSRFCookie); err == nil && len(token) == 32 {
			writeAdminAPIData(c, http.StatusOK, gin.H{"csrf_token": token, "expires_in": 600})
			return
		}
		token, err := secure.RandomToken(24)
		if err != nil {
			s.writeAdminAPIInternalError(c, err)
			return
		}
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     adminAPILoginCSRFCookie,
			Value:    token,
			Path:     basePath + "/auth",
			MaxAge:   600,
			HttpOnly: true,
			Secure:   s.cfg.CookieSecure,
			SameSite: http.SameSiteStrictMode,
		})
		c.Header("Cache-Control", "no-store")
		writeAdminAPIData(c, http.StatusOK, gin.H{"csrf_token": token, "expires_in": 600})
	}
}

func (s *Server) adminAPILogin(basePath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		provided := c.GetHeader(adminAPICSRFHeader)
		if reason := adminAPILoginCSRFFailureReason(c.Request, provided); reason != "" {
			s.logger.Warn("后台 API 登录请求校验失败",
				"reason", reason,
				"cookie_count", requestCookieCount(c.Request, adminAPILoginCSRFCookie),
				"request_id", requestID(c),
			)
			writeAdminAPIError(c, http.StatusForbidden, "CSRF_INVALID", "请求校验失败，请重新获取登录校验信息")
			return
		}
		var input adminAPILoginRequest
		if !decodeAdminAPIJSON(c, &input) {
			return
		}
		username := strings.TrimSpace(input.Username)
		if username == "" || len(username) > 128 || len(input.Password) > 72 {
			writeAdminAPIError(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "用户名或密码错误")
			return
		}
		if !s.loginLimiter.Allow(c.ClientIP()) {
			writeAdminAPIError(c, http.StatusTooManyRequests, "RATE_LIMITED", "尝试次数过多，请稍后再试")
			return
		}

		admin, lookupErr := s.store.GetAdminByUsername(c.Request.Context(), username)
		hash := []byte("$2a$12$MmRQk4bYUn7nFdy4xTqIBuUqhgjYuSmkwJtQA4n.IqQvEp4zrC23e")
		if lookupErr == nil {
			hash = []byte(admin.PasswordHash)
		}
		passwordOK := bcrypt.CompareHashAndPassword(hash, []byte(input.Password)) == nil
		if lookupErr != nil || !passwordOK {
			s.audit(c, nil, username, "login", "admin", "", "failed", "")
			writeAdminAPIError(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "用户名或密码错误")
			return
		}
		if _, err := s.store.DeleteExpiredSessions(c.Request.Context()); err != nil {
			s.logger.Warn("清理过期后台会话失败", "error", err, "request_id", requestID(c))
		}

		rawToken, err := secure.RandomToken(32)
		if err != nil {
			s.writeAdminAPIInternalError(c, err)
			return
		}
		csrf, err := secure.RandomToken(24)
		if err != nil {
			s.writeAdminAPIInternalError(c, err)
			return
		}
		session := domain.Session{
			AdminID:         admin.ID,
			Username:        admin.Username,
			PasswordVersion: admin.PasswordVersion,
			CSRF:            csrf,
			ExpiresAt:       time.Now().UTC().Add(s.cfg.SessionTTL),
		}
		if err := s.store.CreateSession(c.Request.Context(), secure.HashToken(rawToken), session); err != nil {
			if errors.Is(err, store.ErrCredentialsChanged) {
				s.audit(c, &admin.ID, admin.Username, "login", "admin", "", "failed", "credentials_changed")
				writeAdminAPIError(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "用户名或密码错误")
				return
			}
			s.writeAdminAPIInternalError(c, err)
			return
		}
		s.setSessionCookies(c, rawToken)
		s.clearAdminAPILoginCSRFCookie(c, basePath)
		s.audit(c, &admin.ID, admin.Username, "login", "admin", "", "success", "")
		writeAdminAPIData(c, http.StatusOK, adminAPISessionFromDomain(session))
	}
}

func adminAPILoginCSRFFailureReason(r *http.Request, provided string) string {
	cookie, err := r.Cookie(adminAPILoginCSRFCookie)
	if err != nil || cookie.Value == "" {
		return loginCSRFCookieMissing
	}
	if reason := originFailureReason(r); reason != "" {
		return reason
	}
	if provided == "" {
		return loginCSRFFormTokenMissing
	}
	if len(cookie.Value) != len(provided) || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(provided)) != 1 {
		return loginCSRFTokenMismatch
	}
	return ""
}

func (s *Server) clearAdminAPILoginCSRFCookie(c *gin.Context, basePath string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     adminAPILoginCSRFCookie,
		Value:    "",
		Path:     basePath + "/auth",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) adminAPISession(c *gin.Context) {
	writeAdminAPIData(c, http.StatusOK, adminAPISessionFromDomain(mustSession(c)))
}

func (s *Server) adminAPILogout(_ string) gin.HandlerFunc {
	return func(c *gin.Context) {
		session := mustSession(c)
		raw, err := c.Cookie(sessionCookie)
		if err == nil && raw != "" {
			if err := s.store.DeleteSession(c.Request.Context(), secure.HashToken(raw)); err != nil {
				s.audit(c, &session.AdminID, session.Username, "logout", "admin", "", "failed", "")
				s.writeAdminAPIInternalError(c, err)
				return
			}
		}
		s.audit(c, &session.AdminID, session.Username, "logout", "admin", "", "success", "")
		s.clearSessionCookies(c)
		c.Status(http.StatusNoContent)
	}
}

func (s *Server) adminAPIChangePassword(_ string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input adminAPIPasswordRequest
		if !decodeAdminAPIJSON(c, &input) {
			return
		}
		session := mustSession(c)
		admin, err := s.store.GetAdminByID(c.Request.Context(), session.AdminID)
		if errors.Is(err, store.ErrNotFound) {
			s.clearSessionCookies(c)
			writeAdminAPIError(c, http.StatusUnauthorized, "SESSION_EXPIRED", "登录会话已失效")
			return
		}
		if err != nil {
			s.writeAdminAPIInternalError(c, err)
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(input.CurrentPassword)) != nil {
			writeAdminAPIError(c, http.StatusUnauthorized, "CURRENT_PASSWORD_INVALID", "当前密码不正确")
			return
		}
		if len(input.NewPassword) < 12 || len(input.NewPassword) > 72 {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "新密码长度需要在 12 到 72 字节之间")
			return
		}
		if input.NewPassword != input.ConfirmPassword {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "两次填写的新密码不一致")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(input.NewPassword), 12)
		if err != nil {
			s.writeAdminAPIInternalError(c, err)
			return
		}
		if err := s.store.ChangeAdminPasswordAndRevokeSessions(c.Request.Context(), admin.ID, admin.PasswordVersion, string(hash)); err != nil {
			if errors.Is(err, store.ErrCredentialsChanged) {
				s.clearSessionCookies(c)
				writeAdminAPIError(c, http.StatusConflict, "CREDENTIALS_CHANGED", "登录凭据已在其他请求中更新，请重新登录")
				return
			}
			s.writeAdminAPIInternalError(c, err)
			return
		}
		s.audit(c, &admin.ID, admin.Username, "change_password", "admin", "", "success", "")
		s.clearSessionCookies(c)
		writeAdminAPIData(c, http.StatusOK, gin.H{"reauthentication_required": true})
	}
}

func (s *Server) adminAPIListAccounts(c *gin.Context) {
	limit, offset, ok := adminAPIListPage(c)
	if !ok {
		return
	}
	query := strings.TrimSpace(c.Query("query"))
	if len([]rune(query)) > adminAPIMaxListQueryRunes {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "query 参数不能超过 200 个字符")
		return
	}
	page, err := s.store.ListAccountsPage(c.Request.Context(), store.AccountListFilter{
		Query:  query,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	writeAdminAPIData(c, http.StatusOK, gin.H{
		"items":      s.adminAPIAccountsFromDomain(page.Items),
		"pagination": adminAPIPagination(limit, offset, page.Total),
	})
}

func (s *Server) adminAPICreateAccount(basePath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input adminAPICreateAccountRequest
		if !decodeAdminAPIJSON(c, &input) {
			return
		}
		host, port, message := adminAPIIMAPEndpointInput(input.IMAPHost, input.IMAPPort)
		if message != "" {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", message)
			return
		}
		account, password, message := adminAPIAccountInput(
			input.Name, input.Email, input.IMAPUsername, input.IMAPPassword,
			host, port, domain.Account{Enabled: true},
		)
		if message != "" {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", message)
			return
		}
		encrypted, err := s.cipher.Encrypt(password)
		if err != nil {
			s.writeAdminAPIInternalError(c, err)
			return
		}
		account.PasswordCiphertext = encrypted
		created, err := s.store.CreateAccount(c.Request.Context(), account)
		if err != nil {
			if adminAPIUniqueConstraint(err) {
				writeAdminAPIError(c, http.StatusConflict, "ACCOUNT_EXISTS", "这个主号已经存在")
				return
			}
			s.writeAdminAPIInternalError(c, err)
			return
		}
		session := mustSession(c)
		s.audit(c, &session.AdminID, session.Username, "create", "account", strconv.FormatInt(created.ID, 10), "success", "")
		c.Header("Location", fmt.Sprintf("%s/accounts/%d", basePath, created.ID))
		writeAdminAPIData(c, http.StatusCreated, adminAPIAccountFromDomain(created))
	}
}

func (s *Server) adminAPIGetAccount(c *gin.Context) {
	id, ok := adminAPIParseID(c)
	if !ok {
		return
	}
	detail, err := s.adminAPIAccountDetail(c.Request.Context(), id)
	if err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	writeAdminAPIData(c, http.StatusOK, detail)
}

func (s *Server) adminAPIUpdateAccount(c *gin.Context) {
	id, ok := adminAPIParseID(c)
	if !ok {
		return
	}
	var input adminAPIUpdateAccountRequest
	if !decodeAdminAPIJSON(c, &input) {
		return
	}
	if input.Enabled == nil {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "请明确指定主号是否启用")
		return
	}
	host, port, message := adminAPIIMAPEndpointInput(input.IMAPHost, input.IMAPPort)
	if message != "" {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", message)
		return
	}
	var updated domain.Account
	var validationMessage string
	err := s.withAccountLock(c.Request.Context(), id, func() error {
		existing, readErr := s.store.GetAccount(c.Request.Context(), id)
		if readErr != nil {
			return readErr
		}
		account, password, message := adminAPIAccountInput(
			input.Name, input.Email, input.IMAPUsername, input.IMAPPassword,
			host, port, existing,
		)
		account.ID = id
		account.Enabled = *input.Enabled
		if existing.AliasCount > 0 && account.Email != existing.Email {
			return store.ErrAccountIdentityLocked
		}
		if message != "" {
			validationMessage = message
			return nil
		}
		if password != "" {
			account.PasswordCiphertext, readErr = s.cipher.Encrypt(password)
			if readErr != nil {
				return readErr
			}
		} else {
			account.PasswordCiphertext = ""
		}
		var updateErr error
		updated, updateErr = s.store.UpdateAccount(c.Request.Context(), account)
		return updateErr
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "主号不存在")
		case errors.Is(err, store.ErrAccountIdentityLocked):
			writeAdminAPIError(c, http.StatusConflict, "ACCOUNT_IDENTITY_LOCKED", "已有隐私邮箱时不能修改主号邮箱")
		case adminAPIUniqueConstraint(err):
			writeAdminAPIError(c, http.StatusConflict, "ACCOUNT_EXISTS", "这个主号已经存在")
		default:
			s.writeAdminAPIInternalError(c, err)
		}
		return
	}
	if validationMessage != "" {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", validationMessage)
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "update", "account", strconv.FormatInt(id, 10), "success", "")
	writeAdminAPIData(c, http.StatusOK, adminAPIAccountFromDomain(updated))
}

func adminAPIAccountInput(name, email, username, password string, host *string, port *int, base domain.Account) (domain.Account, string, string) {
	base.Name = strings.TrimSpace(name)
	base.Email = domain.NormalizeEmail(email)
	base.IMAPUsername = strings.TrimSpace(username)
	if host != nil {
		if strings.TrimSpace(*host) == "" {
			return base, strings.TrimSpace(password), "IMAP 主机不能为空"
		}
		base.IMAPHost = *host
	} else if strings.TrimSpace(base.IMAPHost) == "" {
		base.IMAPHost = domain.DefaultIMAPHost
	}
	if port != nil {
		base.IMAPPort = *port
	} else if base.IMAPPort == 0 {
		base.IMAPPort = domain.DefaultIMAPPort
	}
	password = strings.TrimSpace(password)
	normalizedHost, normalizedPort, endpointErr := domain.NormalizeIMAPEndpoint(base.IMAPHost, base.IMAPPort)
	if endpointErr == nil {
		base.IMAPHost, base.IMAPPort = normalizedHost, normalizedPort
	}
	switch {
	case utf8.RuneCountInString(base.Name) > 80:
		return base, password, "备注不能超过 80 个字符"
	case validateEmail(base.Email) != nil:
		return base, password, "主号邮箱格式不正确"
	case base.IMAPUsername == "":
		return base, password, "请填写 IMAP 用户名"
	case endpointErr != nil:
		return base, password, "IMAP 服务地址无效: " + endpointErr.Error()
	case base.PasswordCiphertext == "" && password == "":
		return base, password, "请填写 App 专用密码"
	default:
		return base, password, ""
	}
}

func (s *Server) adminAPIDeleteAccount(c *gin.Context) {
	id, ok := adminAPIParseID(c)
	if !ok {
		return
	}
	if err := s.withAccountLock(c.Request.Context(), id, func() error {
		return s.store.DeleteAccount(c.Request.Context(), id)
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "主号不存在")
		} else {
			s.writeAdminAPIInternalError(c, err)
		}
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "delete", "account", strconv.FormatInt(id, 10), "success", "")
	c.Status(http.StatusNoContent)
}

func (s *Server) adminAPISyncAccount(c *gin.Context) {
	id, ok := adminAPIParseID(c)
	if !ok {
		return
	}
	account, err := s.store.GetAccount(c.Request.Context(), id)
	if err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	if !account.Enabled {
		writeAdminAPIError(c, http.StatusConflict, "ACCOUNT_DISABLED", "主号已停用，请先启用主号后再同步邮件")
		return
	}
	result := "success"
	var syncErr error
	if s.sync == nil {
		syncErr = errors.New("sync service is unavailable")
	} else {
		syncErr = s.sync(id)
	}
	pending := errors.Is(syncErr, syncer.ErrSyncPending) || errors.Is(syncErr, syncer.ErrSyncQueued)
	if syncErr != nil && !pending {
		result = "failed"
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "sync", "account", strconv.FormatInt(id, 10), result, "")
	if syncErr != nil && !pending {
		writeAdminAPIError(c, http.StatusBadGateway, "SYNC_FAILED", "同步失败，请检查连接状态")
		return
	}
	detail, err := s.adminAPIAccountDetail(c.Request.Context(), id)
	if err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	detail.SyncPending = pending
	status := http.StatusOK
	if pending {
		status = http.StatusAccepted
	}
	writeAdminAPIData(c, status, detail)
}

func (s *Server) adminAPIAccountDetail(ctx context.Context, id int64) (adminAPIAccountDetailDTO, error) {
	account, err := s.store.GetAccount(ctx, id)
	if err != nil {
		return adminAPIAccountDetailDTO{}, err
	}
	aliases, err := s.store.ListAliasesByAccount(ctx, id)
	if err != nil {
		return adminAPIAccountDetailDTO{}, err
	}
	aliasDTOs, err := s.adminAPIAliasesFromDomain(aliases)
	if err != nil {
		return adminAPIAccountDetailDTO{}, err
	}
	appleSession, err := s.adminAPIAppleSession(ctx, id)
	if err != nil {
		return adminAPIAccountDetailDTO{}, err
	}
	autoCreation, err := s.adminAPIAutoCreation(ctx, id, appleSessionStatus(appleSession))
	if err != nil {
		return adminAPIAccountDetailDTO{}, err
	}
	return adminAPIAccountDetailDTO{
		Account:      s.adminAPIAccountFromDomain(account),
		Aliases:      aliasDTOs,
		AppleSession: appleSession,
		AutoCreation: autoCreation,
	}, nil
}

func appleSessionStatus(session *adminAPIAppleSessionDTO) string {
	if session == nil {
		return ""
	}
	return session.Status
}

func (s *Server) adminAPIAutoCreation(ctx context.Context, accountID int64, appleStatus string) (*adminAPIAutoCreationDTO, error) {
	schedule := domain.AliasCreationSchedule{AccountID: accountID}
	if s.autoCreate != nil {
		loaded, err := s.autoCreate.GetSchedule(ctx, accountID)
		if err != nil {
			return nil, err
		}
		schedule = loaded
	}
	pending, err := s.store.CountPendingAliasAPIKeysByAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return adminAPIAutoCreationFromSchedule(schedule, appleStatus, pending), nil
}

func (s *Server) adminAPICreateAlias(basePath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		accountID, ok := adminAPIParseID(c)
		if !ok {
			return
		}
		if _, err := s.store.GetAccount(c.Request.Context(), accountID); err != nil {
			s.writeAdminAPIStoreReadError(c, err)
			return
		}
		var input adminAPICreateAliasRequest
		if !decodeAdminAPIJSON(c, &input) {
			return
		}
		address := domain.NormalizeEmail(input.Address)
		label := strings.TrimSpace(input.Label)
		if validateEmail(address) != nil {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "邮箱地址格式不正确")
			return
		}
		if utf8.RuneCountInString(label) > 100 {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "用途备注不能超过 100 个字符")
			return
		}
		var alias domain.Alias
		err := s.withAccountLock(c.Request.Context(), accountID, func() error {
			var createErr error
			alias, createErr = s.store.CreateAlias(c.Request.Context(), domain.Alias{
				AccountID: accountID,
				Address:   address,
				Label:     label,
				Enabled:   true,
			})
			return createErr
		})
		if err != nil {
			switch {
			case errors.Is(err, store.ErrAliasLimit):
				writeAdminAPIError(c, http.StatusConflict, "ALIAS_LIMIT_REACHED", fmt.Sprintf("此主号最多启用 %d 个隐私邮箱", domain.MaxEnabledAliasesPerAccount))
			case adminAPIUniqueConstraint(err):
				writeAdminAPIError(c, http.StatusConflict, "ALIAS_EXISTS", "这个隐私邮箱已经登记")
			default:
				s.writeAdminAPIInternalError(c, err)
			}
			return
		}
		aliasDTO, err := s.adminAPIAliasFromDomain(alias)
		if err != nil {
			s.writeAdminAPIInternalError(c, err)
			return
		}
		if strings.TrimSpace(aliasDTO.APIKey) == "" {
			s.writeAdminAPIInternalError(c, errors.New("新建 v2 隐私邮箱未生成 API Key"))
			return
		}
		session := mustSession(c)
		s.audit(c, &session.AdminID, session.Username, "create", "alias", strconv.FormatInt(alias.ID, 10), "success", "")
		c.Header("Cache-Control", "no-store")
		c.Header("Location", fmt.Sprintf("%s/aliases/%d", basePath, alias.ID))
		// The original administrator contract returned the newly generated API key
		// in a one-time envelope. Keep that shape for manually created aliases so
		// existing clients can continue to persist the key they need to use the
		// mailbox, while the alias DTO still carries the v2 direct-link metadata.
		writeAdminAPIData(c, http.StatusCreated, adminAPIOneTimeKeyDTO{
			Alias:  aliasDTO,
			APIKey: aliasDTO.APIKey,
		})
	}
}

func (s *Server) adminAPISetAliasAutoCreation(c *gin.Context) {
	accountID, ok := adminAPIParseID(c)
	if !ok {
		return
	}
	if s.autoCreate == nil {
		writeAdminAPIError(c, http.StatusServiceUnavailable, "AUTO_CREATION_UNAVAILABLE", "自动创建服务暂不可用")
		return
	}
	var input adminAPIAutoCreationRequest
	if !decodeAdminAPIJSON(c, &input) {
		return
	}
	if input.Enabled == nil {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "请明确指定是否开启自动创建")
		return
	}
	var (
		schedule    domain.AliasCreationSchedule
		appleStatus string
		pending     int
	)
	err := s.withAccountLock(c.Request.Context(), accountID, func() error {
		account, err := s.store.GetAccount(c.Request.Context(), accountID)
		if err != nil {
			return err
		}
		if *input.Enabled {
			if !account.Enabled {
				return errAutoCreationAccountDisabled
			}
			if s.hmeSync == nil {
				return errAutoCreationUnavailable
			}
			info, err := s.hmeSync.GetSession(c.Request.Context(), accountID)
			if err != nil {
				return err
			}
			appleStatus = info.Status
			if info.Status != hmesync.StatusAuthenticated {
				if info.Status == hmesync.StatusExpired {
					return hmesync.ErrSessionExpired
				}
				return hmesync.ErrLoginRequired
			}
		}
		var setErr error
		schedule, setErr = s.autoCreate.SetEnabled(c.Request.Context(), accountID, *input.Enabled)
		if setErr != nil {
			return setErr
		}
		pending, setErr = s.store.CountPendingAliasAPIKeysByAccount(c.Request.Context(), accountID)
		return setErr
	})
	if err != nil {
		switch {
		case errors.Is(err, errAutoCreationUnavailable):
			writeAdminAPIError(c, http.StatusServiceUnavailable, "AUTO_CREATION_UNAVAILABLE", "自动创建服务暂不可用")
		case errors.Is(err, errAutoCreationAccountDisabled):
			writeAdminAPIError(c, http.StatusConflict, "ACCOUNT_DISABLED", "主号已停用，不能开启自动创建")
		case errors.Is(err, store.ErrAccountDisabled):
			writeAdminAPIError(c, http.StatusConflict, "ACCOUNT_DISABLED", "主号已停用，不能开启自动创建")
		case errors.Is(err, store.ErrNotFound):
			writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "主号不存在")
		case errors.Is(err, hmesync.ErrLoginRequired), errors.Is(err, hmesync.ErrSessionExpired):
			s.adminAPIFinishAppleFailure(c, mustSession(c), accountID, "alias_auto_create_enable", classifyAdminAPIAppleError(err))
		default:
			s.writeAdminAPIInternalError(c, err)
		}
		return
	}
	dto := adminAPIAutoCreationFromSchedule(schedule, appleStatus, pending)
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "alias_auto_create_set", "account", strconv.FormatInt(accountID, 10), "success", strconv.FormatBool(*input.Enabled))
	writeAdminAPIData(c, http.StatusOK, dto)
}

// adminAPIGetAliasAutoCreationKeys preserves the v1 administrator workflow:
// reading the queue is non-destructive, and the raw key remains available
// until the caller explicitly acknowledges it with the DELETE endpoint.
func (s *Server) adminAPIGetAliasAutoCreationKeys(c *gin.Context) {
	accountID, ok := adminAPIParseID(c)
	if !ok || !s.adminAPIAppleAccountExists(c, accountID) {
		return
	}
	pending, err := s.store.ListPendingAliasAPIKeysByAccount(c.Request.Context(), accountID)
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	created := make([]adminAPIAppleCreatedAliasDTO, 0, len(pending))
	for _, item := range pending {
		rawKey, decryptErr := s.cipher.DecryptPendingAliasAPIKey(item.APIKeyCiphertext)
		if decryptErr != nil {
			s.writeAdminAPIInternalError(c, decryptErr)
			return
		}
		aliasDTO, aliasErr := s.adminAPIAliasFromDomain(item.Alias)
		if aliasErr != nil {
			s.writeAdminAPIInternalError(c, aliasErr)
			return
		}
		created = append(created, adminAPIAppleCreatedAliasDTO{
			Alias:             aliasDTO,
			APIKey:            rawKey,
			MailAPIDirectLink: aliasDTO.DirectLinkPath,
		})
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "alias_auto_create_keys_read", "account", strconv.FormatInt(accountID, 10), "success", strconv.Itoa(len(created)))
	writeAdminAPIData(c, http.StatusOK, gin.H{"created": created})
}

func (s *Server) adminAPIAcknowledgeAliasAutoCreationKeys(c *gin.Context) {
	accountID, ok := adminAPIParseID(c)
	if !ok || !s.adminAPIAppleAccountExists(c, accountID) {
		return
	}
	var input adminAPIAutoCreationKeysRequest
	if !decodeAdminAPIJSON(c, &input) {
		return
	}
	if len(input.AliasIDs) == 0 || len(input.AliasIDs) > domain.MaxEnabledAliasesPerAccount {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "请提供要确认保存的隐私邮箱 ID")
		return
	}
	seen := make(map[int64]struct{}, len(input.AliasIDs))
	ids := make([]int64, 0, len(input.AliasIDs))
	for _, id := range input.AliasIDs {
		if id < 1 {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "隐私邮箱 ID 必须是正整数")
			return
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if err := s.store.DeletePendingAliasAPIKeys(c.Request.Context(), accountID, ids); err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "alias_auto_create_keys_ack", "account", strconv.FormatInt(accountID, 10), "success", strconv.Itoa(len(ids)))
	c.Status(http.StatusNoContent)
}

func (s *Server) adminAPIListAliases(c *gin.Context) {
	limit, offset, ok := adminAPIListPage(c)
	if !ok {
		return
	}
	query := strings.TrimSpace(c.Query("query"))
	if len([]rune(query)) > adminAPIMaxListQueryRunes {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "query 参数不能超过 200 个字符")
		return
	}
	var accountID *int64
	if rawAccountID := strings.TrimSpace(c.Query("account_id")); rawAccountID != "" {
		parsedID, parseErr := strconv.ParseInt(rawAccountID, 10, 64)
		if parseErr != nil || parsedID < 1 {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "account_id 必须是正整数")
			return
		}
		accountID = &parsedID
	}
	page, err := s.store.ListAliasesPage(c.Request.Context(), store.AliasListFilter{
		AccountID: accountID,
		Query:     query,
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	aliasDTOs, err := s.adminAPIAliasesFromDomain(page.Items)
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	writeAdminAPIData(c, http.StatusOK, gin.H{
		"items":      aliasDTOs,
		"pagination": adminAPIPagination(limit, offset, page.Total),
	})
}

func (s *Server) adminAPIGetAlias(c *gin.Context) {
	id, ok := adminAPIParseID(c)
	if !ok {
		return
	}
	alias, err := s.store.GetAlias(c.Request.Context(), id)
	if err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	aliasDTO, err := s.adminAPIAliasFromDomain(alias)
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	writeAdminAPIData(c, http.StatusOK, aliasDTO)
}

func (s *Server) adminAPIRotateAliasCredentials(c *gin.Context) {
	id, ok := adminAPIParseID(c)
	if !ok {
		return
	}
	if _, err := s.store.GetAlias(c.Request.Context(), id); err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	alias, err := s.store.RotateAliasCredentials(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrAliasConfirmationPending) {
			writeAdminAPIAliasConfirmationPending(c)
		} else if errors.Is(err, store.ErrAliasCredentialMode) {
			writeAdminAPIError(c, http.StatusConflict, "ALIAS_CREDENTIAL_MODE_UNSUPPORTED", "旧版隐私邮箱只支持轮换 API Key")
		} else {
			s.writeAdminAPIStoreReadError(c, err)
		}
		return
	}
	aliasDTO, err := s.adminAPIAliasFromDomain(alias)
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "rotate_credentials", "alias", strconv.FormatInt(id, 10), "success", "")
	c.Header("Cache-Control", "no-store")
	writeAdminAPIData(c, http.StatusOK, aliasDTO)
}

// adminAPIRotateAliasKey preserves the original route and one-time response.
// It replaces only the API key; v2 IMAP/OAuth credentials remain valid.
func (s *Server) adminAPIRotateAliasKey(c *gin.Context) {
	id, ok := adminAPIParseID(c)
	if !ok {
		return
	}
	if _, err := s.store.GetAlias(c.Request.Context(), id); err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	rawKey, hash, prefix, err := secure.NewAPIKey()
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	alias, err := s.store.RotateAliasAPIKeyWithRawKey(c.Request.Context(), id, hash, prefix, rawKey)
	if err != nil {
		if errors.Is(err, store.ErrAliasConfirmationPending) {
			writeAdminAPIAliasConfirmationPending(c)
			return
		}
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	aliasDTO, err := s.adminAPIAliasFromDomain(alias)
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "rotate_key", "alias", strconv.FormatInt(id, 10), "success", "")
	c.Header("Cache-Control", "no-store")
	writeAdminAPIData(c, http.StatusOK, adminAPIOneTimeKeyDTO{Alias: aliasDTO, APIKey: rawKey})
}

func (s *Server) adminAPIUpdateAlias(c *gin.Context) {
	id, ok := adminAPIParseID(c)
	if !ok {
		return
	}
	var input adminAPIUpdateAliasRequest
	if !decodeAdminAPIJSON(c, &input) {
		return
	}
	if input.Enabled == nil {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "请明确指定隐私邮箱是否启用")
		return
	}
	alias, err := s.store.GetAlias(c.Request.Context(), id)
	if err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	if alias.Enabled != *input.Enabled {
		err = s.withAccountLock(c.Request.Context(), alias.AccountID, func() error {
			return s.store.SetAliasEnabled(c.Request.Context(), id, *input.Enabled)
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "隐私邮箱不存在")
			} else if errors.Is(err, store.ErrAliasLimit) {
				writeAdminAPIError(c, http.StatusConflict, "ALIAS_LIMIT_REACHED", fmt.Sprintf("此主号最多启用 %d 个隐私邮箱", domain.MaxEnabledAliasesPerAccount))
			} else if errors.Is(err, store.ErrAliasConfirmationPending) {
				writeAdminAPIAliasConfirmationPending(c)
			} else {
				s.writeAdminAPIInternalError(c, err)
			}
			return
		}
		alias, err = s.store.GetAlias(c.Request.Context(), id)
		if err != nil {
			s.writeAdminAPIStoreReadError(c, err)
			return
		}
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "toggle", "alias", strconv.FormatInt(id, 10), "success", "")
	aliasDTO, err := s.adminAPIAliasFromDomain(alias)
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	writeAdminAPIData(c, http.StatusOK, aliasDTO)
}

func (s *Server) adminAPIDeleteAlias(c *gin.Context) {
	id, ok := adminAPIParseID(c)
	if !ok {
		return
	}
	alias, err := s.store.GetAlias(c.Request.Context(), id)
	if err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	if adminAPIAliasConfirmationPending(alias) {
		writeAdminAPIAliasConfirmationPending(c)
		return
	}
	adminSession := mustSession(c)
	if s.hmeSync == nil {
		s.adminAPIFinishAppleAliasDeleteFailure(c, adminSession, id, adminAPIAppleServiceUnavailable())
		return
	}
	if err := s.hmeSync.DeleteAlias(c.Request.Context(), id); err != nil {
		if errors.Is(err, store.ErrAliasConfirmationPending) {
			writeAdminAPIAliasConfirmationPending(c)
		} else {
			apiErr := classifyAdminAPIAppleError(err)
			if errors.Is(err, store.ErrNotFound) && apiErr.Status == http.StatusNotFound {
				writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "隐私邮箱不存在")
			} else {
				s.adminAPIFinishAppleAliasDeleteFailure(c, adminSession, id, apiErr)
			}
		}
		return
	}
	s.audit(c, &adminSession.AdminID, adminSession.Username, "delete", "alias", strconv.FormatInt(id, 10), "success", "")
	c.Status(http.StatusNoContent)
}

func (s *Server) adminAPIListAuditLogs(c *gin.Context) {
	limit, offset, ok := adminAPIListPage(c)
	if !ok {
		return
	}
	page, err := s.store.ListAuditLogsPage(c.Request.Context(), store.AuditLogFilter{Limit: limit, Offset: offset})
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	items := make([]adminAPIAuditLogDTO, 0, len(page.Items))
	for _, log := range page.Items {
		items = append(items, adminAPIAuditLogFromDomain(log))
	}
	writeAdminAPIData(c, http.StatusOK, gin.H{
		"items":      items,
		"pagination": adminAPIPagination(limit, offset, page.Total),
	})
}

func adminAPIListPage(c *gin.Context) (int, int, bool) {
	limit, ok := adminAPIQueryInt(c, "limit", adminAPIDefaultPageLimit, 1, adminAPIMaxPageLimit)
	if !ok {
		return 0, 0, false
	}
	offset, ok := adminAPIQueryInt(c, "offset", 0, 0, adminAPIMaxPageOffset)
	if !ok {
		return 0, 0, false
	}
	return limit, offset, true
}

func adminAPIPagination(limit, offset, total int) gin.H {
	if total < 0 {
		total = 0
	}
	hasMore := offset < total && total-offset > limit
	return gin.H{
		"limit":    limit,
		"offset":   offset,
		"total":    total,
		"has_more": hasMore,
	}
}

func adminAPIQueryInt(c *gin.Context, name string, fallback, minimum, maximum int) (int, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", fmt.Sprintf("%s 参数无效", name))
		return 0, false
	}
	return value, true
}

func adminAPIParseID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "资源不存在")
		return 0, false
	}
	return id, true
}

func decodeAdminAPIJSON(c *gin.Context, destination any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeAdminAPIError(c, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "请求必须使用 application/json")
		return false
	}
	if c.Request.ContentLength > adminAPIMaxJSONBytes {
		writeAdminAPIError(c, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "JSON 请求体不能超过 64 KiB")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, adminAPIMaxJSONBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeAdminAPIJSONDecodeError(c, err)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeAdminAPIJSONDecodeError(c, err)
		return false
	}
	return true
}

func writeAdminAPIJSONDecodeError(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeAdminAPIError(c, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "JSON 请求体不能超过 64 KiB")
		return
	}
	writeAdminAPIError(c, http.StatusBadRequest, "INVALID_JSON", "JSON 请求体格式错误或包含未知字段")
}

func writeAdminAPIData(c *gin.Context, status int, data any) {
	c.JSON(status, gin.H{"data": data})
}

func writeAdminAPIError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{
		"code":       code,
		"message":    message,
		"request_id": requestID(c),
	}})
}

func writeAdminAPIAliasConfirmationPending(c *gin.Context) {
	writeAdminAPIError(c, http.StatusConflict, "ALIAS_CONFIRMATION_PENDING", "该隐私邮箱正在等待 Apple 目录确认，暂时不能修改或删除")
}

func (s *Server) writeAdminAPIInternalError(c *gin.Context, err error) {
	s.logger.Error("后台 JSON API 请求失败", "error", err, "request_id", requestID(c))
	writeAdminAPIError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "请求处理失败，请稍后重试")
}

func (s *Server) writeAdminAPIStoreReadError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "资源不存在")
		return
	}
	s.writeAdminAPIInternalError(c, err)
}

func adminAPIUniqueConstraint(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

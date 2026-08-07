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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

const (
	adminAPILoginCSRFCookie = "icloud_api_login_csrf"
	adminAPILoginCSRFPath   = "/admin/api/v1/auth"
	adminAPICSRFHeader      = "X-CSRF-Token"
	adminAPIMaxJSONBytes    = 64 << 10
)

// registerAdminAPIRoutes attaches the JSON admin API to a group whose base
// path is /admin/api/v1. The HTML admin routes deliberately use separate
// authentication, body parsing, and CSRF middleware.
func (s *Server) registerAdminAPIRoutes(api *gin.RouterGroup) {
	auth := api.Group("/auth")
	auth.GET("/csrf", s.adminAPILoginCSRF)
	auth.POST("/login", s.adminAPILoginRequestGate(), s.adminAPILogin)

	protected := api.Group("")
	protected.Use(s.adminAPIAuth(), s.adminAPICSRF())
	protected.GET("/auth/session", s.adminAPISession)
	protected.POST("/auth/logout", s.adminAPILogout)
	protected.PUT("/auth/password", s.adminAPIChangePassword)

	protected.GET("/accounts", s.adminAPIListAccounts)
	protected.POST("/accounts", s.adminAPICreateAccount)
	protected.GET("/accounts/:id", s.adminAPIGetAccount)
	protected.PUT("/accounts/:id", s.adminAPIUpdateAccount)
	protected.DELETE("/accounts/:id", s.adminAPIDeleteAccount)
	protected.POST("/accounts/:id/sync", s.adminAPISyncAccount)
	protected.POST("/accounts/:id/aliases", s.adminAPICreateAlias)

	protected.GET("/aliases", s.adminAPIListAliases)
	protected.GET("/aliases/:id", s.adminAPIGetAlias)
	protected.POST("/aliases/:id/rotate-key", s.adminAPIRotateAliasKey)
	protected.PATCH("/aliases/:id", s.adminAPIUpdateAlias)
	protected.DELETE("/aliases/:id", s.adminAPIDeleteAlias)

	protected.GET("/audit", s.adminAPIListAuditLogs)
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
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Email          string  `json:"email"`
	IMAPHost       string  `json:"imap_host"`
	IMAPPort       int     `json:"imap_port"`
	IMAPUsername   string  `json:"imap_username"`
	Enabled        bool    `json:"enabled"`
	LastSyncStatus string  `json:"last_sync_status"`
	LastSyncError  string  `json:"last_sync_error"`
	LastSyncedAt   *string `json:"last_synced_at"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	AliasCount     int     `json:"alias_count"`
}

type adminAPIAliasDTO struct {
	ID               int64   `json:"id"`
	AccountID        int64   `json:"account_id"`
	AccountEmail     string  `json:"account_email"`
	Address          string  `json:"address"`
	Label            string  `json:"label"`
	APIKeyPrefix     string  `json:"api_key_prefix"`
	Enabled          bool    `json:"enabled"`
	LastSyncStatus   string  `json:"last_sync_status"`
	LastSyncError    string  `json:"last_sync_error"`
	LastSyncedAt     *string `json:"last_synced_at"`
	LastAccessedAt   *string `json:"last_accessed_at"`
	LatestReceivedAt *string `json:"latest_received_at"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
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
	Account adminAPIAccountDTO `json:"account"`
	Aliases []adminAPIAliasDTO `json:"aliases"`
}

type adminAPIOneTimeKeyDTO struct {
	Alias  adminAPIAliasDTO `json:"alias"`
	APIKey string           `json:"api_key"`
}

type adminAPILoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type adminAPIPasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

type adminAPICreateAccountRequest struct {
	Name         string `json:"name"`
	Email        string `json:"email"`
	IMAPUsername string `json:"imap_username"`
	IMAPPassword string `json:"imap_password"`
}

type adminAPIUpdateAccountRequest struct {
	Name         string `json:"name"`
	Email        string `json:"email"`
	IMAPUsername string `json:"imap_username"`
	IMAPPassword string `json:"imap_password"`
	Enabled      *bool  `json:"enabled"`
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
		ID:             account.ID,
		Name:           account.Name,
		Email:          account.Email,
		IMAPHost:       account.IMAPHost,
		IMAPPort:       account.IMAPPort,
		IMAPUsername:   account.IMAPUsername,
		Enabled:        account.Enabled,
		LastSyncStatus: account.LastSyncStatus,
		LastSyncError:  account.LastSyncError,
		LastSyncedAt:   adminAPIOptionalTime(account.LastSyncedAt),
		CreatedAt:      adminAPITime(account.CreatedAt),
		UpdatedAt:      adminAPITime(account.UpdatedAt),
		AliasCount:     account.AliasCount,
	}
}

func adminAPIAliasFromDomain(alias domain.Alias) adminAPIAliasDTO {
	return adminAPIAliasDTO{
		ID:               alias.ID,
		AccountID:        alias.AccountID,
		AccountEmail:     alias.AccountEmail,
		Address:          alias.Address,
		Label:            alias.Label,
		APIKeyPrefix:     alias.APIKeyPrefix,
		Enabled:          alias.Enabled,
		LastSyncStatus:   alias.LastSyncStatus,
		LastSyncError:    alias.LastSyncError,
		LastSyncedAt:     adminAPIOptionalTime(alias.LastSyncedAt),
		LastAccessedAt:   adminAPIOptionalTime(alias.LastAccessedAt),
		LatestReceivedAt: adminAPIOptionalTime(alias.LatestReceivedAt),
		CreatedAt:        adminAPITime(alias.CreatedAt),
		UpdatedAt:        adminAPITime(alias.UpdatedAt),
	}
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

func adminAPIAccountsFromDomain(accounts []domain.Account) []adminAPIAccountDTO {
	result := make([]adminAPIAccountDTO, 0, len(accounts))
	for _, account := range accounts {
		result = append(result, adminAPIAccountFromDomain(account))
	}
	return result
}

func adminAPIAliasesFromDomain(aliases []domain.Alias) []adminAPIAliasDTO {
	result := make([]adminAPIAliasDTO, 0, len(aliases))
	for _, alias := range aliases {
		result = append(result, adminAPIAliasFromDomain(alias))
	}
	return result
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
			s.clearSessionCookie(c)
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

func (s *Server) adminAPILoginCSRF(c *gin.Context) {
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
		Path:     adminAPILoginCSRFPath,
		MaxAge:   600,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	c.Header("Cache-Control", "no-store")
	writeAdminAPIData(c, http.StatusOK, gin.H{"csrf_token": token, "expires_in": 600})
}

func (s *Server) adminAPILogin(c *gin.Context) {
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
	s.setSessionCookie(c, rawToken)
	s.clearAdminAPILoginCSRFCookie(c)
	s.audit(c, &admin.ID, admin.Username, "login", "admin", "", "success", "")
	writeAdminAPIData(c, http.StatusOK, adminAPISessionFromDomain(session))
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

func (s *Server) clearAdminAPILoginCSRFCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     adminAPILoginCSRFCookie,
		Value:    "",
		Path:     adminAPILoginCSRFPath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) adminAPISession(c *gin.Context) {
	writeAdminAPIData(c, http.StatusOK, adminAPISessionFromDomain(mustSession(c)))
}

func (s *Server) adminAPILogout(c *gin.Context) {
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
	s.clearSessionCookie(c)
	c.Status(http.StatusNoContent)
}

func (s *Server) adminAPIChangePassword(c *gin.Context) {
	var input adminAPIPasswordRequest
	if !decodeAdminAPIJSON(c, &input) {
		return
	}
	session := mustSession(c)
	admin, err := s.store.GetAdminByID(c.Request.Context(), session.AdminID)
	if errors.Is(err, store.ErrNotFound) {
		s.clearSessionCookie(c)
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
			s.clearSessionCookie(c)
			writeAdminAPIError(c, http.StatusConflict, "CREDENTIALS_CHANGED", "登录凭据已在其他请求中更新，请重新登录")
			return
		}
		s.writeAdminAPIInternalError(c, err)
		return
	}
	s.audit(c, &admin.ID, admin.Username, "change_password", "admin", "", "success", "")
	s.clearSessionCookie(c)
	writeAdminAPIData(c, http.StatusOK, gin.H{"reauthentication_required": true})
}

func (s *Server) adminAPIListAccounts(c *gin.Context) {
	accounts, err := s.store.ListAccounts(c.Request.Context())
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	writeAdminAPIData(c, http.StatusOK, adminAPIAccountsFromDomain(accounts))
}

func (s *Server) adminAPICreateAccount(c *gin.Context) {
	var input adminAPICreateAccountRequest
	if !decodeAdminAPIJSON(c, &input) {
		return
	}
	account, password, message := adminAPIAccountInput(input.Name, input.Email, input.IMAPUsername, input.IMAPPassword, domain.Account{Enabled: true})
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
	c.Header("Location", fmt.Sprintf("/admin/api/v1/accounts/%d", created.ID))
	writeAdminAPIData(c, http.StatusCreated, adminAPIAccountFromDomain(created))
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
	existing, err := s.store.GetAccount(c.Request.Context(), id)
	if err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	account, password, message := adminAPIAccountInput(input.Name, input.Email, input.IMAPUsername, input.IMAPPassword, existing)
	account.ID = id
	account.Enabled = *input.Enabled
	if existing.AliasCount > 0 && (account.Email != existing.Email || account.IMAPUsername != existing.IMAPUsername) {
		writeAdminAPIError(c, http.StatusConflict, "ACCOUNT_IDENTITY_LOCKED", "已有隐私邮箱时不能修改主号邮箱或 IMAP 用户名")
		return
	}
	if message != "" {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", message)
		return
	}
	if password != "" {
		account.PasswordCiphertext, err = s.cipher.Encrypt(password)
		if err != nil {
			s.writeAdminAPIInternalError(c, err)
			return
		}
	} else {
		account.PasswordCiphertext = ""
	}
	var updated domain.Account
	err = s.withAccountLock(c.Request.Context(), id, func() error {
		var updateErr error
		updated, updateErr = s.store.UpdateAccount(c.Request.Context(), account)
		return updateErr
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "主号不存在")
		case errors.Is(err, store.ErrAccountIdentityLocked):
			writeAdminAPIError(c, http.StatusConflict, "ACCOUNT_IDENTITY_LOCKED", "已有隐私邮箱时不能修改主号邮箱或 IMAP 用户名")
		case adminAPIUniqueConstraint(err):
			writeAdminAPIError(c, http.StatusConflict, "ACCOUNT_EXISTS", "这个主号已经存在")
		default:
			s.writeAdminAPIInternalError(c, err)
		}
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "update", "account", strconv.FormatInt(id, 10), "success", "")
	writeAdminAPIData(c, http.StatusOK, adminAPIAccountFromDomain(updated))
}

func adminAPIAccountInput(name, email, username, password string, base domain.Account) (domain.Account, string, string) {
	base.Name = strings.TrimSpace(name)
	base.Email = domain.NormalizeEmail(email)
	base.IMAPUsername = strings.TrimSpace(username)
	base.IMAPHost, base.IMAPPort = "imap.mail.me.com", 993
	password = strings.TrimSpace(password)
	switch {
	case utf8.RuneCountInString(base.Name) > 80:
		return base, password, "备注不能超过 80 个字符"
	case validateEmail(base.Email) != nil:
		return base, password, "主号邮箱格式不正确"
	case base.IMAPUsername == "":
		return base, password, "请填写 IMAP 用户名"
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
	if _, err := s.store.GetAccount(c.Request.Context(), id); err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	result := "success"
	var syncErr error
	if s.sync == nil {
		syncErr = errors.New("sync service is unavailable")
	} else {
		syncErr = s.sync(id)
	}
	if syncErr != nil {
		result = "failed"
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "sync", "account", strconv.FormatInt(id, 10), result, "")
	if syncErr != nil {
		writeAdminAPIError(c, http.StatusBadGateway, "SYNC_FAILED", "同步失败，请检查连接状态")
		return
	}
	detail, err := s.adminAPIAccountDetail(c.Request.Context(), id)
	if err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	writeAdminAPIData(c, http.StatusOK, detail)
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
	return adminAPIAccountDetailDTO{
		Account: adminAPIAccountFromDomain(account),
		Aliases: adminAPIAliasesFromDomain(aliases),
	}, nil
}

func (s *Server) adminAPICreateAlias(c *gin.Context) {
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
	rawKey, keyHash, keyPrefix, err := secure.NewAPIKey()
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	var alias domain.Alias
	err = s.withAccountLock(c.Request.Context(), accountID, func() error {
		var createErr error
		alias, createErr = s.store.CreateAlias(c.Request.Context(), domain.Alias{
			AccountID:    accountID,
			Address:      address,
			Label:        label,
			APIKeyHash:   keyHash,
			APIKeyPrefix: keyPrefix,
			Enabled:      true,
		})
		return createErr
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrAliasLimit):
			writeAdminAPIError(c, http.StatusConflict, "ALIAS_LIMIT_REACHED", "此主号最多启用 256 个隐私邮箱")
		case adminAPIUniqueConstraint(err):
			writeAdminAPIError(c, http.StatusConflict, "ALIAS_EXISTS", "这个隐私邮箱已经登记")
		default:
			s.writeAdminAPIInternalError(c, err)
		}
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "create", "alias", strconv.FormatInt(alias.ID, 10), "success", "")
	c.Header("Cache-Control", "no-store")
	c.Header("Location", fmt.Sprintf("/admin/api/v1/aliases/%d", alias.ID))
	writeAdminAPIData(c, http.StatusCreated, adminAPIOneTimeKeyDTO{Alias: adminAPIAliasFromDomain(alias), APIKey: rawKey})
}

func (s *Server) adminAPIListAliases(c *gin.Context) {
	var (
		aliases []domain.Alias
		err     error
	)
	if rawAccountID := strings.TrimSpace(c.Query("account_id")); rawAccountID != "" {
		accountID, parseErr := strconv.ParseInt(rawAccountID, 10, 64)
		if parseErr != nil || accountID < 1 {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "account_id 必须是正整数")
			return
		}
		aliases, err = s.store.ListAliasesByAccount(c.Request.Context(), accountID)
	} else {
		aliases, err = s.store.ListAliases(c.Request.Context())
	}
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	writeAdminAPIData(c, http.StatusOK, adminAPIAliasesFromDomain(aliases))
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
	writeAdminAPIData(c, http.StatusOK, adminAPIAliasFromDomain(alias))
}

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
	alias, err := s.store.RotateAliasAPIKey(c.Request.Context(), id, hash, prefix)
	if err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "rotate_key", "alias", strconv.FormatInt(id, 10), "success", "")
	c.Header("Cache-Control", "no-store")
	writeAdminAPIData(c, http.StatusOK, adminAPIOneTimeKeyDTO{Alias: adminAPIAliasFromDomain(alias), APIKey: rawKey})
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
				writeAdminAPIError(c, http.StatusConflict, "ALIAS_LIMIT_REACHED", "此主号最多启用 256 个隐私邮箱")
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
	writeAdminAPIData(c, http.StatusOK, adminAPIAliasFromDomain(alias))
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
	if err := s.withAccountLock(c.Request.Context(), alias.AccountID, func() error {
		return s.store.DeleteAlias(c.Request.Context(), id)
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "隐私邮箱不存在")
		} else {
			s.writeAdminAPIInternalError(c, err)
		}
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "delete", "alias", strconv.FormatInt(id, 10), "success", "")
	c.Status(http.StatusNoContent)
}

func (s *Server) adminAPIListAuditLogs(c *gin.Context) {
	limit, ok := adminAPIQueryInt(c, "limit", 200, 1, 200)
	if !ok {
		return
	}
	offset, ok := adminAPIQueryInt(c, "offset", 0, 0, 1_000_000)
	if !ok {
		return
	}
	logs, err := s.store.ListAuditLogs(c.Request.Context(), limit+1, offset)
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	hasMore := len(logs) > limit
	if hasMore {
		logs = logs[:limit]
	}
	items := make([]adminAPIAuditLogDTO, 0, len(logs))
	for _, log := range logs {
		items = append(items, adminAPIAuditLogFromDomain(log))
	}
	writeAdminAPIData(c, http.StatusOK, gin.H{
		"items":      items,
		"pagination": gin.H{"limit": limit, "offset": offset, "has_more": hasMore},
	})
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

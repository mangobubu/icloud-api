package httpserver

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

func (s *Server) loginPage(c *gin.Context) {
	if raw, err := c.Cookie(sessionCookie); err == nil && raw != "" {
		if _, err := s.store.GetSessionByHash(c.Request.Context(), secure.HashToken(raw)); err == nil {
			c.Redirect(http.StatusFound, "/admin")
			return
		}
	}
	csrf := s.ensureLoginCSRF(c)
	flash, kind := notice(c.Query("notice"))
	c.HTML(http.StatusOK, "login.html", PageData{Title: "登录", CSRF: csrf, Flash: flash, FlashKind: kind, RequestID: requestID(c)})
}

func (s *Server) login(c *gin.Context) {
	username := strings.TrimSpace(c.PostForm("username"))
	if len(username) == 0 || len(username) > 128 || len(c.PostForm("password")) > 72 {
		s.renderLoginError(c, "", "用户名或密码错误。", http.StatusUnauthorized)
		return
	}
	if !s.validLoginCSRF(c) {
		s.renderLoginError(c, username, "请求校验失败，请刷新页面后重试。", http.StatusForbidden)
		return
	}
	limiterKey := c.ClientIP()
	if !s.loginLimiter.Allow(limiterKey) {
		s.renderLoginError(c, username, "尝试次数过多，请稍后再试。", http.StatusTooManyRequests)
		return
	}

	admin, err := s.store.GetAdminByUsername(c.Request.Context(), username)
	hash := []byte("$2a$12$MmRQk4bYUn7nFdy4xTqIBuUqhgjYuSmkwJtQA4n.IqQvEp4zrC23e")
	if err == nil {
		hash = []byte(admin.PasswordHash)
	}
	passwordOK := bcrypt.CompareHashAndPassword(hash, []byte(c.PostForm("password"))) == nil
	if err != nil || !passwordOK {
		s.audit(c, nil, username, "login", "admin", "", "failed", "")
		s.renderLoginError(c, username, "用户名或密码错误。", http.StatusUnauthorized)
		return
	}
	if _, cleanupErr := s.store.DeleteExpiredSessions(c.Request.Context()); cleanupErr != nil {
		s.logger.Warn("清理过期后台会话失败", "error", cleanupErr, "request_id", requestID(c))
	}

	rawToken, err := secure.RandomToken(32)
	if err != nil {
		s.renderLoginError(c, username, "登录失败，请稍后重试。", http.StatusInternalServerError)
		return
	}
	csrf, err := secure.RandomToken(24)
	if err != nil {
		s.renderLoginError(c, username, "登录失败，请稍后重试。", http.StatusInternalServerError)
		return
	}
	session := domain.Session{AdminID: admin.ID, Username: admin.Username, PasswordVersion: admin.PasswordVersion, CSRF: csrf, ExpiresAt: time.Now().UTC().Add(s.cfg.SessionTTL)}
	if err := s.store.CreateSession(c.Request.Context(), secure.HashToken(rawToken), session); err != nil {
		if errors.Is(err, store.ErrCredentialsChanged) {
			s.audit(c, &admin.ID, admin.Username, "login", "admin", "", "failed", "credentials_changed")
			s.renderLoginError(c, username, "用户名或密码错误。", http.StatusUnauthorized)
			return
		}
		s.logger.Error("创建后台会话失败", "error", err, "request_id", requestID(c))
		s.renderLoginError(c, username, "登录失败，请稍后重试。", http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(c, rawToken)
	s.audit(c, &admin.ID, admin.Username, "login", "admin", "", "success", "")
	c.Redirect(http.StatusFound, "/admin")
}

func (s *Server) logout(c *gin.Context) {
	session := mustSession(c)
	if raw, err := c.Cookie(sessionCookie); err == nil {
		if err := s.store.DeleteSession(c.Request.Context(), secure.HashToken(raw)); err != nil {
			s.audit(c, &session.AdminID, session.Username, "logout", "admin", "", "failed", "")
			s.renderPageError(c, err)
			return
		}
	}
	s.audit(c, &session.AdminID, session.Username, "logout", "admin", "", "success", "")
	s.clearSessionCookie(c)
	c.Redirect(http.StatusFound, "/admin/login")
}

func (s *Server) securityPage(c *gin.Context) {
	data := s.pageData(c, "安全设置", "security")
	data.Subtitle = "管理当前管理员的登录凭据"
	c.HTML(http.StatusOK, "security.html", data)
}

func (s *Server) changePassword(c *gin.Context) {
	session := mustSession(c)
	data := s.pageData(c, "安全设置", "security")
	data.Subtitle = "管理当前管理员的登录凭据"
	admin, err := s.store.GetAdminByID(c.Request.Context(), session.AdminID)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(c.PostForm("current_password"))) != nil {
		data.Flash, data.FlashKind = "当前密码不正确。", "error"
		c.HTML(http.StatusUnauthorized, "security.html", data)
		return
	}
	newPassword := c.PostForm("new_password")
	if len(newPassword) < 12 || len(newPassword) > 72 {
		data.Flash, data.FlashKind = "新密码长度需要在 12 到 72 字节之间。", "error"
		c.HTML(http.StatusBadRequest, "security.html", data)
		return
	}
	if newPassword != c.PostForm("confirm_password") {
		data.Flash, data.FlashKind = "两次填写的新密码不一致。", "error"
		c.HTML(http.StatusBadRequest, "security.html", data)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), 12)
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	if err := s.store.ChangeAdminPasswordAndRevokeSessions(c.Request.Context(), admin.ID, admin.PasswordVersion, string(hash)); err != nil {
		if errors.Is(err, store.ErrCredentialsChanged) {
			data.Flash, data.FlashKind = "登录凭据已在其他请求中更新，请重新登录。", "error"
			c.HTML(http.StatusConflict, "security.html", data)
			return
		}
		s.renderPageError(c, err)
		return
	}
	s.audit(c, &admin.ID, admin.Username, "change_password", "admin", "", "success", "")
	s.clearSessionCookie(c)
	c.Redirect(http.StatusSeeOther, "/admin/login?notice=password_changed")
}

func (s *Server) ensureLoginCSRF(c *gin.Context) string {
	if token, err := c.Cookie(loginCSRFCookie); err == nil && len(token) >= 24 {
		return token
	}
	token, err := secure.RandomToken(24)
	if err != nil {
		return "csrf-unavailable"
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: loginCSRFCookie, Value: token, Path: "/admin/login", MaxAge: 600, HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode})
	return token
}

func (s *Server) validLoginCSRF(c *gin.Context) bool {
	cookie, err := c.Cookie(loginCSRFCookie)
	provided := c.PostForm("csrf_token")
	return err == nil && sameOrigin(c.Request) && len(cookie) == len(provided) && subtle.ConstantTimeCompare([]byte(cookie), []byte(provided)) == 1
}

func (s *Server) renderLoginError(c *gin.Context, username, message string, status int) {
	csrf := s.ensureLoginCSRF(c)
	c.HTML(status, "login.html", PageData{Title: "登录", CSRF: csrf, LoginUsername: username, Flash: message, FlashKind: "error", RequestID: requestID(c)})
}

func (s *Server) setSessionCookie(c *gin.Context, token string) {
	http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Value: token, Path: "/admin", MaxAge: int(s.cfg.SessionTTL.Seconds()), HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode})
}

func (s *Server) clearSessionCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Value: "", Path: "/admin", MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode})
}

func (s *Server) audit(c *gin.Context, adminID *int64, username, action, resourceType, resourceID, result, detail string) {
	entry := domain.AuditLog{AdminID: adminID, Username: username, Action: action, ResourceType: resourceType, ResourceID: resourceID, Result: result, IP: c.ClientIP(), RequestID: requestID(c), Detail: detail, CreatedAt: time.Now().UTC()}
	if _, err := s.store.CreateAuditLog(c.Request.Context(), entry); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("写入操作记录失败", "error", err, "request_id", requestID(c))
	}
}

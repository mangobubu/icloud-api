package httpserver

import (
	"errors"
	"fmt"
	"net/http"
	mailaddr "net/mail"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

func (s *Server) accountsPage(c *gin.Context) {
	accounts, err := s.store.ListAccounts(c.Request.Context())
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	data := s.pageData(c, "主号管理", "accounts")
	data.Subtitle = "配置接收隐私邮箱转发邮件的 iCloud IMAP 主号"
	data.Accounts = accounts
	c.HTML(http.StatusOK, "accounts.html", data)
}

func (s *Server) newAccountPage(c *gin.Context) {
	data := s.pageData(c, "添加主号", "accounts")
	data.Account = domain.Account{IMAPHost: "imap.mail.me.com", IMAPPort: 993, Enabled: true}
	data.FormAction = "/admin/accounts"
	c.HTML(http.StatusOK, "account_form.html", data)
}

func (s *Server) createAccount(c *gin.Context) {
	account, password, validationErr := accountFromForm(c, domain.Account{Enabled: true})
	data := s.pageData(c, "添加主号", "accounts")
	data.Account, data.FormAction = account, "/admin/accounts"
	if validationErr != nil {
		data.Flash, data.FlashKind = validationErr.Error(), "error"
		c.HTML(http.StatusBadRequest, "account_form.html", data)
		return
	}
	encrypted, err := s.cipher.Encrypt(password)
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	account.PasswordCiphertext = encrypted
	created, err := s.store.CreateAccount(c.Request.Context(), account)
	if err != nil {
		data.Flash, data.FlashKind = friendlyStoreError(err, "这个主号已经存在。"), "error"
		c.HTML(http.StatusConflict, "account_form.html", data)
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "create", "account", strconv.FormatInt(created.ID, 10), "success", "")
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/admin/accounts/%d?notice=account_created", created.ID))
}

func (s *Server) accountPage(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	s.renderAccountPage(c, id, http.StatusOK, "", "", "")
}

func (s *Server) editAccountPage(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	account, err := s.store.GetAccount(c.Request.Context(), id)
	if err != nil {
		s.renderNotFoundOrError(c, err)
		return
	}
	data := s.pageData(c, "编辑主号", "accounts")
	data.Account, data.IsEdit = account, true
	data.FormAction = fmt.Sprintf("/admin/accounts/%d", id)
	c.HTML(http.StatusOK, "account_form.html", data)
}

func (s *Server) updateAccount(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	existing, err := s.store.GetAccount(c.Request.Context(), id)
	if err != nil {
		s.renderNotFoundOrError(c, err)
		return
	}
	account, password, validationErr := accountFromForm(c, existing)
	account.ID = id
	account.Enabled = c.PostForm("enabled") == "1"
	if existing.AliasCount > 0 && (account.Email != existing.Email || account.IMAPUsername != existing.IMAPUsername) {
		validationErr = errors.New("已有隐私邮箱时不能修改主号邮箱或 IMAP 用户名，请新建主号后重新登记")
	}
	data := s.pageData(c, "编辑主号", "accounts")
	data.Account, data.IsEdit = account, true
	data.FormAction = fmt.Sprintf("/admin/accounts/%d", id)
	if validationErr != nil {
		data.Flash, data.FlashKind = validationErr.Error(), "error"
		c.HTML(http.StatusBadRequest, "account_form.html", data)
		return
	}
	if password != "" {
		account.PasswordCiphertext, err = s.cipher.Encrypt(password)
		if err != nil {
			s.renderPageError(c, err)
			return
		}
	} else {
		account.PasswordCiphertext = ""
	}
	err = s.withAccountLock(c.Request.Context(), id, func() error {
		_, updateErr := s.store.UpdateAccount(c.Request.Context(), account)
		return updateErr
	})
	if err != nil {
		if errors.Is(err, store.ErrAccountIdentityLocked) {
			data.Flash, data.FlashKind = "已有隐私邮箱时不能修改主号邮箱或 IMAP 用户名，请新建主号后重新登记。", "error"
			c.HTML(http.StatusConflict, "account_form.html", data)
			return
		}
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			data.Flash, data.FlashKind = "这个主号已经存在。", "error"
			c.HTML(http.StatusConflict, "account_form.html", data)
			return
		}
		s.renderPageError(c, err)
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "update", "account", strconv.FormatInt(id, 10), "success", "")
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/admin/accounts/%d?notice=account_updated", id))
}

func (s *Server) syncAccount(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	result := "success"
	noticeCode := "sync_ok"
	if s.sync == nil || s.sync(id) != nil {
		result, noticeCode = "failed", "sync_error"
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "sync", "account", strconv.FormatInt(id, 10), result, "")
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/admin/accounts/%d?notice=%s", id, noticeCode))
}

func (s *Server) deleteAccount(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if err := s.withAccountLock(c.Request.Context(), id, func() error {
		return s.store.DeleteAccount(c.Request.Context(), id)
	}); err != nil {
		s.renderNotFoundOrError(c, err)
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "delete", "account", strconv.FormatInt(id, 10), "success", "")
	c.Redirect(http.StatusSeeOther, "/admin?notice=account_deleted")
}

func (s *Server) createAlias(c *gin.Context) {
	accountID, ok := parseID(c)
	if !ok {
		return
	}
	if _, err := s.store.GetAccount(c.Request.Context(), accountID); err != nil {
		s.renderNotFoundOrError(c, err)
		return
	}
	address := domain.NormalizeEmail(c.PostForm("address"))
	if err := validateEmail(address); err != nil {
		s.renderAccountPage(c, accountID, http.StatusBadRequest, err.Error(), "error", "")
		return
	}
	rawKey, keyHash, keyPrefix, err := secure.NewAPIKey()
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	var alias domain.Alias
	err = s.withAccountLock(c.Request.Context(), accountID, func() error {
		var createErr error
		alias, createErr = s.store.CreateAlias(c.Request.Context(), domain.Alias{AccountID: accountID, Address: address, Label: strings.TrimSpace(c.PostForm("label")), APIKeyHash: keyHash, APIKeyPrefix: keyPrefix, Enabled: true})
		return createErr
	})
	if err != nil {
		if errors.Is(err, store.ErrAliasLimit) {
			s.renderAccountPage(c, accountID, http.StatusConflict, fmt.Sprintf("此主号最多启用 %d 个隐私邮箱。", domain.MaxEnabledAliasesPerAccount), "error", "")
			return
		}
		s.renderAccountPage(c, accountID, http.StatusConflict, friendlyStoreError(err, "这个隐私邮箱已经登记。"), "error", "")
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "create", "alias", strconv.FormatInt(alias.ID, 10), "success", "")
	s.renderAccountPage(c, accountID, http.StatusCreated, "隐私邮箱已添加。", "success", rawKey)
}

func (s *Server) aliasesPage(c *gin.Context) {
	aliases, err := s.store.ListAliases(c.Request.Context())
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	data := s.pageData(c, "隐私邮箱", "aliases")
	data.Subtitle = "查看每个地址的主号归属、Key 状态和最新收件时间"
	data.Aliases = aliases
	c.HTML(http.StatusOK, "aliases.html", data)
}

func (s *Server) rotateAliasKey(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	alias, err := s.store.GetAlias(c.Request.Context(), id)
	if err != nil {
		s.renderNotFoundOrError(c, err)
		return
	}
	rawKey, hash, prefix, err := secure.NewAPIKey()
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	if _, err := s.store.RotateAliasAPIKey(c.Request.Context(), id, hash, prefix); err != nil {
		s.renderPageError(c, err)
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "rotate_key", "alias", strconv.FormatInt(id, 10), "success", "")
	s.renderAccountPage(c, alias.AccountID, http.StatusOK, "API Key 已轮换，旧 Key 已失效。", "success", rawKey)
}

func (s *Server) toggleAlias(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	alias, err := s.store.GetAlias(c.Request.Context(), id)
	if err != nil {
		s.renderNotFoundOrError(c, err)
		return
	}
	err = s.withAccountLock(c.Request.Context(), alias.AccountID, func() error {
		current, getErr := s.store.GetAlias(c.Request.Context(), id)
		if getErr != nil {
			return getErr
		}
		alias = current
		alias.Enabled = !current.Enabled
		return s.store.SetAliasEnabled(c.Request.Context(), id, alias.Enabled)
	})
	if err != nil {
		if errors.Is(err, store.ErrAliasLimit) {
			s.renderAccountPage(c, alias.AccountID, http.StatusConflict, fmt.Sprintf("此主号最多启用 %d 个隐私邮箱。", domain.MaxEnabledAliasesPerAccount), "error", "")
			return
		}
		s.renderPageError(c, err)
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "toggle", "alias", strconv.FormatInt(id, 10), "success", "")
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/admin/accounts/%d?notice=alias_updated", alias.AccountID))
}

func (s *Server) deleteAlias(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	alias, err := s.store.GetAlias(c.Request.Context(), id)
	if err != nil {
		s.renderNotFoundOrError(c, err)
		return
	}
	if err := s.withAccountLock(c.Request.Context(), alias.AccountID, func() error {
		return s.store.DeleteAlias(c.Request.Context(), id)
	}); err != nil {
		s.renderPageError(c, err)
		return
	}
	session := mustSession(c)
	s.audit(c, &session.AdminID, session.Username, "delete", "alias", strconv.FormatInt(id, 10), "success", "")
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/admin/accounts/%d?notice=alias_deleted", alias.AccountID))
}

func (s *Server) auditPage(c *gin.Context) {
	logs, err := s.store.ListAuditLogs(c.Request.Context(), 200, 0)
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	data := s.pageData(c, "操作记录", "audit")
	data.Subtitle = "追踪后台登录与配置变更"
	data.AuditLogs = logs
	c.HTML(http.StatusOK, "audit.html", data)
}

func (s *Server) renderAccountPage(c *gin.Context, accountID int64, status int, flash, kind, secret string) {
	account, err := s.store.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		s.renderNotFoundOrError(c, err)
		return
	}
	aliases, err := s.store.ListAliasesByAccount(c.Request.Context(), accountID)
	if err != nil {
		s.renderPageError(c, err)
		return
	}
	data := s.pageData(c, account.Email, "accounts")
	data.Subtitle = "管理 IMAP 连接、同步状态和所属隐私邮箱"
	data.Account, data.Aliases, data.Secret = account, aliases, secret
	if flash != "" {
		data.Flash, data.FlashKind = flash, kind
	}
	c.HTML(status, "account_detail.html", data)
}

func accountFromForm(c *gin.Context, base domain.Account) (domain.Account, string, error) {
	base.Name = strings.TrimSpace(c.PostForm("name"))
	base.Email = domain.NormalizeEmail(c.PostForm("email"))
	base.IMAPUsername = strings.TrimSpace(c.PostForm("imap_username"))
	base.IMAPHost, base.IMAPPort = "imap.mail.me.com", 993
	password := strings.TrimSpace(c.PostForm("imap_password"))
	if err := validateEmail(base.Email); err != nil {
		return base, password, fmt.Errorf("主号邮箱格式不正确")
	}
	if base.IMAPUsername == "" {
		return base, password, fmt.Errorf("请填写 IMAP 用户名")
	}
	if base.PasswordCiphertext == "" && password == "" {
		return base, password, fmt.Errorf("请填写 App 专用密码")
	}
	return base, password, nil
}

func validateEmail(value string) error {
	parsed, err := mailaddr.ParseAddress(value)
	if err != nil || domain.NormalizeEmail(parsed.Address) != domain.NormalizeEmail(value) || !strings.Contains(value, "@") {
		return errors.New("邮箱地址格式不正确")
	}
	return nil
}

func parseID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		c.String(http.StatusNotFound, "页面不存在")
		return 0, false
	}
	return id, true
}

func friendlyStoreError(err error, conflictMessage string) string {
	if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
		return conflictMessage
	}
	return "保存失败，请检查填写内容后重试。"
}

func (s *Server) renderNotFoundOrError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrNotFound) {
		c.String(http.StatusNotFound, "页面不存在")
		return
	}
	s.renderPageError(c, err)
}

func (s *Server) renderPageError(c *gin.Context, err error) {
	s.logger.Error("后台请求失败", "error", err, "request_id", requestID(c))
	c.String(http.StatusInternalServerError, "请求处理失败，请稍后重试。请求编号：%s", requestID(c))
}

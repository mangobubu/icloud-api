package httpserver

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

const (
	externalAliasAddressField = "add_hide_my_eamil"
	externalAliasAccountField = "icloud"
	externalAliasMaxFormBytes = 4 << 10
)

var (
	errExternalAliasExists           = errors.New("external alias already exists")
	errExternalAliasIdentityConflict = errors.New("external alias conflicts with account identity")
)

type externalAliasResponse struct {
	Alias             string `json:"alias"`
	ICloud            string `json:"icloud"`
	APIKey            string `json:"api_key"`
	MailAPIDirectLink string `json:"mail_api_direct_link"`
}

func (s *Server) oauthTokenAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if !s.externalAPILimiter.Allow(c.ClientIP()) {
			s.writeAPIError(c, http.StatusTooManyRequests, "RATE_LIMITED", "请求过于频繁")
			c.Abort()
			return
		}

		token, validHeader := strictBearerToken(c.Request)
		tokenMatches := secure.HashEqual(s.oauthTokenHash, secure.HashToken(token))
		if !s.oauthTokenConfigured || !validHeader || !tokenMatches {
			c.Header("WWW-Authenticate", "Bearer")
			s.writeAPIError(c, http.StatusUnauthorized, "INVALID_OAUTH_TOKEN", "OAuth 令牌无效")
			c.Abort()
			return
		}
		c.Next()
	}
}

func strictBearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" ||
		strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func (s *Server) createExternalAlias(c *gin.Context) {
	values, ok := s.externalAliasForm(c)
	if !ok {
		return
	}
	address, accountEmail, ok := externalAliasFields(values)
	if !ok {
		s.writeAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", "请求参数必须且只能包含一次 add_hide_my_eamil 和 icloud")
		return
	}
	address = domain.NormalizeEmail(address)
	accountEmail = domain.NormalizeEmail(accountEmail)
	if validateEmail(address) != nil || validateEmail(accountEmail) != nil {
		s.writeAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", "邮箱地址格式不正确")
		return
	}

	account, err := s.store.GetAccountByEmail(c.Request.Context(), accountEmail)
	if err != nil {
		s.writeExternalAccountLookupError(c, err)
		return
	}
	identityConflict, err := s.externalAliasIdentityConflict(c.Request.Context(), address, account)
	if err != nil {
		s.writeExternalStoreError(c, err)
		return
	}
	if identityConflict {
		s.writeAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", "隐私邮箱不能与已登记主号邮箱或所选主号的 IMAP 用户名相同")
		return
	}
	rawKey, keyHash, keyPrefix, err := secure.NewAPIKey()
	if err != nil {
		s.writeExternalInternalError(c, err)
		return
	}

	var alias domain.Alias
	err = s.withAccountLock(c.Request.Context(), account.ID, func() error {
		current, lookupErr := s.store.GetAccountByEmail(c.Request.Context(), accountEmail)
		if lookupErr != nil {
			return lookupErr
		}
		if current.ID != account.ID {
			return store.ErrNotFound
		}
		identityConflict, conflictErr := s.externalAliasIdentityConflict(c.Request.Context(), address, current)
		if conflictErr != nil {
			return conflictErr
		}
		if identityConflict {
			return errExternalAliasIdentityConflict
		}
		if _, duplicateErr := s.store.GetAliasByAddress(c.Request.Context(), address); duplicateErr == nil {
			return errExternalAliasExists
		} else if !errors.Is(duplicateErr, store.ErrNotFound) {
			return duplicateErr
		}
		var createErr error
		alias, createErr = s.store.CreateAlias(c.Request.Context(), domain.Alias{
			AccountID:    account.ID,
			Address:      address,
			APIKeyHash:   keyHash,
			APIKeyPrefix: keyPrefix,
			Enabled:      true,
		})
		return createErr
	})
	if err != nil {
		s.writeExternalAliasCreateError(c, err)
		return
	}

	aliasDTO, err := s.adminAPIAliasFromDomain(alias)
	if err != nil {
		s.writeExternalInternalError(c, err)
		return
	}
	s.audit(c, nil, "oauth_api", "create", "alias", strconv.FormatInt(alias.ID, 10), "success", "")
	c.JSON(http.StatusCreated, gin.H{"data": externalAliasResponse{
		Alias:             alias.Address,
		ICloud:            alias.AccountEmail,
		APIKey:            rawKey,
		MailAPIDirectLink: aliasDTO.DirectLinkPath,
	}})
}

func (s *Server) externalAliasIdentityConflict(ctx context.Context, address string, account domain.Account) (bool, error) {
	if address == domain.NormalizeEmail(account.IMAPUsername) {
		return true, nil
	}
	_, err := s.store.GetAccountByEmail(ctx, address)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

func (s *Server) externalAliasForm(c *gin.Context) (url.Values, bool) {
	r := c.Request
	remainingBodyBytes := int64(externalAliasMaxFormBytes - len(r.URL.RawQuery))
	if remainingBodyBytes < 0 || r.ContentLength > remainingBodyBytes {
		s.writeAPIError(c, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "请求参数不能超过 4 KiB")
		return nil, false
	}
	hasBody := r.Body != nil && r.Body != http.NoBody && (r.ContentLength != 0 || len(r.TransferEncoding) > 0)
	if hasBody {
		mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || !strings.EqualFold(mediaType, "application/x-www-form-urlencoded") {
			s.writeAPIError(c, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "请求正文必须使用 application/x-www-form-urlencoded")
			return nil, false
		}
		r.Body = http.MaxBytesReader(c.Writer, r.Body, remainingBodyBytes)
	}
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeAPIError(c, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "请求参数不能超过 4 KiB")
		} else {
			s.writeAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", "URL 编码参数格式错误")
		}
		return nil, false
	}
	return r.Form, true
}

func externalAliasFields(values url.Values) (string, string, bool) {
	for name := range values {
		if name != externalAliasAddressField && name != externalAliasAccountField {
			return "", "", false
		}
	}
	addresses := values[externalAliasAddressField]
	accounts := values[externalAliasAccountField]
	if len(addresses) != 1 || len(accounts) != 1 ||
		strings.TrimSpace(addresses[0]) == "" || strings.TrimSpace(accounts[0]) == "" {
		return "", "", false
	}
	return addresses[0], accounts[0], true
}

func (s *Server) writeExternalAccountLookupError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.writeAPIError(c, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "主号邮箱不存在")
		return
	}
	s.writeExternalStoreError(c, err)
}

func (s *Server) writeExternalAliasCreateError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errExternalAliasIdentityConflict):
		s.writeAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", "隐私邮箱不能与已登记主号邮箱或所选主号的 IMAP 用户名相同")
	case errors.Is(err, errExternalAliasExists):
		s.writeAPIError(c, http.StatusConflict, "ALIAS_EXISTS", "这个隐私邮箱已经登记")
	case errors.Is(err, store.ErrNotFound):
		s.writeAPIError(c, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "主号邮箱不存在")
	case errors.Is(err, store.ErrAliasLimit):
		s.writeAPIError(c, http.StatusConflict, "ALIAS_LIMIT_REACHED", fmt.Sprintf("此主号最多启用 %d 个隐私邮箱", domain.MaxEnabledAliasesPerAccount))
	case adminAPIUniqueConstraint(err):
		s.writeAPIError(c, http.StatusConflict, "ALIAS_EXISTS", "这个隐私邮箱已经登记")
	default:
		s.writeExternalStoreError(c, err)
	}
}

func (s *Server) writeExternalStoreError(c *gin.Context, err error) {
	s.logger.Error("外部隐私邮箱 API 请求失败", "error", err, "request_id", requestID(c))
	s.writeAPIError(c, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "数据库暂不可用")
}

func (s *Server) writeExternalInternalError(c *gin.Context, err error) {
	s.logger.Error("外部隐私邮箱 API 请求失败", "error", err, "request_id", requestID(c))
	s.writeAPIError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "请求处理失败，请稍后重试")
}

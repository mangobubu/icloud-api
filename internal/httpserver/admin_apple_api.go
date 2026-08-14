package httpserver

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
	"icloud-api/internal/store"
)

// HMESyncService is the Apple web-session and Hide My Email synchronization
// surface used by the admin API.
type HMESyncService interface {
	StartAuth(context.Context, int64, int64, string, string, apple.Region) (hmesync.AuthResult, error)
	VerifyAuth(context.Context, int64, int64, string, string) (hmesync.AuthResult, error)
	GetSession(context.Context, int64) (hmesync.SessionInfo, error)
	ClearAuth(context.Context, int64) error
	SyncAliases(context.Context, int64) (hmesync.SyncResult, error)
	DeleteAlias(context.Context, int64) error
}

type adminAPIAppleAuthRequest struct {
	AppleID  string `json:"apple_id"`
	Password string `json:"password"`
	Region   string `json:"region"`
}

type adminAPIAppleVerifyRequest struct {
	ChallengeID string `json:"challenge_id"`
	Code        string `json:"code"`
}

type adminAPIAppleSessionDTO struct {
	Status          string  `json:"status"`
	AppleID         string  `json:"apple_id"`
	Region          string  `json:"region"`
	AuthenticatedAt *string `json:"authenticated_at"`
	ExpiresAt       *string `json:"expires_at"`
}

type adminAPIAppleAuthResultDTO struct {
	Status       string                   `json:"status"`
	ChallengeID  string                   `json:"challenge_id,omitempty"`
	AppleSession *adminAPIAppleSessionDTO `json:"apple_session"`
}

type adminAPIAppleSyncSummaryDTO struct {
	Total                 int `json:"total"`
	CreatedCount          int `json:"created_count"`
	ExistingCount         int `json:"existing_count"`
	InactiveCount         int `json:"inactive_count"`
	ImportedDisabledCount int `json:"imported_disabled_count"`
	ConflictCount         int `json:"conflict_count"`
	FilteredOutCount      int `json:"filtered_out_count"`
}

type adminAPIAppleCreatedAliasDTO struct {
	Alias             adminAPIAliasDTO `json:"alias"`
	APIKey            string           `json:"api_key"`
	MailAPIDirectLink string           `json:"mail_api_direct_link"`
}

type adminAPIAppleSyncResultDTO struct {
	adminAPIAccountDetailDTO
	Summary adminAPIAppleSyncSummaryDTO    `json:"summary"`
	Created []adminAPIAppleCreatedAliasDTO `json:"created"`
}

func (s *Server) adminAPIStartAppleAuth(c *gin.Context) {
	accountID, ok := adminAPIParseID(c)
	if !ok || !s.adminAPIAppleAccountExists(c, accountID) {
		return
	}

	var input adminAPIAppleAuthRequest
	if !decodeAdminAPIJSON(c, &input) {
		return
	}
	input.AppleID = strings.TrimSpace(input.AppleID)
	input.Region = strings.TrimSpace(input.Region)
	if input.Region == "" {
		input.Region = hmesync.RegionGlobal
	}
	if input.AppleID == "" || len(input.AppleID) > 320 || input.Password == "" || len(input.Password) > 1024 {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "请填写有效的 Apple ID 和密码")
		return
	}
	if input.Region != hmesync.RegionGlobal && input.Region != hmesync.RegionChina {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "Apple 服务区域无效")
		return
	}

	adminSession := mustSession(c)
	if s.hmeSync == nil {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "apple_auth_start", adminAPIAppleServiceUnavailable())
		return
	}
	result, err := s.hmeSync.StartAuth(
		c.Request.Context(), adminSession.AdminID, accountID,
		input.AppleID, input.Password, apple.Region(input.Region),
	)
	input.Password = ""
	if err != nil {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "apple_auth_start", classifyAdminAPIAppleError(err))
		return
	}
	status, valid := adminAPIAppleAuthHTTPStatus(result)
	if !valid {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "apple_auth_start", adminAPIAppleServiceUnavailable())
		return
	}
	s.audit(c, &adminSession.AdminID, adminSession.Username, "apple_auth_start", "account", strconv.FormatInt(accountID, 10), "success", result.Status)
	writeAdminAPIData(c, status, adminAPIAppleAuthResult(result))
}

func (s *Server) adminAPIVerifyAppleAuth(c *gin.Context) {
	accountID, ok := adminAPIParseID(c)
	if !ok || !s.adminAPIAppleAccountExists(c, accountID) {
		return
	}

	var input adminAPIAppleVerifyRequest
	if !decodeAdminAPIJSON(c, &input) {
		return
	}
	input.ChallengeID = strings.TrimSpace(input.ChallengeID)
	input.Code = strings.TrimSpace(input.Code)
	if input.ChallengeID == "" || len(input.ChallengeID) > 256 || !adminAPISixDigitCode(input.Code) {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "验证码请求信息无效")
		return
	}

	adminSession := mustSession(c)
	if s.hmeSync == nil {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "apple_auth_verify", adminAPIAppleServiceUnavailable())
		return
	}
	result, err := s.hmeSync.VerifyAuth(
		c.Request.Context(), adminSession.AdminID, accountID,
		input.ChallengeID, input.Code,
	)
	input.Code = ""
	if err != nil {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "apple_auth_verify", classifyAdminAPIAppleError(err))
		return
	}
	status, valid := adminAPIAppleAuthHTTPStatus(result)
	if !valid || result.Status != hmesync.StatusAuthenticated {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "apple_auth_verify", adminAPIAppleServiceUnavailable())
		return
	}
	s.audit(c, &adminSession.AdminID, adminSession.Username, "apple_auth_verify", "account", strconv.FormatInt(accountID, 10), "success", result.Status)
	writeAdminAPIData(c, status, adminAPIAppleAuthResult(result))
}

func (s *Server) adminAPIClearAppleAuth(c *gin.Context) {
	accountID, ok := adminAPIParseID(c)
	if !ok || !s.adminAPIAppleAccountExists(c, accountID) {
		return
	}
	adminSession := mustSession(c)
	if s.hmeSync == nil {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "apple_session_clear", adminAPIAppleServiceUnavailable())
		return
	}
	if err := s.hmeSync.ClearAuth(c.Request.Context(), accountID); err != nil {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "apple_session_clear", classifyAdminAPIAppleError(err))
		return
	}
	s.audit(c, &adminSession.AdminID, adminSession.Username, "apple_session_clear", "account", strconv.FormatInt(accountID, 10), "success", "")
	c.Status(http.StatusNoContent)
}

func (s *Server) adminAPISyncAppleAliases(c *gin.Context) {
	accountID, ok := adminAPIParseID(c)
	if !ok || !s.adminAPIAppleAccountExists(c, accountID) {
		return
	}
	adminSession := mustSession(c)
	if s.hmeSync == nil {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "sync_hme_aliases", adminAPIAppleServiceUnavailable())
		return
	}
	detail, err := s.adminAPIAccountDetail(c.Request.Context(), accountID)
	if err != nil {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "sync_hme_aliases", classifyAdminAPIAppleError(err))
		return
	}
	result, err := s.hmeSync.SyncAliases(c.Request.Context(), accountID)
	if err != nil {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "sync_hme_aliases", classifyAdminAPIAppleError(err))
		return
	}
	created, err := s.adminAPIAppleCreatedAliases(result.Created)
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	if refreshed, refreshErr := s.adminAPIAccountDetail(c.Request.Context(), accountID); refreshErr == nil {
		detail = refreshed
	} else {
		s.logger.Warn("Apple 隐私邮箱同步成功后刷新账户详情失败，使用同步前快照",
			"request_id", requestID(c),
		)
	}
	detail.Aliases = adminAPIMergeCreatedAliases(detail.Aliases, created)
	detail.AppleSession = adminAPIAppleSessionFromInfo(result.Session)
	detail.Account.AliasCount = len(detail.Aliases)
	summary := adminAPIAppleSyncSummary(result.Summary)
	auditDetail := "total=" + strconv.Itoa(summary.Total) +
		" created=" + strconv.Itoa(summary.CreatedCount) +
		" existing=" + strconv.Itoa(summary.ExistingCount) +
		" inactive=" + strconv.Itoa(summary.InactiveCount) +
		" imported_disabled=" + strconv.Itoa(summary.ImportedDisabledCount) +
		" filtered=" + strconv.Itoa(summary.FilteredOutCount)
	s.audit(c, &adminSession.AdminID, adminSession.Username, "sync_hme_aliases", "account", strconv.FormatInt(accountID, 10), "success", auditDetail)
	writeAdminAPIData(c, http.StatusOK, adminAPIAppleSyncResultDTO{
		adminAPIAccountDetailDTO: detail,
		Summary:                  summary,
		Created:                  created,
	})
}

func (s *Server) adminAPIAppleAccountExists(c *gin.Context, accountID int64) bool {
	if _, err := s.store.GetAccount(c.Request.Context(), accountID); err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return false
	}
	return true
}

func (s *Server) adminAPIAppleSession(ctx context.Context, accountID int64) (*adminAPIAppleSessionDTO, error) {
	if s.hmeSync == nil {
		return nil, nil
	}
	info, err := s.hmeSync.GetSession(ctx, accountID)
	if err == nil {
		return adminAPIAppleSessionFromInfo(info), nil
	}
	if errors.Is(err, hmesync.ErrLoginRequired) {
		return &adminAPIAppleSessionDTO{Status: hmesync.StatusLoginRequired}, nil
	}
	if errors.Is(err, hmesync.ErrSessionExpired) {
		return &adminAPIAppleSessionDTO{Status: hmesync.StatusExpired}, nil
	}
	return nil, err
}

func adminAPIAppleSessionFromInfo(info hmesync.SessionInfo) *adminAPIAppleSessionDTO {
	status := info.Status
	if status == "" {
		status = hmesync.StatusLoginRequired
	}
	return &adminAPIAppleSessionDTO{
		Status:          status,
		AppleID:         info.AppleID,
		Region:          info.Region,
		AuthenticatedAt: adminAPIOptionalTime(info.AuthenticatedAt),
		ExpiresAt:       adminAPIOptionalTime(info.ExpiresAt),
	}
}

func adminAPIAppleAuthResult(result hmesync.AuthResult) adminAPIAppleAuthResultDTO {
	result.Session.Status = result.Status
	return adminAPIAppleAuthResultDTO{
		Status:       result.Status,
		ChallengeID:  result.ChallengeID,
		AppleSession: adminAPIAppleSessionFromInfo(result.Session),
	}
}

func adminAPIAppleAuthHTTPStatus(result hmesync.AuthResult) (int, bool) {
	switch result.Status {
	case hmesync.StatusAuthenticated:
		return http.StatusOK, true
	case hmesync.StatusVerificationRequired:
		return http.StatusAccepted, result.ChallengeID != ""
	default:
		return 0, false
	}
}

func adminAPISixDigitCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for index := range code {
		if code[index] < '0' || code[index] > '9' {
			return false
		}
	}
	return true
}

func adminAPIAppleSyncSummary(summary hmesync.SyncSummary) adminAPIAppleSyncSummaryDTO {
	return adminAPIAppleSyncSummaryDTO{
		Total:                 summary.Total,
		CreatedCount:          summary.CreatedCount,
		ExistingCount:         summary.ExistingCount,
		InactiveCount:         summary.InactiveCount,
		ImportedDisabledCount: summary.ImportedDisabledCount,
		ConflictCount:         summary.ConflictCount,
		FilteredOutCount:      summary.FilteredOutCount,
	}
}

func (s *Server) adminAPIAppleCreatedAliases(created []hmesync.CreatedAlias) ([]adminAPIAppleCreatedAliasDTO, error) {
	result := make([]adminAPIAppleCreatedAliasDTO, 0, len(created))
	for _, item := range created {
		alias, err := s.adminAPIAliasFromDomain(item.Alias)
		if err != nil {
			return nil, err
		}
		apiKey := strings.TrimSpace(item.APIKey)
		if apiKey == "" {
			apiKey = alias.APIKey
		}
		result = append(result, adminAPIAppleCreatedAliasDTO{
			Alias:             alias,
			APIKey:            apiKey,
			MailAPIDirectLink: alias.DirectLinkPath,
		})
	}
	return result, nil
}

func adminAPIMergeCreatedAliases(existing []adminAPIAliasDTO, created []adminAPIAppleCreatedAliasDTO) []adminAPIAliasDTO {
	createdIDs := make(map[int64]struct{}, len(created))
	createdAddresses := make(map[string]struct{}, len(created))
	for _, item := range created {
		createdIDs[item.Alias.ID] = struct{}{}
		createdAddresses[domain.NormalizeEmail(item.Alias.Address)] = struct{}{}
	}

	result := make([]adminAPIAliasDTO, 0, len(existing)+len(created))
	seenIDs := make(map[int64]struct{}, len(existing)+len(created))
	seenAddresses := make(map[string]struct{}, len(existing)+len(created))
	for _, alias := range existing {
		address := domain.NormalizeEmail(alias.Address)
		if _, replaced := createdIDs[alias.ID]; replaced {
			continue
		}
		if _, replaced := createdAddresses[address]; replaced {
			continue
		}
		if _, duplicate := seenIDs[alias.ID]; duplicate {
			continue
		}
		if _, duplicate := seenAddresses[address]; duplicate {
			continue
		}
		seenIDs[alias.ID] = struct{}{}
		seenAddresses[address] = struct{}{}
		result = append(result, alias)
	}
	for _, item := range created {
		address := domain.NormalizeEmail(item.Alias.Address)
		if _, duplicate := seenIDs[item.Alias.ID]; duplicate {
			continue
		}
		if _, duplicate := seenAddresses[address]; duplicate {
			continue
		}
		seenIDs[item.Alias.ID] = struct{}{}
		seenAddresses[address] = struct{}{}
		result = append(result, item.Alias)
	}
	sort.SliceStable(result, func(left, right int) bool {
		leftAddress := domain.NormalizeEmail(result[left].Address)
		rightAddress := domain.NormalizeEmail(result[right].Address)
		if leftAddress == rightAddress {
			return result[left].ID < result[right].ID
		}
		return leftAddress < rightAddress
	})
	return result
}

type adminAPIAppleError struct {
	Status  int
	Code    string
	Message string
}

func adminAPIAppleServiceUnavailable() adminAPIAppleError {
	return adminAPIAppleError{
		Status:  http.StatusServiceUnavailable,
		Code:    "INTERNAL_ERROR",
		Message: "Apple 同步服务暂时不可用，请稍后再试",
	}
}

func classifyAdminAPIAppleError(err error) adminAPIAppleError {
	if errors.Is(err, context.DeadlineExceeded) {
		return adminAPIAppleError{Status: http.StatusGatewayTimeout, Code: hmesync.CodeUpstreamError, Message: "Apple 服务响应超时，请稍后再试"}
	}
	code := hmesync.Code(err)
	if code == "" {
		switch {
		case errors.Is(err, hmesync.ErrLoginRequired):
			code = hmesync.CodeLoginRequired
		case errors.Is(err, hmesync.ErrSessionExpired):
			code = hmesync.CodeSessionExpired
		case errors.Is(err, hmesync.ErrCredentialsInvalid):
			code = hmesync.CodeCredentialsInvalid
		case errors.Is(err, hmesync.ErrVerificationInvalid):
			code = hmesync.CodeVerificationInvalid
		case errors.Is(err, hmesync.ErrFlowExpired):
			code = hmesync.CodeFlowExpired
		case errors.Is(err, hmesync.ErrAccountActionRequired):
			code = hmesync.CodeAccountActionRequired
		case errors.Is(err, hmesync.ErrRateLimited):
			code = hmesync.CodeRateLimited
		case errors.Is(err, hmesync.ErrAccountMismatch):
			code = hmesync.CodeAccountMismatch
		case errors.Is(err, hmesync.ErrAccountChanged):
			code = hmesync.CodeAccountChanged
		case errors.Is(err, hmesync.ErrAliasOwnershipConflict), errors.Is(err, store.ErrAliasOwnershipConflict):
			code = hmesync.CodeAliasOwnershipConflict
		case errors.Is(err, hmesync.ErrUpstream):
			code = hmesync.CodeUpstreamError
		case errors.Is(err, store.ErrNotFound):
			return adminAPIAppleError{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "主号不存在"}
		default:
			return adminAPIAppleError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "请求处理失败，请稍后重试"}
		}
	}
	switch code {
	case hmesync.CodeLoginRequired:
		return adminAPIAppleError{Status: http.StatusConflict, Code: code, Message: "请先连接 Apple 账户"}
	case hmesync.CodeSessionExpired:
		return adminAPIAppleError{Status: http.StatusConflict, Code: code, Message: "Apple 会话已过期，请重新登录"}
	case hmesync.CodeCredentialsInvalid:
		return adminAPIAppleError{Status: http.StatusUnprocessableEntity, Code: code, Message: "Apple ID 或密码不正确"}
	case hmesync.CodeVerificationInvalid:
		return adminAPIAppleError{Status: http.StatusUnprocessableEntity, Code: code, Message: "验证码不正确，请重新输入"}
	case hmesync.CodeFlowExpired:
		return adminAPIAppleError{Status: http.StatusGone, Code: code, Message: "验证流程已过期，请重新登录"}
	case hmesync.CodeAccountActionRequired:
		return adminAPIAppleError{Status: http.StatusConflict, Code: code, Message: "Apple 账户需要完成条款确认或其他账户操作，请前往 Apple 官网处理后重试"}
	case hmesync.CodeRateLimited:
		return adminAPIAppleError{Status: http.StatusTooManyRequests, Code: code, Message: "Apple 请求过于频繁，请稍后再试"}
	case hmesync.CodeAccountMismatch:
		return adminAPIAppleError{Status: http.StatusConflict, Code: code, Message: "Apple 登录账户或转发邮箱与该主号不匹配"}
	case hmesync.CodeAccountChanged:
		return adminAPIAppleError{Status: http.StatusConflict, Code: code, Message: "主号信息已发生变化，请重新操作"}
	case hmesync.CodeAliasOwnershipConflict:
		return adminAPIAppleError{Status: http.StatusConflict, Code: code, Message: "隐私邮箱已属于其他主号"}
	default:
		return adminAPIAppleError{Status: http.StatusBadGateway, Code: hmesync.CodeUpstreamError, Message: "Apple 服务暂时异常，请稍后再试"}
	}
}

func (s *Server) adminAPIFinishAppleFailure(
	c *gin.Context,
	adminSession domain.Session,
	accountID int64,
	action string,
	apiErr adminAPIAppleError,
) {
	s.logger.Warn("Apple 管理操作失败",
		"action", action,
		"code", apiErr.Code,
		"request_id", requestID(c),
	)
	s.audit(c, &adminSession.AdminID, adminSession.Username, action, "account", strconv.FormatInt(accountID, 10), "failed", apiErr.Code)
	writeAdminAPIError(c, apiErr.Status, apiErr.Code, apiErr.Message)
}

func (s *Server) auditAppleAliasDeleteFailure(
	c *gin.Context,
	adminSession domain.Session,
	aliasID int64,
	apiErr adminAPIAppleError,
) {
	s.logger.Warn("Apple alias deletion failed",
		"action", "delete",
		"code", apiErr.Code,
		"request_id", requestID(c),
	)
	s.audit(c, &adminSession.AdminID, adminSession.Username, "delete", "alias", strconv.FormatInt(aliasID, 10), "failed", apiErr.Code)
}

func (s *Server) adminAPIFinishAppleAliasDeleteFailure(
	c *gin.Context,
	adminSession domain.Session,
	aliasID int64,
	apiErr adminAPIAppleError,
) {
	apiErr = adminAPIAppleAliasDeleteFailure(apiErr)
	s.auditAppleAliasDeleteFailure(c, adminSession, aliasID, apiErr)
	writeAdminAPIError(c, apiErr.Status, apiErr.Code, apiErr.Message)
}

func adminAPIAppleAliasDeleteFailure(apiErr adminAPIAppleError) adminAPIAppleError {
	message := strings.TrimRight(strings.TrimSpace(apiErr.Message), "。")
	if !strings.Contains(message, "本地记录已保留") {
		message += "；本地记录已保留，可稍后重试"
	}
	apiErr.Message = message
	return apiErr
}

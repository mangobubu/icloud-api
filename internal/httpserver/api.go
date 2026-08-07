package httpserver

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/domain"
)

const recentMailWindow = time.Hour

type recentMailResponse struct {
	Email   string `json:"email"`
	Content string `json:"content"`
	Time    string `json:"time"`
}

func (s *Server) availableMailboxSnapshot(c *gin.Context) (domain.MailboxBinding, time.Time, bool) {
	binding := mustBinding(c)
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	if err := s.store.TouchAliasAccess(c.Request.Context(), binding.Alias.ID, now); err != nil {
		s.logger.Warn("更新 API 最近访问时间失败", "alias_id", binding.Alias.ID, "error", err, "request_id", requestID(c))
	}
	staleAfter := 3 * s.cfg.PollInterval
	if binding.Alias.LastSyncStatus != domain.SyncStatusOK || binding.Alias.LastSyncedAt == nil ||
		binding.Alias.LastSyncedAt.After(now) || now.Sub(*binding.Alias.LastSyncedAt) > staleAfter {
		s.writeAPIError(c, http.StatusServiceUnavailable, "SYNC_UNAVAILABLE", "邮箱同步暂不可用")
		return domain.MailboxBinding{}, time.Time{}, false
	}
	return binding, now, true
}

func (s *Server) latestMail(c *gin.Context) {
	binding, _, ok := s.availableMailboxSnapshot(c)
	if !ok {
		return
	}
	if binding.Message == nil {
		s.writeAPIError(c, http.StatusNotFound, "MAIL_NOT_FOUND", "尚未收到邮件")
		return
	}
	message := binding.Message

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"alias": binding.Alias.Address,
			"message": gin.H{
				"id":              fmt.Sprintf("%d-%d", message.UIDValidity, message.UID),
				"message_id":      message.MessageID,
				"received_at":     message.InternalDate.UTC().Format(time.RFC3339),
				"sent_at":         formatRFC3339(message.HeaderDate),
				"from":            message.From,
				"to":              message.To,
				"cc":              message.CC,
				"subject":         message.Subject,
				"text":            message.TextBody,
				"html":            message.HTMLBody,
				"attachments":     message.Attachments,
				"has_attachments": len(message.Attachments) > 0,
				"body_truncated":  message.BodyTruncated,
			},
			"synced_at": message.SyncedAt.UTC().Format(time.RFC3339),
			"stale":     false,
		},
	})
}

func (s *Server) recentMail(c *gin.Context) {
	binding, now, ok := s.availableMailboxSnapshot(c)
	if !ok {
		return
	}
	message := binding.Message
	if message == nil || message.InternalDate.IsZero() ||
		message.InternalDate.Before(now.Add(-recentMailWindow)) || message.InternalDate.After(now) {
		s.writeAPIError(c, http.StatusNotFound, "MAIL_NOT_FOUND", "最近一小时内没有邮件")
		return
	}

	content := message.TextBody
	if strings.TrimSpace(content) == "" {
		content = plainTextFromHTML(message.HTMLBody)
	}
	location := s.cfg.Timezone
	if location == nil {
		location = time.Local
	}
	c.JSON(http.StatusOK, recentMailResponse{
		Email:   binding.Alias.Address,
		Content: content,
		Time:    message.InternalDate.In(location).Format(time.RFC3339),
	})
}

func formatRFC3339(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339)
}

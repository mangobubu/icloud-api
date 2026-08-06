package httpserver

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) latestMail(c *gin.Context) {
	binding := mustBinding(c)
	now := time.Now().UTC()
	if err := s.store.TouchAliasAccess(c.Request.Context(), binding.Alias.ID, now); err != nil {
		s.logger.Warn("更新 API 最近访问时间失败", "alias_id", binding.Alias.ID, "error", err, "request_id", requestID(c))
	}
	staleAfter := 3 * s.cfg.PollInterval
	if binding.Alias.LastSyncStatus != "ok" || binding.Alias.LastSyncedAt == nil || now.Sub(*binding.Alias.LastSyncedAt) > staleAfter {
		s.writeAPIError(c, http.StatusServiceUnavailable, "SYNC_UNAVAILABLE", "邮箱同步暂不可用")
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

func formatRFC3339(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339)
}

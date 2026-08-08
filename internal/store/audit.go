package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

type AuditLogFilter struct {
	AdminID      *int64
	Action       string
	ResourceType string
	Result       string
	Since        *time.Time
	Until        *time.Time
	Limit        int
	Offset       int
}

func (s *Store) CreateAuditLog(ctx context.Context, log domain.AuditLog) (domain.AuditLog, error) {
	log.Username = truncate(log.Username, 128)
	log.Action = truncate(log.Action, 64)
	log.ResourceType = truncate(log.ResourceType, 64)
	log.ResourceID = truncate(log.ResourceID, 128)
	log.Result = truncate(log.Result, 32)
	log.IP = truncate(log.IP, 64)
	log.RequestID = truncate(log.RequestID, 128)
	log.Detail = truncate(log.Detail, 1024)
	if log.CreatedAt.IsZero() {
		log.CreatedAt = s.now()
	}
	var id int64
	err := s.queryRowContext(ctx, `
		INSERT INTO audit_logs(
			admin_id, username, action, resource_type, resource_id,
			result, ip, request_id, detail, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		log.AdminID, log.Username, log.Action, log.ResourceType, log.ResourceID,
		log.Result, log.IP, log.RequestID, log.Detail, timestamp(log.CreatedAt),
	).Scan(&id)
	if err != nil {
		return domain.AuditLog{}, fmt.Errorf("create audit log: %w", err)
	}
	log.ID = id
	return log, nil
}

func truncate(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	value = sanitizePostgresText(value)
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

func (s *Store) AppendAuditLog(ctx context.Context, log domain.AuditLog) error {
	_, err := s.CreateAuditLog(ctx, log)
	return err
}

func (s *Store) ListAuditLogs(ctx context.Context, limit, offset int) ([]domain.AuditLog, error) {
	return s.ListAuditLogsFiltered(ctx, AuditLogFilter{Limit: limit, Offset: offset})
}

func (s *Store) ListAuditLogsFiltered(ctx context.Context, filter AuditLogFilter) ([]domain.AuditLog, error) {
	filter.Action = sanitizePostgresText(filter.Action)
	filter.ResourceType = sanitizePostgresText(filter.ResourceType)
	filter.Result = sanitizePostgresText(filter.Result)
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	if filter.Limit > 1000 {
		filter.Limit = 1000
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	query := strings.Builder{}
	query.WriteString(`SELECT
		id, admin_id, username, action, resource_type, resource_id,
		result, ip, request_id, detail, created_at
		FROM audit_logs`)
	var conditions []string
	var args []any
	if filter.AdminID != nil {
		conditions = append(conditions, "admin_id = ?")
		args = append(args, *filter.AdminID)
	}
	if filter.Action != "" {
		conditions = append(conditions, "action = ?")
		args = append(args, filter.Action)
	}
	if filter.ResourceType != "" {
		conditions = append(conditions, "resource_type = ?")
		args = append(args, filter.ResourceType)
	}
	if filter.Result != "" {
		conditions = append(conditions, "result = ?")
		args = append(args, filter.Result)
	}
	if filter.Since != nil {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, timestamp(*filter.Since))
	}
	if filter.Until != nil {
		conditions = append(conditions, "created_at < ?")
		args = append(args, timestamp(*filter.Until))
	}
	if len(conditions) > 0 {
		query.WriteString(" WHERE ")
		query.WriteString(strings.Join(conditions, " AND "))
	}
	query.WriteString(" ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?")
	args = append(args, filter.Limit, filter.Offset)

	rows, err := s.queryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("list audit logs: %w", err)
	}
	defer rows.Close()

	var logs []domain.AuditLog
	for rows.Next() {
		var log domain.AuditLog
		var adminID sql.NullInt64
		var createdAt int64
		if err := rows.Scan(
			&log.ID, &adminID, &log.Username, &log.Action, &log.ResourceType,
			&log.ResourceID, &log.Result, &log.IP, &log.RequestID, &log.Detail, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan audit log: %w", err)
		}
		if adminID.Valid {
			value := adminID.Int64
			log.AdminID = &value
		}
		log.CreatedAt = timeFromTimestamp(createdAt)
		logs = append(logs, log)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit logs: %w", err)
	}
	return logs, nil
}

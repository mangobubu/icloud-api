package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

// GetAliasCreationSchedule returns the persisted plan. A missing row means the
// account has never opted in and is intentionally represented by ErrNotFound;
// callers that expose settings can map that to a disabled default.
func (s *Store) GetAliasCreationSchedule(ctx context.Context, accountID int64) (domain.AliasCreationSchedule, error) {
	var schedule domain.AliasCreationSchedule
	var enabled bool
	var plannedJSON string
	var nextRun, lastAttempted, lastCreated sql.NullInt64
	var createdAt, updatedAt int64
	err := s.queryRowContext(ctx, `
		SELECT account_id, enabled, planned_at_json, next_run_at,
		       last_attempted_at, last_created_at, last_alias_address, last_error,
		       created_at, updated_at
		FROM alias_creation_schedules WHERE account_id = ?`, accountID).Scan(
		&schedule.AccountID, &enabled, &plannedJSON, &nextRun, &lastAttempted,
		&lastCreated, &schedule.LastAliasAddress, &schedule.LastError,
		&createdAt, &updatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.AliasCreationSchedule{}, ErrNotFound
		}
		return domain.AliasCreationSchedule{}, fmt.Errorf("read alias creation schedule: %w", err)
	}
	schedule.Enabled = enabled
	schedule.PlannedAt, err = decodePlannedAliasCreationTimes(plannedJSON)
	if err != nil {
		return domain.AliasCreationSchedule{}, fmt.Errorf("decode alias creation schedule: %w", err)
	}
	schedule.NextRunAt = timePtr(nextRun)
	schedule.LastAttemptedAt = timePtr(lastAttempted)
	schedule.LastCreatedAt = timePtr(lastCreated)
	schedule.CreatedAt = timeFromTimestamp(createdAt)
	schedule.UpdatedAt = timeFromTimestamp(updatedAt)
	return schedule, nil
}

func (s *Store) ListDueAliasCreationSchedules(ctx context.Context, dueAt time.Time) ([]domain.AliasCreationSchedule, error) {
	dueAt = dueAt.UTC()
	rows, err := s.queryContext(ctx, `
		SELECT account_id, enabled, planned_at_json, next_run_at,
		       last_attempted_at, last_created_at, last_alias_address, last_error,
		       created_at, updated_at
		FROM alias_creation_schedules
		WHERE enabled = TRUE AND next_run_at IS NOT NULL AND next_run_at <= ?
		ORDER BY next_run_at, account_id`, timestamp(dueAt))
	if err != nil {
		return nil, fmt.Errorf("list due alias creation schedules: %w", err)
	}
	defer rows.Close()
	result := make([]domain.AliasCreationSchedule, 0)
	for rows.Next() {
		schedule, err := scanAliasCreationSchedule(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, schedule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due alias creation schedules: %w", err)
	}
	return result, nil
}

// EnableAliasCreation atomically replaces the current cycle. The schedule row
// is separate from accounts.updated_at so IMAP compare-and-swap versions are
// unaffected by routine scheduler ticks.
func (s *Store) EnableAliasCreation(ctx context.Context, accountID int64, planned []time.Time, now time.Time) error {
	if accountID < 1 {
		return fmt.Errorf("enable alias creation: account id must be positive")
	}
	plannedJSON, err := encodePlannedAliasCreationTimes(planned)
	if err != nil {
		return fmt.Errorf("enable alias creation: %w", err)
	}
	now = now.UTC()
	var next any
	if len(planned) > 0 {
		next = timestamp(planned[0])
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin enable alias creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.lockAccountVersionForUpdate(ctx, tx, accountID); err != nil {
		return fmt.Errorf("lock account for enabling alias creation: %w", err)
	}
	var accountEnabled bool
	if err := s.txQueryRowContext(ctx, tx,
		`SELECT enabled FROM accounts WHERE id = ?`, accountID,
	).Scan(&accountEnabled); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("read account state while enabling alias creation: %w", err)
	}
	if !accountEnabled {
		return ErrAccountDisabled
	}
	_, err = s.txExecContext(ctx, tx, `
		INSERT INTO alias_creation_schedules(
			account_id, enabled, planned_at_json, next_run_at,
			last_attempted_at, last_created_at, last_alias_address, last_error,
			created_at, updated_at
		) VALUES(?, TRUE, ?, ?, NULL, NULL, '', '', ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			enabled = TRUE,
			planned_at_json = excluded.planned_at_json,
			next_run_at = excluded.next_run_at,
			updated_at = excluded.updated_at`,
		accountID, plannedJSON, next, timestamp(now), timestamp(now))
	if err != nil {
		return fmt.Errorf("write alias creation schedule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit alias creation enable: %w", err)
	}
	return nil
}

func (s *Store) DisableAliasCreation(ctx context.Context, accountID int64, now time.Time) error {
	if accountID < 1 {
		return fmt.Errorf("disable alias creation: account id must be positive")
	}
	result, err := s.execContext(ctx, `
		UPDATE alias_creation_schedules
		SET enabled = FALSE, planned_at_json = '[]', next_run_at = NULL, updated_at = ?
		WHERE account_id = ?`, timestamp(now), accountID)
	if err != nil {
		return fmt.Errorf("disable alias creation: %w", err)
	}
	// Disabling an account that has never opted in is deliberately idempotent.
	_ = result
	return nil
}

func (s *Store) RescheduleAliasCreation(ctx context.Context, accountID int64, expectedNext time.Time, planned []time.Time, now time.Time) (bool, error) {
	return s.casAliasCreationPlan(ctx, accountID, expectedNext, planned, now, false)
}

func (s *Store) ClaimAliasCreation(ctx context.Context, accountID int64, expectedNext time.Time, planned []time.Time, attemptedAt time.Time) (bool, error) {
	return s.casAliasCreationPlan(ctx, accountID, expectedNext, planned, attemptedAt, true)
}

func (s *Store) casAliasCreationPlan(ctx context.Context, accountID int64, expectedNext time.Time, planned []time.Time, at time.Time, claim bool) (bool, error) {
	plannedJSON, err := encodePlannedAliasCreationTimes(planned)
	if err != nil {
		return false, err
	}
	var next any
	if len(planned) > 0 {
		next = timestamp(planned[0])
	}
	query := `
		UPDATE alias_creation_schedules
		SET planned_at_json = ?, next_run_at = ?, updated_at = ?`
	args := []any{plannedJSON, next, timestamp(at.UTC())}
	if claim {
		query += `, last_attempted_at = ?`
		args = append(args, timestamp(at.UTC()))
	}
	query += ` WHERE account_id = ? AND enabled = TRUE AND `
	args = append(args, accountID)
	if expectedNext.IsZero() {
		query += `next_run_at IS NULL`
	} else {
		query += `next_run_at = ?`
		args = append(args, timestamp(expectedNext.UTC()))
	}
	result, err := s.execContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("compare-and-swap alias creation schedule: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read alias creation schedule CAS result: %w", err)
	}
	return count == 1, nil
}

func (s *Store) RecordAliasCreationSuccess(ctx context.Context, accountID int64, attemptedAt time.Time, address string) error {
	result, err := s.execContext(ctx, `
		UPDATE alias_creation_schedules
		SET last_attempted_at = ?, last_created_at = ?, last_alias_address = ?, last_error = '', updated_at = ?
		WHERE account_id = ? AND enabled = TRUE`,
		timestamp(attemptedAt), timestamp(attemptedAt), strings.TrimSpace(sanitizePostgresText(address)),
		timestamp(s.now()), accountID)
	if err != nil {
		return fmt.Errorf("record alias creation success: %w", err)
	}
	return requireAffected(result, "alias creation schedule")
}

func (s *Store) RecordAliasCreationFailure(ctx context.Context, accountID int64, attemptedAt time.Time, message string) error {
	message = strings.TrimSpace(sanitizePostgresText(message))
	if len([]rune(message)) > 240 {
		message = string([]rune(message)[:240])
	}
	result, err := s.execContext(ctx, `
		UPDATE alias_creation_schedules
		SET last_attempted_at = ?, last_error = ?, updated_at = ?
		WHERE account_id = ? AND enabled = TRUE`,
		timestamp(attemptedAt), message, timestamp(s.now()), accountID)
	if err != nil {
		return fmt.Errorf("record alias creation failure: %w", err)
	}
	return requireAffected(result, "alias creation schedule")
}

func scanAliasCreationSchedule(scanner rowScanner) (domain.AliasCreationSchedule, error) {
	var schedule domain.AliasCreationSchedule
	var enabled bool
	var plannedJSON string
	var nextRun, lastAttempted, lastCreated sql.NullInt64
	var createdAt, updatedAt int64
	if err := scanner.Scan(
		&schedule.AccountID, &enabled, &plannedJSON, &nextRun, &lastAttempted,
		&lastCreated, &schedule.LastAliasAddress, &schedule.LastError,
		&createdAt, &updatedAt,
	); err != nil {
		return domain.AliasCreationSchedule{}, fmt.Errorf("scan alias creation schedule: %w", err)
	}
	schedule.Enabled = enabled
	var err error
	schedule.PlannedAt, err = decodePlannedAliasCreationTimes(plannedJSON)
	if err != nil {
		return domain.AliasCreationSchedule{}, fmt.Errorf("decode alias creation schedule: %w", err)
	}
	schedule.NextRunAt = timePtr(nextRun)
	schedule.LastAttemptedAt = timePtr(lastAttempted)
	schedule.LastCreatedAt = timePtr(lastCreated)
	schedule.CreatedAt = timeFromTimestamp(createdAt)
	schedule.UpdatedAt = timeFromTimestamp(updatedAt)
	return schedule, nil
}

func encodePlannedAliasCreationTimes(values []time.Time) (string, error) {
	encoded := make([]string, len(values))
	for i, value := range values {
		if value.IsZero() {
			return "", fmt.Errorf("planned time %d is zero", i)
		}
		encoded[i] = value.UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(encoded)
	if err != nil {
		return "", fmt.Errorf("encode planned times: %w", err)
	}
	return string(data), nil
}

func decodePlannedAliasCreationTimes(value string) ([]time.Time, error) {
	var encoded []string
	if err := json.Unmarshal([]byte(value), &encoded); err != nil {
		return nil, err
	}
	if encoded == nil {
		return []time.Time{}, nil
	}
	result := make([]time.Time, len(encoded))
	for i, raw := range encoded {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return nil, fmt.Errorf("planned time %d: %w", i, err)
		}
		result[i] = parsed.UTC()
	}
	return result, nil
}

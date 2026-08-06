package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

const aliasColumns = `
	al.id, al.account_id, ac.email, al.address, al.label, al.api_key_hash,
	al.api_key_prefix, al.enabled, al.last_sync_status, al.last_sync_error,
	al.last_synced_at, al.last_accessed_at, al.created_at, al.updated_at,
	lm.internal_date`

const aliasJoins = `
	FROM aliases al
	JOIN accounts ac ON ac.id = al.account_id
	LEFT JOIN latest_messages lm ON lm.alias_id = al.id`

func (s *Store) CreateAlias(ctx context.Context, alias domain.Alias) (domain.Alias, error) {
	if len(alias.APIKeyHash) == 0 {
		return domain.Alias{}, fmt.Errorf("create alias: api key hash is empty")
	}
	now := s.now()
	status := strings.TrimSpace(alias.LastSyncStatus)
	if status == "" {
		status = domain.SyncStatusPending
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO aliases(
			account_id, address, label, api_key_hash, api_key_prefix, enabled,
			last_sync_status, last_sync_error, last_synced_at,
			last_accessed_at, created_at, updated_at
		) SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE ? = 0 OR (
			SELECT COUNT(*) FROM aliases WHERE account_id = ? AND enabled = 1
		) < ?`,
		alias.AccountID, domain.NormalizeEmail(alias.Address), strings.TrimSpace(alias.Label),
		alias.APIKeyHash, strings.TrimSpace(alias.APIKeyPrefix), alias.Enabled,
		status, strings.TrimSpace(alias.LastSyncError), nullableTimestamp(alias.LastSyncedAt),
		nullableTimestamp(alias.LastAccessedAt), timestamp(now), timestamp(now),
		alias.Enabled, alias.AccountID, domain.MaxEnabledAliasesPerAccount,
	)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("create alias: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return domain.Alias{}, fmt.Errorf("read create alias result: %w", err)
	}
	if affected == 0 {
		return domain.Alias{}, fmt.Errorf("create alias: %w", ErrAliasLimit)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Alias{}, fmt.Errorf("read new alias id: %w", err)
	}
	return s.getAliasAfterWrite(id)
}

// UpdateAlias updates mutable administrator metadata. Address and account
// ownership are immutable; moving an alias requires deleting and recreating it.
func (s *Store) UpdateAlias(ctx context.Context, alias domain.Alias) (domain.Alias, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE aliases
		SET label = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		strings.TrimSpace(alias.Label), alias.Enabled, timestamp(s.now()), alias.ID,
	)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("update alias: %w", err)
	}
	if err := requireAffected(result, "alias"); err != nil {
		return domain.Alias{}, err
	}
	return s.getAliasAfterWrite(alias.ID)
}

func (s *Store) GetAlias(ctx context.Context, id int64) (domain.Alias, error) {
	return scanAlias(s.db.QueryRowContext(ctx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.id = ?`, id,
	))
}

func (s *Store) GetAliasByAddress(ctx context.Context, address string) (domain.Alias, error) {
	return scanAlias(s.db.QueryRowContext(ctx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.address = ? COLLATE NOCASE`,
		domain.NormalizeEmail(address),
	))
}

// ListAliases lists all aliases when accountIDs is empty, or aliases for the
// single supplied account ID.
func (s *Store) ListAliases(ctx context.Context, accountIDs ...int64) ([]domain.Alias, error) {
	if len(accountIDs) > 1 {
		return nil, fmt.Errorf("list aliases: expected at most one account id")
	}
	query := `SELECT ` + aliasColumns + aliasJoins
	var args []any
	if len(accountIDs) == 1 {
		query += ` WHERE al.account_id = ?`
		args = append(args, accountIDs[0])
	}
	query += ` ORDER BY al.address COLLATE NOCASE, al.id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer rows.Close()

	var aliases []domain.Alias
	for rows.Next() {
		alias, err := scanAlias(rows)
		if err != nil {
			return nil, err
		}
		aliases = append(aliases, alias)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate aliases: %w", err)
	}
	return aliases, nil
}

func (s *Store) ListAliasesByAccount(ctx context.Context, accountID int64) ([]domain.Alias, error) {
	return s.ListAliases(ctx, accountID)
}

func (s *Store) DeleteAlias(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM aliases WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete alias: %w", err)
	}
	return requireAffected(result, "alias")
}

func (s *Store) RotateAliasAPIKey(
	ctx context.Context,
	id int64,
	apiKeyHash []byte,
	apiKeyPrefix string,
) (domain.Alias, error) {
	if len(apiKeyHash) == 0 {
		return domain.Alias{}, fmt.Errorf("rotate alias api key: hash is empty")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE aliases
		SET api_key_hash = ?, api_key_prefix = ?, updated_at = ?
		WHERE id = ?`,
		apiKeyHash, strings.TrimSpace(apiKeyPrefix), timestamp(s.now()), id,
	)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("rotate alias api key: %w", err)
	}
	if err := requireAffected(result, "alias"); err != nil {
		return domain.Alias{}, err
	}
	return s.getAliasAfterWrite(id)
}

func (s *Store) getAliasAfterWrite(id int64) (domain.Alias, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.GetAlias(ctx, id)
}

func (s *Store) TouchAliasAccess(ctx context.Context, id int64, accessedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE aliases SET last_accessed_at = ?, updated_at = ? WHERE id = ?`,
		timestamp(accessedAt), timestamp(s.now()), id,
	)
	if err != nil {
		return fmt.Errorf("touch alias access: %w", err)
	}
	return requireAffected(result, "alias")
}

func (s *Store) TouchAliasLastAccessed(ctx context.Context, id int64, accessedAt time.Time) error {
	return s.TouchAliasAccess(ctx, id, accessedAt)
}

func (s *Store) UpdateAliasSyncStatus(ctx context.Context, id int64, status, syncError string, syncedAt *time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE aliases
		SET last_sync_status = ?, last_sync_error = ?, last_synced_at = ?, updated_at = ?
		WHERE id = ?`,
		status, syncError, nullableTimestamp(syncedAt), timestamp(s.now()), id,
	)
	if err != nil {
		return fmt.Errorf("update alias sync status: %w", err)
	}
	return requireAffected(result, "alias")
}

func (s *Store) ResetAliasSnapshot(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin alias reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM latest_messages WHERE alias_id = ?`, id); err != nil {
		return fmt.Errorf("delete alias snapshot: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE aliases
		SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL, updated_at = ?
		WHERE id = ?`, domain.SyncStatusPending, timestamp(s.now()), id)
	if err != nil {
		return fmt.Errorf("reset alias sync status: %w", err)
	}
	if err := requireAffected(result, "alias"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit alias reset: %w", err)
	}
	return nil
}

func (s *Store) SetAliasEnabled(ctx context.Context, id int64, enabled bool) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin alias state update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE aliases SET enabled = ?, updated_at = ?
		WHERE id = ? AND (? = 0 OR (
			SELECT COUNT(*) FROM aliases
			WHERE account_id = (SELECT account_id FROM aliases WHERE id = ?)
			  AND enabled = 1 AND id <> ?
		) < ?)`, enabled, timestamp(s.now()), id, enabled,
		id, id, domain.MaxEnabledAliasesPerAccount)
	if err != nil {
		return fmt.Errorf("update alias state: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read alias state update result: %w", err)
	}
	if affected == 0 {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM aliases WHERE id = ?)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("check alias state update target: %w", err)
		}
		if exists == 0 {
			return ErrNotFound
		}
		return fmt.Errorf("update alias state: %w", ErrAliasLimit)
	}
	if enabled {
		if _, err := tx.ExecContext(ctx, `DELETE FROM latest_messages WHERE alias_id = ?`, id); err != nil {
			return fmt.Errorf("delete re-enabled alias snapshot: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE aliases
			SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL
			WHERE id = ?`, domain.SyncStatusPending, id); err != nil {
			return fmt.Errorf("reset re-enabled alias status: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit alias state update: %w", err)
	}
	return nil
}

func (s *Store) ResetAccountAliasSnapshots(ctx context.Context, accountID int64) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin account snapshot reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM latest_messages
		WHERE alias_id IN (SELECT id FROM aliases WHERE account_id = ?)`, accountID); err != nil {
		return fmt.Errorf("delete account snapshots: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE aliases
		SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL, updated_at = ?
		WHERE account_id = ?`, domain.SyncStatusPending, timestamp(s.now()), accountID); err != nil {
		return fmt.Errorf("reset account alias statuses: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE accounts
		SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL, updated_at = ?
		WHERE id = ?`, domain.SyncStatusPending, timestamp(s.now()), accountID)
	if err != nil {
		return fmt.Errorf("reset account sync status: %w", err)
	}
	if err := requireAffected(result, "account"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit account snapshot reset: %w", err)
	}
	return nil
}

func scanAlias(scanner rowScanner) (domain.Alias, error) {
	var alias domain.Alias
	var enabled int
	var lastSyncedAt, lastAccessedAt, latestReceivedAt sql.NullInt64
	var createdAt, updatedAt int64
	err := scanner.Scan(
		&alias.ID, &alias.AccountID, &alias.AccountEmail, &alias.Address, &alias.Label,
		&alias.APIKeyHash, &alias.APIKeyPrefix, &enabled, &alias.LastSyncStatus,
		&alias.LastSyncError, &lastSyncedAt, &lastAccessedAt,
		&createdAt, &updatedAt, &latestReceivedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.Alias{}, ErrNotFound
		}
		return domain.Alias{}, fmt.Errorf("scan alias: %w", err)
	}
	alias.Enabled = enabled != 0
	alias.LastSyncedAt = timePtr(lastSyncedAt)
	alias.LastAccessedAt = timePtr(lastAccessedAt)
	alias.CreatedAt = timeFromTimestamp(createdAt)
	alias.UpdatedAt = timeFromTimestamp(updatedAt)
	alias.LatestReceivedAt = timePtr(latestReceivedAt)
	return alias, nil
}

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
	status := strings.TrimSpace(sanitizePostgresText(alias.LastSyncStatus))
	if status == "" {
		status = domain.SyncStatusPending
	}
	syncError := strings.TrimSpace(sanitizePostgresText(alias.LastSyncError))
	if !alias.Enabled && syncError == domain.AppleAliasConfirmationPending {
		return domain.Alias{}, fmt.Errorf("create alias: reserved confirmation marker: %w", ErrAliasConfirmationPending)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return domain.Alias{}, fmt.Errorf("begin alias creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, alias.AccountID)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("lock account for alias creation: %w", err)
	}
	if alias.Enabled {
		if err := s.requireEnabledAliasCapacity(ctx, tx, alias.AccountID); err != nil {
			return domain.Alias{}, fmt.Errorf("create alias: %w", err)
		}
	}
	var id int64
	err = s.txQueryRowContext(ctx, tx, `
		INSERT INTO aliases(
			account_id, address, label, api_key_hash, api_key_prefix, enabled,
			last_sync_status, last_sync_error, last_synced_at,
			last_accessed_at, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		alias.AccountID, domain.NormalizeEmail(sanitizePostgresText(alias.Address)),
		strings.TrimSpace(sanitizePostgresText(alias.Label)),
		alias.APIKeyHash, strings.TrimSpace(sanitizePostgresText(alias.APIKeyPrefix)), alias.Enabled,
		status, syncError, nullableTimestamp(alias.LastSyncedAt),
		nullableTimestamp(alias.LastAccessedAt), timestamp(now), timestamp(now),
	).Scan(&id)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("create alias: %w", err)
	}
	if alias.Enabled {
		if _, err := s.txExecContext(ctx, tx,
			`DELETE FROM imap_sync_states WHERE account_id = ?`, alias.AccountID,
		); err != nil {
			return domain.Alias{}, fmt.Errorf("reset IMAP cursor after alias creation: %w", err)
		}
		if _, err := s.bumpAccountVersionTx(ctx, tx, alias.AccountID, accountVersion); err != nil {
			return domain.Alias{}, fmt.Errorf("advance account version after alias creation: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Alias{}, fmt.Errorf("commit alias creation: %w", err)
	}
	return s.getAliasAfterWrite(id)
}

// UpdateAlias updates mutable administrator metadata. Address and account
// ownership are immutable; moving an alias requires deleting and recreating it.
func (s *Store) UpdateAlias(ctx context.Context, alias domain.Alias) (domain.Alias, error) {
	label := strings.TrimSpace(alias.Label)
	if err := s.updateAliasState(ctx, alias.ID, &label, alias.Enabled); err != nil {
		return domain.Alias{}, fmt.Errorf("update alias: %w", err)
	}
	return s.getAliasAfterWrite(alias.ID)
}

func (s *Store) GetAlias(ctx context.Context, id int64) (domain.Alias, error) {
	return scanAlias(s.queryRowContext(ctx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.id = ?`, id,
	))
}

func (s *Store) GetAliasByAddress(ctx context.Context, address string) (domain.Alias, error) {
	return scanAlias(s.queryRowContext(ctx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.address = ?`,
		domain.NormalizeEmail(sanitizePostgresText(address)),
	))
}

// ListAliases lists all aliases when accountIDs is empty, or aliases for the
// single supplied account ID.
func (s *Store) ListAliases(ctx context.Context, accountIDs ...int64) ([]domain.Alias, error) {
	return s.listAliases(ctx, false, accountIDs...)
}

func (s *Store) ListAliasesByAccount(ctx context.Context, accountID int64) ([]domain.Alias, error) {
	return s.ListAliases(ctx, accountID)
}

// ListEnabledAliasesByAccount returns the active aliases used by mailbox sync.
// Keeping this query separate preserves the administrator-facing list while
// allowing PostgreSQL to use the enabled-alias partial index.
func (s *Store) ListEnabledAliasesByAccount(ctx context.Context, accountID int64) ([]domain.Alias, error) {
	return s.listAliases(ctx, true, accountID)
}

func (s *Store) listAliases(ctx context.Context, enabledOnly bool, accountIDs ...int64) ([]domain.Alias, error) {
	if len(accountIDs) > 1 {
		return nil, fmt.Errorf("list aliases: expected at most one account id")
	}
	query := `SELECT ` + aliasColumns + aliasJoins
	var args []any
	var predicates []string
	if len(accountIDs) == 1 {
		predicates = append(predicates, `al.account_id = ?`)
		args = append(args, accountIDs[0])
	}
	if enabledOnly {
		predicates = append(predicates, `al.enabled = TRUE`)
	}
	if len(predicates) > 0 {
		query += ` WHERE ` + strings.Join(predicates, ` AND `)
	}
	query += ` ORDER BY al.address, al.id`

	rows, err := s.queryContext(ctx, query, args...)
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

func (s *Store) DeleteAlias(ctx context.Context, id int64) error {
	var accountID int64
	var enabled bool
	var lastSyncError string
	if err := s.queryRowContext(ctx,
		`SELECT account_id, enabled, last_sync_error FROM aliases WHERE id = ?`, id,
	).Scan(&accountID, &enabled, &lastSyncError); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("read alias account before delete: %w", err)
	}
	if !enabled && lastSyncError == domain.AppleAliasConfirmationPending {
		return fmt.Errorf("delete alias: %w", ErrAliasConfirmationPending)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin alias deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, accountID)
	if err != nil {
		return fmt.Errorf("lock alias account before delete: %w", err)
	}
	var currentEnabled bool
	var currentLastSyncError string
	if err := s.txQueryRowContext(ctx, tx,
		`SELECT enabled, last_sync_error FROM aliases WHERE id = ? AND account_id = ?`, id, accountID,
	).Scan(&currentEnabled, &currentLastSyncError); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("read alias state before delete: %w", err)
	}
	if !currentEnabled && currentLastSyncError == domain.AppleAliasConfirmationPending {
		return fmt.Errorf("delete alias: %w", ErrAliasConfirmationPending)
	}
	result, err := s.txExecContext(ctx, tx,
		`DELETE FROM aliases WHERE id = ? AND account_id = ?`, id, accountID,
	)
	if err != nil {
		return fmt.Errorf("delete alias: %w", err)
	}
	if err := requireAffected(result, "alias"); err != nil {
		return err
	}
	if currentEnabled {
		if _, err := s.bumpAccountVersionTx(ctx, tx, accountID, accountVersion); err != nil {
			return fmt.Errorf("advance account version after alias delete: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit alias deletion: %w", err)
	}
	return nil
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
	var accountID int64
	if err := s.queryRowContext(ctx,
		`SELECT account_id FROM aliases WHERE id = ?`, id,
	).Scan(&accountID); err != nil {
		if err == sql.ErrNoRows {
			return domain.Alias{}, ErrNotFound
		}
		return domain.Alias{}, fmt.Errorf("read alias account before api key rotation: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return domain.Alias{}, fmt.Errorf("begin alias api key rotation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.lockAccountVersionForUpdate(ctx, tx, accountID); err != nil {
		return domain.Alias{}, fmt.Errorf("lock alias account before api key rotation: %w", err)
	}
	var enabled bool
	var lastSyncError string
	if err := s.txQueryRowContext(ctx, tx,
		`SELECT enabled, last_sync_error FROM aliases WHERE id = ? AND account_id = ?`, id, accountID,
	).Scan(&enabled, &lastSyncError); err != nil {
		if err == sql.ErrNoRows {
			return domain.Alias{}, ErrNotFound
		}
		return domain.Alias{}, fmt.Errorf("read alias before api key rotation: %w", err)
	}
	if !enabled && lastSyncError == domain.AppleAliasConfirmationPending {
		return domain.Alias{}, fmt.Errorf("rotate alias api key: %w", ErrAliasConfirmationPending)
	}
	result, err := s.txExecContext(ctx, tx, `
		UPDATE aliases
		SET api_key_hash = ?, api_key_prefix = ?, updated_at = ?
		WHERE id = ? AND account_id = ?`,
		apiKeyHash, strings.TrimSpace(sanitizePostgresText(apiKeyPrefix)), timestamp(s.now()), id, accountID,
	)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("rotate alias api key: %w", err)
	}
	if err := requireAffected(result, "alias"); err != nil {
		return domain.Alias{}, err
	}
	if _, err := s.txExecContext(ctx, tx,
		`DELETE FROM pending_alias_api_keys WHERE alias_id = ?`, id,
	); err != nil {
		return domain.Alias{}, fmt.Errorf("delete stale pending alias key after rotation: %w", err)
	}
	rotated, err := s.getAliasByIDTx(ctx, tx, id)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("read alias after api key rotation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Alias{}, fmt.Errorf("commit alias api key rotation: %w", err)
	}
	return rotated, nil
}

func (s *Store) getAliasAfterWrite(id int64) (domain.Alias, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.GetAlias(ctx, id)
}

func (s *Store) TouchAliasAccess(ctx context.Context, id int64, accessedAt time.Time) error {
	result, err := s.execContext(ctx, `
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
	sanitizedError := sanitizePostgresText(syncError)
	reservedMarkerRequested := strings.TrimSpace(sanitizedError) == domain.AppleAliasConfirmationPending
	result, err := s.execContext(ctx, `
		UPDATE aliases
		SET last_sync_status = ?, last_sync_error = ?, last_synced_at = ?, updated_at = ?
		WHERE id = ?
		  AND NOT (enabled = FALSE AND last_sync_error = ?)
		  AND (enabled = TRUE OR ? = FALSE)`,
		sanitizePostgresText(status), sanitizedError,
		nullableTimestamp(syncedAt), timestamp(s.now()), id,
		domain.AppleAliasConfirmationPending, reservedMarkerRequested,
	)
	if err != nil {
		return fmt.Errorf("update alias sync status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read alias sync status update result: %w", err)
	}
	if affected != 0 {
		return nil
	}

	var exists int
	if err := s.queryRowContext(ctx, `SELECT 1 FROM aliases WHERE id = ?`, id).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("read alias after rejected sync status update: %w", err)
	}
	return fmt.Errorf("update alias sync status: reserved confirmation marker: %w", ErrAliasConfirmationPending)
}

func (s *Store) ResetAliasSnapshot(ctx context.Context, id int64) error {
	var accountID int64
	var enabled bool
	var lastSyncError string
	if err := s.queryRowContext(ctx,
		`SELECT account_id, enabled, last_sync_error FROM aliases WHERE id = ?`, id,
	).Scan(&accountID, &enabled, &lastSyncError); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("read alias account before reset: %w", err)
	}
	if !enabled && lastSyncError == domain.AppleAliasConfirmationPending {
		return fmt.Errorf("reset alias snapshot: %w", ErrAliasConfirmationPending)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin alias reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, accountID)
	if err != nil {
		return fmt.Errorf("lock alias account before reset: %w", err)
	}
	if _, err := s.txExecContext(ctx, tx,
		`DELETE FROM imap_sync_states WHERE account_id = ?`, accountID,
	); err != nil {
		return fmt.Errorf("reset IMAP cursor for alias snapshot: %w", err)
	}
	if _, err := s.txExecContext(ctx, tx, `DELETE FROM latest_messages WHERE alias_id = ?`, id); err != nil {
		return fmt.Errorf("delete alias snapshot: %w", err)
	}
	result, err := s.txExecContext(ctx, tx, `
		UPDATE aliases
		SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL, updated_at = ?
		WHERE id = ?`, domain.SyncStatusPending, timestamp(s.now()), id)
	if err != nil {
		return fmt.Errorf("reset alias sync status: %w", err)
	}
	if err := requireAffected(result, "alias"); err != nil {
		return err
	}
	if _, err := s.bumpAccountVersionTx(ctx, tx, accountID, accountVersion); err != nil {
		return fmt.Errorf("advance account version after alias reset: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit alias reset: %w", err)
	}
	return nil
}

func (s *Store) SetAliasEnabled(ctx context.Context, id int64, enabled bool) error {
	if err := s.updateAliasState(ctx, id, nil, enabled); err != nil {
		return fmt.Errorf("update alias state: %w", err)
	}
	return nil
}

func (s *Store) updateAliasState(ctx context.Context, id int64, label *string, enabled bool) error {
	var accountID int64
	if err := s.queryRowContext(ctx,
		`SELECT account_id FROM aliases WHERE id = ?`, id,
	).Scan(&accountID); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("read alias account: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, accountID)
	if err != nil {
		return fmt.Errorf("lock alias account: %w", err)
	}

	var currentEnabled bool
	var lastSyncError string
	if err := s.txQueryRowContext(ctx, tx,
		`SELECT enabled, last_sync_error FROM aliases WHERE id = ? AND account_id = ?`, id, accountID,
	).Scan(&currentEnabled, &lastSyncError); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("read alias state: %w", err)
	}
	if !currentEnabled && lastSyncError == domain.AppleAliasConfirmationPending {
		return ErrAliasConfirmationPending
	}
	reenabled := enabled && !currentEnabled
	clearReservedMarker := !enabled && currentEnabled && lastSyncError == domain.AppleAliasConfirmationPending
	if reenabled {
		if err := s.requireEnabledAliasCapacity(ctx, tx, accountID); err != nil {
			return err
		}
	}
	if enabled != currentEnabled {
		if _, err := s.bumpAccountVersionTx(ctx, tx, accountID, accountVersion); err != nil {
			return fmt.Errorf("advance account version after alias state change: %w", err)
		}
	}

	updates := make([]string, 0, 6)
	args := make([]any, 0, 8)
	if label != nil {
		*label = strings.TrimSpace(sanitizePostgresText(*label))
		updates = append(updates, "label = ?")
		args = append(args, *label)
	}
	updates = append(updates, "enabled = ?")
	args = append(args, enabled)
	if reenabled {
		updates = append(updates,
			"last_sync_status = ?",
			"last_sync_error = ''",
			"last_synced_at = NULL",
		)
		args = append(args, domain.SyncStatusPending)
	} else if clearReservedMarker {
		updates = append(updates, "last_sync_error = ''")
	}
	updates = append(updates, "updated_at = ?")
	args = append(args, timestamp(s.now()), id)
	result, err := s.txExecContext(ctx, tx,
		`UPDATE aliases SET `+strings.Join(updates, ", ")+` WHERE id = ?`, args...,
	)
	if err != nil {
		return fmt.Errorf("write alias: %w", err)
	}
	if err := requireAffected(result, "alias"); err != nil {
		return err
	}

	if reenabled {
		if _, err := s.txExecContext(ctx, tx, `DELETE FROM latest_messages WHERE alias_id = ?`, id); err != nil {
			return fmt.Errorf("delete re-enabled alias snapshot: %w", err)
		}
		if _, err := s.txExecContext(ctx, tx,
			`DELETE FROM imap_sync_states WHERE account_id = ?`, accountID,
		); err != nil {
			return fmt.Errorf("reset IMAP cursor after alias re-enable: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func (s *Store) requireEnabledAliasCapacity(ctx context.Context, tx *sql.Tx, accountID int64) error {
	var count int
	if err := s.txQueryRowContext(ctx, tx, `
		SELECT COUNT(*) FROM aliases
		WHERE account_id = ?
		  AND (enabled = TRUE OR (enabled = FALSE AND last_sync_error = ?))`,
		accountID, domain.AppleAliasConfirmationPending,
	).Scan(&count); err != nil {
		return fmt.Errorf("count enabled and confirmation-pending aliases: %w", err)
	}
	if count >= domain.MaxEnabledAliasesPerAccount {
		return ErrAliasLimit
	}
	return nil
}

func (s *Store) ResetAccountAliasSnapshots(ctx context.Context, accountID int64) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin account snapshot reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, accountID)
	if err != nil {
		return fmt.Errorf("lock account before snapshot reset: %w", err)
	}
	if _, err := s.txExecContext(ctx, tx, `DELETE FROM imap_sync_states WHERE account_id = ?`, accountID); err != nil {
		return fmt.Errorf("delete account IMAP sync state: %w", err)
	}
	if _, err := s.txExecContext(ctx, tx, `
		DELETE FROM latest_messages
		WHERE alias_id IN (
			SELECT id FROM aliases
			WHERE account_id = ?
			  AND NOT (enabled = FALSE AND last_sync_error = ?)
		)`, accountID, domain.AppleAliasConfirmationPending); err != nil {
		return fmt.Errorf("delete account snapshots: %w", err)
	}
	if _, err := s.txExecContext(ctx, tx, `
		UPDATE aliases
		SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL, updated_at = ?
		WHERE account_id = ?
		  AND NOT (enabled = FALSE AND last_sync_error = ?)`,
		domain.SyncStatusPending, timestamp(s.now()), accountID,
		domain.AppleAliasConfirmationPending); err != nil {
		return fmt.Errorf("reset account alias statuses: %w", err)
	}
	nextAccountVersion, err := s.nextAccountVersion(accountVersion)
	if err != nil {
		return fmt.Errorf("advance account version for snapshot reset: %w", err)
	}
	result, err := s.txExecContext(ctx, tx, `
		UPDATE accounts
		SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL, updated_at = ?
		WHERE id = ? AND updated_at = ?`,
		domain.SyncStatusPending, nextAccountVersion, accountID, accountVersion)
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
	var enabled bool
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
	alias.Enabled = enabled
	alias.LastSyncedAt = timePtr(lastSyncedAt)
	alias.LastAccessedAt = timePtr(lastAccessedAt)
	alias.CreatedAt = timeFromTimestamp(createdAt)
	alias.UpdatedAt = timeFromTimestamp(updatedAt)
	alias.LatestReceivedAt = timePtr(latestReceivedAt)
	return alias, nil
}

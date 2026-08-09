package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

// CountEnabledAliasesByAccount returns the number of active aliases for an
// account. The auto-creation service uses it before making a remote request;
// CreateAliasWithPendingAPIKey repeats the check inside its write transaction.
func (s *Store) CountEnabledAliasesByAccount(ctx context.Context, accountID int64) (int, error) {
	var count int
	if err := s.queryRowContext(ctx,
		`SELECT COUNT(*) FROM aliases WHERE account_id = ? AND enabled = TRUE`, accountID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count enabled aliases: %w", err)
	}
	return count, nil
}

// CreateAliasWithPendingAPIKey atomically persists a newly reserved Apple
// alias, its one-time API key ciphertext, and the rotated Apple web session.
// Enabled aliases are published immediately; disabled aliases remain staged
// for authoritative directory confirmation.
func (s *Store) CreateAliasWithPendingAPIKey(
	ctx context.Context,
	session domain.AppleWebSession,
	alias domain.Alias,
	apiKeyCiphertext string,
) (domain.Alias, domain.AppleWebSession, error) {
	if alias.AccountID < 1 || session.AccountID != alias.AccountID {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias: account identity is invalid")
	}
	if len(alias.APIKeyHash) == 0 {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias: api key hash is empty")
	}
	if strings.TrimSpace(alias.Address) == "" {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias: address is empty")
	}
	if strings.TrimSpace(apiKeyCiphertext) == "" {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias: api key ciphertext is empty")
	}
	if strings.TrimSpace(session.Ciphertext) == "" || strings.TrimSpace(session.AppleID) == "" {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias: Apple session is incomplete")
	}

	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("begin automatic alias creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, alias.AccountID)
	if err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("lock account for automatic alias creation: %w", err)
	}
	if alias.Enabled {
		if err := s.requireEnabledAliasCapacity(ctx, tx, alias.AccountID); err != nil {
			return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias: %w", err)
		}
	}

	if err := s.saveAppleWebSessionTx(ctx, tx, session, now); err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("save rotated Apple session: %w", err)
	}

	status := strings.TrimSpace(sanitizePostgresText(alias.LastSyncStatus))
	if status == "" {
		status = domain.SyncStatusPending
	}
	syncError := strings.TrimSpace(sanitizePostgresText(alias.LastSyncError))
	if !alias.Enabled {
		status = domain.SyncStatusPending
		syncError = domain.AppleAliasConfirmationPending
	}
	var aliasID int64
	err = s.txQueryRowContext(ctx, tx, `
		INSERT INTO aliases(
			account_id, address, label, api_key_hash, api_key_prefix, enabled,
			last_sync_status, last_sync_error, last_synced_at,
			last_accessed_at, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		alias.AccountID,
		domain.NormalizeEmail(sanitizePostgresText(alias.Address)),
		strings.TrimSpace(sanitizePostgresText(alias.Label)), alias.APIKeyHash,
		strings.TrimSpace(sanitizePostgresText(alias.APIKeyPrefix)), alias.Enabled, status,
		syncError, nullableTimestamp(alias.LastSyncedAt),
		nullableTimestamp(alias.LastAccessedAt), timestamp(now), timestamp(now),
	).Scan(&aliasID)
	if err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("insert automatic alias: %w", err)
	}
	if _, err := s.txExecContext(ctx, tx, `
		INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
		VALUES(?, ?, ?)`, aliasID, apiKeyCiphertext, timestamp(now)); err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("save pending automatic alias key: %w", err)
	}
	if alias.Enabled {
		if _, err := s.txExecContext(ctx, tx,
			`DELETE FROM imap_sync_states WHERE account_id = ?`, alias.AccountID); err != nil {
			return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("reset IMAP cursor after automatic alias creation: %w", err)
		}
		if _, err := s.bumpAccountVersionTx(ctx, tx, alias.AccountID, accountVersion); err != nil {
			return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("advance account version after automatic alias creation: %w", err)
		}
	}
	// Read the committed-shaped values while the transaction is still open.
	// Once Commit succeeds the caller's context may be canceled, so a second
	// query on the original context could report a false failure even though
	// the alias, key, and rotated session were already published atomically.
	createdAlias, err := scanAlias(s.txQueryRowContext(ctx, tx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.id = ?`, aliasID,
	))
	if err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("read automatic alias after insert: %w", err)
	}
	savedSession, err := scanAppleWebSession(s.txQueryRowContext(ctx, tx,
		`SELECT `+appleWebSessionColumns+` FROM apple_web_sessions WHERE account_id = ?`, alias.AccountID,
	))
	if err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("read rotated Apple session after insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("commit automatic alias creation: %w", err)
	}
	return createdAlias, savedSession, nil
}

// CreateAutoAlias is kept as a concise compatibility name for service
// adapters; it has exactly the same atomic behavior as the explicit method.
func (s *Store) CreateAutoAlias(ctx context.Context, session domain.AppleWebSession, alias domain.Alias, apiKeyCiphertext string) (domain.Alias, domain.AppleWebSession, error) {
	return s.CreateAliasWithPendingAPIKey(ctx, session, alias, apiKeyCiphertext)
}

func (s *Store) GetPendingAutoAliasConfirmation(ctx context.Context, accountID int64) (domain.PendingAliasAPIKey, error) {
	if accountID < 1 {
		return domain.PendingAliasAPIKey{}, fmt.Errorf("get pending automatic alias confirmation: account ID must be positive")
	}
	pending, err := scanPendingAliasAPIKey(s.queryRowContext(ctx, `
		SELECT `+aliasColumns+`, p.api_key_ciphertext, p.created_at`+aliasJoins+`
		JOIN pending_alias_api_keys p ON p.alias_id = al.id
		WHERE al.account_id = ?
		  AND al.enabled = FALSE
		  AND al.last_sync_error = ?
		ORDER BY p.created_at, al.id
		LIMIT 1`, accountID, domain.AppleAliasConfirmationPending))
	if err != nil {
		return domain.PendingAliasAPIKey{}, fmt.Errorf("get pending automatic alias confirmation: %w", err)
	}
	return pending, nil
}

// ConfirmPendingAutoAlias atomically publishes an alias whose Apple directory
// visibility was delayed after reserve. Its pending API key remains available
// for the normal one-time administrator retrieval flow.
func (s *Store) ConfirmPendingAutoAlias(
	ctx context.Context,
	session domain.AppleWebSession,
	aliasID int64,
) (domain.Alias, domain.AppleWebSession, error) {
	if aliasID < 1 || session.AccountID < 1 {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("confirm pending automatic alias: identity is invalid")
	}
	if strings.TrimSpace(session.Ciphertext) == "" || strings.TrimSpace(session.AppleID) == "" {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("confirm pending automatic alias: Apple session is incomplete")
	}

	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("begin pending automatic alias confirmation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, session.AccountID)
	if err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("lock account for pending automatic alias confirmation: %w", err)
	}

	var enabled bool
	var marker string
	if err := s.txQueryRowContext(ctx, tx, `
		SELECT al.enabled, al.last_sync_error
		FROM aliases al
		JOIN pending_alias_api_keys p ON p.alias_id = al.id
		WHERE al.id = ? AND al.account_id = ?`, aliasID, session.AccountID,
	).Scan(&enabled, &marker); err != nil {
		if err == sql.ErrNoRows {
			return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("confirm pending automatic alias: %w", ErrNotFound)
		}
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("read pending automatic alias confirmation: %w", err)
	}
	if enabled || marker != domain.AppleAliasConfirmationPending {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("confirm pending automatic alias: state marker is invalid")
	}
	if err := s.requirePendingAutoAliasConfirmationCapacity(ctx, tx, session.AccountID); err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("confirm pending automatic alias: %w", err)
	}
	if err := s.saveAppleWebSessionTx(ctx, tx, session, now); err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("save rotated Apple session: %w", err)
	}
	result, err := s.txExecContext(ctx, tx, `
		UPDATE aliases
		SET enabled = TRUE,
			last_sync_status = ?, last_sync_error = '', last_synced_at = NULL,
			updated_at = ?
		WHERE id = ? AND account_id = ?
		  AND enabled = FALSE AND last_sync_error = ?`,
		domain.SyncStatusPending, timestamp(now), aliasID, session.AccountID,
		domain.AppleAliasConfirmationPending,
	)
	if err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("enable confirmed automatic alias: %w", err)
	}
	if err := requireAffected(result, "pending automatic alias"); err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, err
	}
	if _, err := s.txExecContext(ctx, tx,
		`DELETE FROM imap_sync_states WHERE account_id = ?`, session.AccountID); err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("reset IMAP cursor after automatic alias confirmation: %w", err)
	}
	if _, err := s.bumpAccountVersionTx(ctx, tx, session.AccountID, accountVersion); err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("advance account version after automatic alias confirmation: %w", err)
	}

	confirmedAlias, err := scanAlias(s.txQueryRowContext(ctx, tx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.id = ?`, aliasID,
	))
	if err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("read confirmed automatic alias: %w", err)
	}
	savedSession, err := scanAppleWebSession(s.txQueryRowContext(ctx, tx,
		`SELECT `+appleWebSessionColumns+` FROM apple_web_sessions WHERE account_id = ?`, session.AccountID,
	))
	if err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("read rotated Apple session after confirmation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("commit pending automatic alias confirmation: %w", err)
	}
	return confirmedAlias, savedSession, nil
}

func (s *Store) ListPendingAliasAPIKeysByAccount(ctx context.Context, accountID int64) ([]domain.PendingAliasAPIKey, error) {
	rows, err := s.queryContext(ctx, `
		SELECT `+aliasColumns+`, p.api_key_ciphertext, p.created_at`+aliasJoins+`
		JOIN pending_alias_api_keys p ON p.alias_id = al.id
		WHERE al.account_id = ?
		  AND NOT (al.enabled = FALSE AND al.last_sync_error = ?)
		ORDER BY p.created_at, al.id`, accountID, domain.AppleAliasConfirmationPending)
	if err != nil {
		return nil, fmt.Errorf("list pending automatic alias keys: %w", err)
	}
	defer rows.Close()
	result := make([]domain.PendingAliasAPIKey, 0)
	for rows.Next() {
		pending, err := scanPendingAliasAPIKey(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, pending)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending automatic alias keys: %w", err)
	}
	return result, nil
}

func (s *Store) ListPendingAliasAPIKeys(ctx context.Context, accountID int64) ([]domain.PendingAliasAPIKey, error) {
	return s.ListPendingAliasAPIKeysByAccount(ctx, accountID)
}

func (s *Store) CountPendingAliasAPIKeysByAccount(ctx context.Context, accountID int64) (int, error) {
	var count int
	if err := s.queryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_alias_api_keys p
		JOIN aliases al ON al.id = p.alias_id
		WHERE al.account_id = ?
		  AND NOT (al.enabled = FALSE AND al.last_sync_error = ?)`,
		accountID, domain.AppleAliasConfirmationPending).Scan(&count); err != nil {
		return 0, fmt.Errorf("count pending automatic alias keys: %w", err)
	}
	return count, nil
}

// DeletePendingAliasAPIKeys acknowledges only the supplied alias IDs for this
// account. It never bulk-deletes a newer key created after a prior retrieval.
func (s *Store) DeletePendingAliasAPIKeys(ctx context.Context, accountID int64, aliasIDs []int64) error {
	if len(aliasIDs) == 0 {
		return nil
	}
	placeholders := make([]string, len(aliasIDs))
	args := make([]any, 0, len(aliasIDs)+1)
	args = append(args, accountID)
	for i, id := range aliasIDs {
		if id < 1 {
			return fmt.Errorf("delete pending automatic alias keys: alias id must be positive")
		}
		placeholders[i] = "?"
		args = append(args, id)
	}
	_, err := s.execContext(ctx, `
		DELETE FROM pending_alias_api_keys
		WHERE alias_id IN (`+strings.Join(placeholders, ",")+`)
		  AND alias_id IN (
			SELECT id FROM aliases
			WHERE account_id = ?
			  AND NOT (enabled = FALSE AND last_sync_error = ?)
		  )`, append(args[1:], accountID, domain.AppleAliasConfirmationPending)...)
	if err != nil {
		return fmt.Errorf("delete pending automatic alias keys: %w", err)
	}
	return nil
}

func (s *Store) DeletePendingAliasAPIKey(ctx context.Context, accountID, aliasID int64) error {
	return s.DeletePendingAliasAPIKeys(ctx, accountID, []int64{aliasID})
}

func scanPendingAliasAPIKey(scanner rowScanner) (domain.PendingAliasAPIKey, error) {
	var pending domain.PendingAliasAPIKey
	var alias domain.Alias
	var enabled bool
	var lastSyncedAt, lastAccessedAt, latestReceivedAt sql.NullInt64
	var createdAt, updatedAt, pendingCreatedAt int64
	if err := scanner.Scan(
		&alias.ID, &alias.AccountID, &alias.AccountEmail, &alias.Address, &alias.Label,
		&alias.APIKeyHash, &alias.APIKeyPrefix, &enabled, &alias.LastSyncStatus,
		&alias.LastSyncError, &lastSyncedAt, &lastAccessedAt, &createdAt, &updatedAt,
		&latestReceivedAt, &pending.APIKeyCiphertext, &pendingCreatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return domain.PendingAliasAPIKey{}, ErrNotFound
		}
		return domain.PendingAliasAPIKey{}, fmt.Errorf("scan pending automatic alias key: %w", err)
	}
	alias.Enabled = enabled
	alias.LastSyncedAt = timePtr(lastSyncedAt)
	alias.LastAccessedAt = timePtr(lastAccessedAt)
	alias.LatestReceivedAt = timePtr(latestReceivedAt)
	alias.CreatedAt = timeFromTimestamp(createdAt)
	alias.UpdatedAt = timeFromTimestamp(updatedAt)
	pending.Alias = alias
	pending.CreatedAt = timeFromTimestamp(pendingCreatedAt)
	return pending, nil
}

func (s *Store) requirePendingAutoAliasConfirmationCapacity(
	ctx context.Context,
	tx *sql.Tx,
	accountID int64,
) error {
	var count int
	if err := s.txQueryRowContext(ctx, tx, `
		SELECT COUNT(*) FROM aliases
		WHERE account_id = ? AND enabled = TRUE`, accountID,
	).Scan(&count); err != nil {
		return fmt.Errorf("count enabled aliases: %w", err)
	}
	if count >= domain.MaxEnabledAliasesPerAccount {
		return ErrAliasLimit
	}
	return nil
}

func (s *Store) saveAppleWebSessionTx(
	ctx context.Context,
	tx *sql.Tx,
	session domain.AppleWebSession,
	now time.Time,
) error {
	validatedAt := session.LastValidatedAt
	if validatedAt == nil || validatedAt.IsZero() {
		validatedAt = &now
	}
	_, err := s.txExecContext(ctx, tx, `
		INSERT INTO apple_web_sessions(
			account_id, session_ciphertext, apple_id, region, authenticated,
			last_validated_at, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			session_ciphertext = excluded.session_ciphertext,
			apple_id = excluded.apple_id,
			region = excluded.region,
			authenticated = excluded.authenticated,
			last_validated_at = excluded.last_validated_at,
			updated_at = excluded.updated_at`,
		session.AccountID,
		session.Ciphertext,
		strings.TrimSpace(sanitizePostgresText(session.AppleID)),
		strings.TrimSpace(sanitizePostgresText(session.Region)),
		session.Authenticated,
		timestamp(*validatedAt), timestamp(now), timestamp(now),
	)
	return err
}

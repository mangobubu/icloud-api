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

// CreateAliasWithPendingAPIKey atomically stores a newly reserved Apple alias,
// a v2 bundle that reuses its already-issued API key, the legacy encrypted
// pending key, and the rotated Apple session.
func (s *Store) CreateAliasWithPendingAPIKey(
	ctx context.Context,
	session domain.AppleWebSession,
	alias domain.Alias,
	apiKeyCiphertext string,
) (domain.Alias, domain.AppleWebSession, error) {
	if len(alias.APIKeyHash) == 0 || strings.TrimSpace(alias.APIKeyPrefix) == "" {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias: API key metadata is incomplete")
	}
	if strings.TrimSpace(apiKeyCiphertext) == "" {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias: API key ciphertext is empty")
	}
	return s.createAutoAliasCandidate(ctx, session, alias, apiKeyCiphertext)
}

// SupportsV2AutoAliasCredentials identifies the richer implementation without
// changing the original AutoCreateRepository method set.
func (s *Store) SupportsV2AutoAliasCredentials() bool { return true }

// CreateAutoAlias preserves the concise compatibility spelling exposed by the
// original store implementation.
func (s *Store) CreateAutoAlias(
	ctx context.Context,
	session domain.AppleWebSession,
	alias domain.Alias,
	apiKeyCiphertext string,
) (domain.Alias, domain.AppleWebSession, error) {
	return s.CreateAliasWithPendingAPIKey(ctx, session, alias, apiKeyCiphertext)
}

func (s *Store) createAutoAliasCandidate(
	ctx context.Context,
	session domain.AppleWebSession,
	alias domain.Alias,
	apiKeyCiphertext string,
) (domain.Alias, domain.AppleWebSession, error) {
	if alias.AccountID < 1 || session.AccountID != alias.AccountID {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias: account identity is invalid")
	}
	if strings.TrimSpace(alias.Address) == "" {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias: address is empty")
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
	initialHash := alias.APIKeyHash
	if len(initialHash) == 0 {
		initialHash, err = provisionalAliasHash(nil)
		if err != nil {
			return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias provisional credential: %w", err)
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
			account_id, address, label, api_key_hash, api_key_prefix, credential_mode, enabled,
			last_sync_status, last_sync_error, last_synced_at,
			last_accessed_at, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		alias.AccountID,
		domain.NormalizeEmail(sanitizePostgresText(alias.Address)),
		strings.TrimSpace(sanitizePostgresText(alias.Label)), initialHash,
		strings.TrimSpace(sanitizePostgresText(alias.APIKeyPrefix)),
		func() string {
			if apiKeyCiphertext != "" && s.credentialReuseFactory != nil {
				return domain.AliasCredentialModeV2
			}
			return domain.AliasCredentialModeLegacy
		}(), alias.Enabled, status,
		syncError, nullableTimestamp(alias.LastSyncedAt),
		nullableTimestamp(alias.LastAccessedAt), timestamp(now), timestamp(now),
	).Scan(&aliasID)
	if err != nil {
		return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("insert automatic alias: %w", err)
	}
	if apiKeyCiphertext != "" && s.credentialReuseFactory != nil {
		if err := s.installAliasCredentialMaterialTx(
			ctx, tx, aliasID, 1, true, s.credentialReuseFactory,
			apiKeyCiphertext, alias.APIKeyHash,
		); err != nil {
			return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias credentials: %w", err)
		}
		if _, err := s.txExecContext(ctx, tx, `
			INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
			VALUES(?, ?, ?)`, aliasID, apiKeyCiphertext, timestamp(now)); err != nil {
			return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("save pending automatic alias key: %w", err)
		}
	} else if apiKeyCiphertext != "" {
		// Legacy deployments have no v2 issuer. Preserve their original
		// pending-key record and API-key hash exactly.
		if _, err := s.txExecContext(ctx, tx, `
			INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
			VALUES(?, ?, ?)`, aliasID, apiKeyCiphertext, timestamp(now)); err != nil {
			return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("save pending automatic alias key: %w", err)
		}
	} else if s.credentialFactory != nil {
		if _, err := s.installGeneratedAliasCredentialsTx(ctx, tx, aliasID, 1, true); err != nil {
			return domain.Alias{}, domain.AppleWebSession{}, fmt.Errorf("create automatic alias credentials: %w", err)
		}
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
	// the alias, credential bundle, and rotated session were already published.
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
// visibility was delayed after reserve. Its credential bundle was already
// persisted with the provisional alias and becomes usable when enabled.
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

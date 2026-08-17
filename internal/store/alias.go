package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

const aliasColumns = `
	al.id, al.account_id, ac.email, al.address, al.label,
	al.group_id, COALESCE(mg.name, ''), al.api_key_hash,
	al.api_key_prefix, al.credential_mode, al.credential_ciphertext, al.imap_password_hash,
	al.oauth_client_id, al.refresh_token_hash, al.credential_version,
	al.mailbox_uid_validity, al.mailbox_uid_next,
	al.enabled, al.last_sync_status, al.last_sync_error,
	al.last_synced_at, al.last_accessed_at, al.created_at, al.updated_at,
	(SELECT MAX(m.internal_date)
	 FROM alias_messages am
	 JOIN archived_messages m ON m.id = am.message_id
	 WHERE am.alias_id = al.id)`

const aliasJoins = `
	FROM aliases al
	JOIN accounts ac ON ac.id = al.account_id
	LEFT JOIN mail_groups mg ON mg.id = al.group_id`

type AliasListFilter struct {
	AccountID *int64
	GroupID   *int64
	Ungrouped bool
	Query     string
	Limit     int
	Offset    int
}

type AliasPage struct {
	Items []domain.Alias
	Total int
}

// AliasAdminStateUpdate describes the optional administrator-facing alias
// fields that must be applied together. GroupIDPresent distinguishes an
// omitted group from an explicit nil value, which removes the alias from its
// current group.
type AliasAdminStateUpdate struct {
	Enabled        *bool
	GroupID        *int64
	GroupIDPresent bool
}

func (s *Store) CreateAlias(ctx context.Context, alias domain.Alias) (domain.Alias, error) {
	credentialMode, err := aliasCredentialMode(alias)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("create alias: %w", err)
	}
	if credentialMode == domain.AliasCredentialModeV2 && s.credentialFactory == nil {
		return domain.Alias{}, errors.New("create alias: v2 credential factory is not configured")
	}
	initialHash, err := provisionalAliasHash(alias.APIKeyHash)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("create alias provisional credential: %w", err)
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
	var groupID any
	if alias.GroupID != nil {
		if *alias.GroupID < 1 {
			return domain.Alias{}, errors.New("create alias: group ID must be positive")
		}
		if err := s.lockMailGroupForMove(ctx, tx, *alias.GroupID); err != nil {
			return domain.Alias{}, fmt.Errorf("create alias: %w", err)
		}
		groupID = *alias.GroupID
	}
	var id int64
	err = s.txQueryRowContext(ctx, tx, `
		INSERT INTO aliases(
			account_id, address, label, group_id, api_key_hash, api_key_prefix, credential_mode, enabled,
			last_sync_status, last_sync_error, last_synced_at,
			last_accessed_at, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		alias.AccountID, domain.NormalizeEmail(sanitizePostgresText(alias.Address)),
		strings.TrimSpace(sanitizePostgresText(alias.Label)), groupID,
		initialHash, strings.TrimSpace(sanitizePostgresText(alias.APIKeyPrefix)),
		credentialMode, alias.Enabled,
		status, syncError, nullableTimestamp(alias.LastSyncedAt),
		nullableTimestamp(alias.LastAccessedAt), timestamp(now), timestamp(now),
	).Scan(&id)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("create alias: %w", err)
	}
	// Explicit legacy callers provide an already-established API-key hash and
	// must not be silently rotated as part of the v2 credential setup.
	if s.credentialFactory != nil && credentialMode == domain.AliasCredentialModeV2 {
		if _, err := s.installGeneratedAliasCredentialsTx(ctx, tx, id, 1, true); err != nil {
			return domain.Alias{}, fmt.Errorf("create alias credentials: %w", err)
		}
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

func provisionalAliasHash(existing []byte) ([]byte, error) {
	if len(existing) > 0 {
		return append([]byte(nil), existing...), nil
	}
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return nil, err
	}
	return value, nil
}

func aliasCredentialMode(alias domain.Alias) (string, error) {
	mode := strings.TrimSpace(alias.CredentialMode)
	switch mode {
	case "":
		// Before credential modes existed, Store callers supplied the
		// established API-key hash directly. Treat that shape as legacy so
		// embedded callers keep their original key and mailbox contract. A
		// hash-less call is the unambiguous new-alias shape and therefore v2.
		if len(alias.APIKeyHash) > 0 {
			return domain.AliasCredentialModeLegacy, nil
		}
		return domain.AliasCredentialModeV2, nil
	case domain.AliasCredentialModeV2:
		return domain.AliasCredentialModeV2, nil
	case domain.AliasCredentialModeLegacy:
		if len(alias.APIKeyHash) != 32 {
			return "", errors.New("legacy credential mode requires a 32-byte API key hash")
		}
		return domain.AliasCredentialModeLegacy, nil
	default:
		return "", fmt.Errorf("unsupported credential mode %q", mode)
	}
}

// UpdateAlias updates mutable administrator metadata. Address and account
// ownership are immutable; moving an alias requires deleting and recreating it.
func (s *Store) UpdateAlias(ctx context.Context, alias domain.Alias) (domain.Alias, error) {
	label := strings.TrimSpace(alias.Label)
	enabled := alias.Enabled
	if err := s.updateAliasState(ctx, alias.ID, &label, &enabled, nil, false); err != nil {
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

// ListAliasesPage returns one administrator-facing page and the total number
// of aliases matching the optional primary-account and literal substring
// filters. Query matches alias address and label case-insensitively.
func (s *Store) ListAliasesPage(ctx context.Context, filter AliasListFilter) (AliasPage, error) {
	if err := validateListPage(filter.Limit, filter.Offset); err != nil {
		return AliasPage{}, fmt.Errorf("list aliases page: %w", err)
	}

	var predicates []string
	var filterArgs []any
	if filter.AccountID != nil {
		if *filter.AccountID < 1 {
			return AliasPage{}, errors.New("list aliases page: account ID must be positive")
		}
		predicates = append(predicates, `al.account_id = ?`)
		filterArgs = append(filterArgs, *filter.AccountID)
	}
	if filter.GroupID != nil {
		if *filter.GroupID < 1 {
			return AliasPage{}, errors.New("list aliases page: group ID must be positive")
		}
		predicates = append(predicates, `al.group_id = ?`)
		filterArgs = append(filterArgs, *filter.GroupID)
	}
	if filter.Ungrouped {
		if filter.GroupID != nil {
			return AliasPage{}, errors.New("list aliases page: group ID and ungrouped filter are mutually exclusive")
		}
		predicates = append(predicates, `al.group_id IS NULL`)
	}
	if query := strings.TrimSpace(sanitizePostgresText(filter.Query)); query != "" {
		pattern := "%" + escapeLikePattern(query) + "%"
		predicates = append(predicates, `(
			LOWER(al.address) LIKE LOWER(?) ESCAPE '!'
			OR LOWER(al.label) LIKE LOWER(?) ESCAPE '!'
		)`)
		filterArgs = append(filterArgs, pattern, pattern)
	}
	predicate := ""
	if len(predicates) > 0 {
		predicate = ` WHERE ` + strings.Join(predicates, ` AND `)
	}

	var total int
	if err := s.queryRowContext(ctx,
		`SELECT COUNT(*) FROM aliases al`+predicate,
		filterArgs...,
	).Scan(&total); err != nil {
		return AliasPage{}, fmt.Errorf("count aliases page: %w", err)
	}

	query := `SELECT ` + aliasColumns + aliasJoins + predicate +
		` ORDER BY al.address, al.id LIMIT ? OFFSET ?`
	args := append(append([]any{}, filterArgs...), filter.Limit, filter.Offset)
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return AliasPage{}, fmt.Errorf("list aliases page: %w", err)
	}
	defer rows.Close()

	aliases := make([]domain.Alias, 0, filter.Limit)
	for rows.Next() {
		alias, err := scanAlias(rows)
		if err != nil {
			return AliasPage{}, err
		}
		aliases = append(aliases, alias)
	}
	if err := rows.Err(); err != nil {
		return AliasPage{}, fmt.Errorf("iterate aliases page: %w", err)
	}
	return AliasPage{Items: aliases, Total: total}, nil
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

func (s *Store) SetAliasEnabled(ctx context.Context, id int64, enabled bool) error {
	if err := s.updateAliasState(ctx, id, nil, &enabled, nil, false); err != nil {
		return fmt.Errorf("update alias state: %w", err)
	}
	return nil
}

// UpdateAliasAdminState atomically applies an optional enabled state and an
// optional group assignment. In particular, a failed group assignment cannot
// leave the enabled state partially committed.
func (s *Store) UpdateAliasAdminState(
	ctx context.Context,
	id int64,
	update AliasAdminStateUpdate,
) (domain.Alias, error) {
	if update.Enabled == nil && !update.GroupIDPresent {
		return domain.Alias{}, errors.New("update alias admin state: no changes requested")
	}
	if err := s.updateAliasState(
		ctx,
		id,
		nil,
		update.Enabled,
		update.GroupID,
		update.GroupIDPresent,
	); err != nil {
		return domain.Alias{}, fmt.Errorf("update alias admin state: %w", err)
	}
	return s.getAliasAfterWrite(id)
}

func (s *Store) updateAliasState(
	ctx context.Context,
	id int64,
	label *string,
	enabled *bool,
	groupID *int64,
	groupIDPresent bool,
) error {
	if groupIDPresent && groupID != nil && *groupID < 1 {
		return errors.New("group ID must be positive")
	}
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
	if groupIDPresent && groupID != nil {
		if err := s.lockMailGroupForMove(ctx, tx, *groupID); err != nil {
			return err
		}
	}
	if !currentEnabled && lastSyncError == domain.AppleAliasConfirmationPending &&
		(label != nil || (enabled != nil && *enabled)) {
		return ErrAliasConfirmationPending
	}
	targetEnabled := currentEnabled
	if enabled != nil {
		targetEnabled = *enabled
	}
	reenabled := targetEnabled && !currentEnabled
	clearReservedMarker := enabled != nil && !targetEnabled && currentEnabled && lastSyncError == domain.AppleAliasConfirmationPending
	if reenabled {
		if err := s.requireEnabledAliasCapacity(ctx, tx, accountID); err != nil {
			return err
		}
	}
	if targetEnabled != currentEnabled {
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
	if enabled != nil {
		updates = append(updates, "enabled = ?")
		args = append(args, targetEnabled)
	}
	if groupIDPresent {
		var value any
		if groupID != nil {
			value = *groupID
		}
		updates = append(updates, "group_id = ?")
		args = append(args, value)
	}
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
	state, err := s.readAccountMailboxStateTx(ctx, tx, accountID)
	if err != nil {
		return fmt.Errorf("read account mailbox before alias capacity check: %w", err)
	}
	if !mailboxHasEnabledAliasLimit(state.MailboxType) {
		return nil
	}

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

func mailboxHasEnabledAliasLimit(mailboxType string) bool {
	return domain.NormalizeMailboxType(mailboxType) != domain.MailboxTypeCustom
}

func scanAlias(scanner rowScanner) (domain.Alias, error) {
	var alias domain.Alias
	var enabled bool
	var groupID sql.NullInt64
	var mailboxUIDValidity, mailboxUIDNext int64
	var lastSyncedAt, lastAccessedAt, latestReceivedAt sql.NullInt64
	var createdAt, updatedAt int64
	err := scanner.Scan(
		&alias.ID, &alias.AccountID, &alias.AccountEmail, &alias.Address, &alias.Label,
		&groupID, &alias.GroupName,
		&alias.APIKeyHash, &alias.APIKeyPrefix, &alias.CredentialMode, &alias.CredentialCiphertext,
		&alias.IMAPPasswordHash, &alias.OAuthClientID, &alias.RefreshTokenHash,
		&alias.CredentialVersion, &mailboxUIDValidity, &mailboxUIDNext,
		&enabled, &alias.LastSyncStatus,
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
	if groupID.Valid {
		value := groupID.Int64
		if value < 1 {
			return domain.Alias{}, fmt.Errorf("scan alias: invalid group ID")
		}
		alias.GroupID = &value
	}
	if mailboxUIDValidity < 1 || mailboxUIDValidity > int64(^uint32(0)) ||
		mailboxUIDNext < 1 || mailboxUIDNext > int64(^uint32(0)) {
		return domain.Alias{}, fmt.Errorf("scan alias: invalid mailbox UID state")
	}
	alias.MailboxUIDValidity = uint32(mailboxUIDValidity)
	alias.MailboxUIDNext = uint32(mailboxUIDNext)
	alias.LastSyncedAt = timePtr(lastSyncedAt)
	alias.LastAccessedAt = timePtr(lastAccessedAt)
	alias.CreatedAt = timeFromTimestamp(createdAt)
	alias.UpdatedAt = timeFromTimestamp(updatedAt)
	alias.LatestReceivedAt = timePtr(latestReceivedAt)
	return alias, nil
}

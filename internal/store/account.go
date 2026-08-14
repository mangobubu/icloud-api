package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

const accountColumns = `
	a.id, a.name, a.email, a.imap_host, a.imap_port, a.imap_username,
	a.password_ciphertext, a.enabled, a.last_sync_status, a.last_sync_error,
	a.last_synced_at, a.created_at, a.updated_at,
	(SELECT COUNT(*) FROM aliases al WHERE al.account_id = a.id) AS alias_count`

type AccountListFilter struct {
	Query  string
	Limit  int
	Offset int
}

type AccountPage struct {
	Items []domain.Account
	Total int
}

const maxListPageLimit = 1000

func (s *Store) CreateAccount(ctx context.Context, account domain.Account) (domain.Account, error) {
	now := s.now()
	status := strings.TrimSpace(sanitizePostgresText(account.LastSyncStatus))
	if status == "" {
		status = domain.SyncStatusPending
	}
	var id int64
	err := s.queryRowContext(ctx, `
		INSERT INTO accounts(
			name, email, imap_host, imap_port, imap_username, password_ciphertext,
			enabled, last_sync_status, last_sync_error, last_synced_at, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		strings.TrimSpace(sanitizePostgresText(account.Name)), domain.NormalizeEmail(sanitizePostgresText(account.Email)),
		strings.TrimSpace(sanitizePostgresText(account.IMAPHost)), account.IMAPPort,
		strings.TrimSpace(sanitizePostgresText(account.IMAPUsername)),
		account.PasswordCiphertext, account.Enabled, status, sanitizePostgresText(account.LastSyncError),
		nullableTimestamp(account.LastSyncedAt), timestamp(now), timestamp(now),
	).Scan(&id)
	if err != nil {
		return domain.Account{}, fmt.Errorf("create account: %w", err)
	}
	return s.getAccountAfterWrite(id)
}

// UpdateAccount updates administrator-editable IMAP settings. Rotating the
// credential or re-enabling an account atomically withdraws every alias from
// serving until the next confirmed sync. Changing the mailbox source (the
// endpoint or username) also discards mailbox identity state because UIDs are
// meaningful only to that source.
func (s *Store) UpdateAccount(ctx context.Context, account domain.Account) (domain.Account, error) {
	unlockArchive := s.lockMailArchiveAccount(account.ID)
	defer unlockArchive()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return domain.Account{}, fmt.Errorf("begin account update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, account.ID)
	if err != nil {
		return domain.Account{}, fmt.Errorf("lock account for update: %w", err)
	}

	var currentPassword string
	var currentEnabled bool
	var currentEmail, currentHost, currentUsername string
	var currentPort int
	var hasAliases bool
	if err := s.txQueryRowContext(ctx, tx, `
		SELECT password_ciphertext, enabled, email, imap_host, imap_port, imap_username,
		       EXISTS(SELECT 1 FROM aliases WHERE account_id = accounts.id)
		FROM accounts WHERE id = ?`, account.ID,
	).Scan(&currentPassword, &currentEnabled, &currentEmail, &currentHost, &currentPort, &currentUsername, &hasAliases); err != nil {
		if err == sql.ErrNoRows {
			return domain.Account{}, ErrNotFound
		}
		return domain.Account{}, fmt.Errorf("read account before update: %w", err)
	}
	requestedEmail := domain.NormalizeEmail(sanitizePostgresText(account.Email))
	requestedHost := strings.TrimSpace(sanitizePostgresText(account.IMAPHost))
	requestedUsername := strings.TrimSpace(sanitizePostgresText(account.IMAPUsername))
	emailChanged := requestedEmail != domain.NormalizeEmail(currentEmail)
	usernameChanged := requestedUsername != strings.TrimSpace(currentUsername)
	if hasAliases && emailChanged {
		return domain.Account{}, ErrAccountIdentityLocked
	}

	now := s.now()
	nextAccountVersion, err := s.nextAccountVersion(accountVersion)
	if err != nil {
		return domain.Account{}, fmt.Errorf("advance account version: %w", err)
	}
	result, err := s.txExecContext(ctx, tx, `
		UPDATE accounts SET
			name = ?,
			email = CASE WHEN EXISTS(SELECT 1 FROM aliases WHERE account_id = ?) THEN email ELSE ? END,
			imap_host = ?,
			imap_port = ?,
			imap_username = ?,
			password_ciphertext = CASE WHEN ? = '' THEN password_ciphertext ELSE ? END,
			enabled = ?, updated_at = ?
		WHERE id = ? AND updated_at = ?`,
		strings.TrimSpace(sanitizePostgresText(account.Name)), account.ID, requestedEmail,
		requestedHost, account.IMAPPort, requestedUsername,
		account.PasswordCiphertext, account.PasswordCiphertext, account.Enabled,
		nextAccountVersion, account.ID, accountVersion,
	)
	if err != nil {
		return domain.Account{}, fmt.Errorf("update account: %w", err)
	}
	if err := requireAffected(result, "account"); err != nil {
		return domain.Account{}, err
	}
	if !account.Enabled {
		// A disabled primary account must not retain a live background creation
		// plan. Clearing future slots in this transaction prevents the worker
		// from making another remote request after the account update commits.
		if _, err := s.txExecContext(ctx, tx, `
			UPDATE alias_creation_schedules
			SET enabled = FALSE, planned_at_json = '[]', next_run_at = NULL, updated_at = ?
			WHERE account_id = ?`, timestamp(now), account.ID); err != nil {
			return domain.Account{}, fmt.Errorf("disable alias creation after account update: %w", err)
		}
	}

	passwordChanged := account.PasswordCiphertext != "" && account.PasswordCiphertext != currentPassword
	reenabled := !currentEnabled && account.Enabled
	endpointChanged := requestedHost != strings.TrimSpace(currentHost) || account.IMAPPort != currentPort
	mailboxSourceChanged := endpointChanged || usernameChanged
	if emailChanged {
		if _, err := s.txExecContext(ctx, tx, `DELETE FROM apple_web_sessions WHERE account_id = ?`, account.ID); err != nil {
			return domain.Account{}, fmt.Errorf("delete apple web session after account email change: %w", err)
		}
	}
	if passwordChanged || reenabled || mailboxSourceChanged || emailChanged {
		// The next sync establishes a new no-backfill boundary. Password rotation
		// and re-enabling preserve stored mail; changing the mailbox source clears
		// every UID-scoped projection below.
		if _, err := s.txExecContext(ctx, tx, `DELETE FROM imap_sync_states WHERE account_id = ?`, account.ID); err != nil {
			return domain.Account{}, fmt.Errorf("delete account IMAP sync state: %w", err)
		}
		if mailboxSourceChanged {
			// UIDVALIDITY and UID are scoped to one mailbox source. Retaining
			// snapshots or consumption history across an endpoint or username
			// change could expose old mail or suppress unrelated mail with
			// matching UIDs.
			if _, err := s.txExecContext(ctx, tx, `
				DELETE FROM latest_messages
				WHERE alias_id IN (SELECT id FROM aliases WHERE account_id = ?)`, account.ID); err != nil {
				return domain.Account{}, fmt.Errorf("delete account mailbox snapshots: %w", err)
			}
			if _, err := s.txExecContext(ctx, tx, `
				DELETE FROM consumed_messages
				WHERE alias_id IN (SELECT id FROM aliases WHERE account_id = ?)`, account.ID); err != nil {
				return domain.Account{}, fmt.Errorf("delete account message consumption history: %w", err)
			}
			// Seen tasks contain UIDs from the old mailbox source and must not be
			// replayed against the newly configured mailbox.
			if _, err := s.txExecContext(ctx, tx, `DELETE FROM imap_seen_tasks WHERE account_id = ?`, account.ID); err != nil {
				return domain.Account{}, fmt.Errorf("delete account IMAP seen tasks: %w", err)
			}
			// The normalized v2 archive uses the upstream UID identity as an
			// account-scoped key. A different source may reuse the same values, so
			// remove the old generation and invalidate every public IMAPS mailbox.
			if _, err := s.txExecContext(ctx, tx, `DELETE FROM archived_messages WHERE account_id = ?`, account.ID); err != nil {
				return domain.Account{}, fmt.Errorf("delete account archived messages: %w", err)
			}
			if _, err := s.txExecContext(ctx, tx, `
				UPDATE aliases
				SET mailbox_uid_validity = CASE
						WHEN mailbox_uid_validity >= 4294967295 THEN 1
						ELSE mailbox_uid_validity + 1
					END,
					mailbox_uid_next = 1,
					updated_at = ?
				WHERE account_id = ?`, timestamp(now), account.ID); err != nil {
				return domain.Account{}, fmt.Errorf("reset alias mailbox generation: %w", err)
			}
		}
		if _, err := s.txExecContext(ctx, tx, `
			UPDATE aliases
			SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL, updated_at = ?
			WHERE account_id = ?
			  AND NOT (enabled = FALSE AND last_sync_error = ?)`,
			domain.SyncStatusPending, timestamp(now), account.ID,
			domain.AppleAliasConfirmationPending); err != nil {
			return domain.Account{}, fmt.Errorf("reset account alias statuses: %w", err)
		}
		if _, err := s.txExecContext(ctx, tx, `
			UPDATE accounts
			SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL, updated_at = ?
			WHERE id = ?`, domain.SyncStatusPending, nextAccountVersion, account.ID); err != nil {
			return domain.Account{}, fmt.Errorf("reset account sync status: %w", err)
		}
	}

	quarantine := mailArchiveQuarantine{}
	if mailboxSourceChanged {
		quarantine, err = s.quarantineMailArchiveAccount(account.ID)
		if err != nil {
			return domain.Account{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		if restoreErr := quarantine.restore(); restoreErr != nil {
			return domain.Account{}, fmt.Errorf("commit account update: %w", errors.Join(err, restoreErr))
		}
		return domain.Account{}, fmt.Errorf("commit account update: %w", err)
	}
	quarantine.discard()
	return s.getAccountAfterWrite(account.ID)
}

func (s *Store) getAccountAfterWrite(id int64) (domain.Account, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.GetAccount(ctx, id)
}

func (s *Store) GetAccount(ctx context.Context, id int64) (domain.Account, error) {
	return scanAccount(s.queryRowContext(ctx,
		`SELECT `+accountColumns+` FROM accounts a WHERE a.id = ?`, id,
	))
}

func (s *Store) GetAccountByEmail(ctx context.Context, email string) (domain.Account, error) {
	return scanAccount(s.queryRowContext(ctx,
		`SELECT `+accountColumns+` FROM accounts a WHERE a.email = ?`,
		domain.NormalizeEmail(sanitizePostgresText(email)),
	))
}

func (s *Store) ListAccounts(ctx context.Context) ([]domain.Account, error) {
	return s.listAccounts(ctx, false)
}

// ListAccountsPage returns one administrator-facing page and the total number
// of accounts matching Query. Query is a case-insensitive literal substring
// match against account email and name.
func (s *Store) ListAccountsPage(ctx context.Context, filter AccountListFilter) (AccountPage, error) {
	if err := validateListPage(filter.Limit, filter.Offset); err != nil {
		return AccountPage{}, fmt.Errorf("list accounts page: %w", err)
	}

	predicate := ""
	var filterArgs []any
	if query := strings.TrimSpace(sanitizePostgresText(filter.Query)); query != "" {
		pattern := "%" + escapeLikePattern(query) + "%"
		predicate = ` WHERE (
			LOWER(a.email) LIKE LOWER(?) ESCAPE '!'
			OR LOWER(a.name) LIKE LOWER(?) ESCAPE '!'
		)`
		filterArgs = append(filterArgs, pattern, pattern)
	}

	var total int
	if err := s.queryRowContext(ctx,
		`SELECT COUNT(*) FROM accounts a`+predicate,
		filterArgs...,
	).Scan(&total); err != nil {
		return AccountPage{}, fmt.Errorf("count accounts page: %w", err)
	}

	query := `SELECT ` + accountColumns + ` FROM accounts a` + predicate +
		` ORDER BY a.email, a.id LIMIT ? OFFSET ?`
	args := append(append([]any{}, filterArgs...), filter.Limit, filter.Offset)
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return AccountPage{}, fmt.Errorf("list accounts page: %w", err)
	}
	defer rows.Close()

	accounts := make([]domain.Account, 0, filter.Limit)
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return AccountPage{}, err
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return AccountPage{}, fmt.Errorf("iterate accounts page: %w", err)
	}
	return AccountPage{Items: accounts, Total: total}, nil
}

func (s *Store) ListEnabledAccounts(ctx context.Context) ([]domain.Account, error) {
	return s.listAccounts(ctx, true)
}

func (s *Store) listAccounts(ctx context.Context, enabledOnly bool) ([]domain.Account, error) {
	query := `SELECT ` + accountColumns + ` FROM accounts a`
	if enabledOnly {
		query += ` WHERE a.enabled = TRUE`
	}
	query += ` ORDER BY a.email, a.id`
	rows, err := s.queryContext(ctx,
		query,
	)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	var accounts []domain.Account
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accounts: %w", err)
	}
	return accounts, nil
}

func validateListPage(limit, offset int) error {
	if limit < 1 {
		return errors.New("limit must be positive")
	}
	if limit > maxListPageLimit {
		return fmt.Errorf("limit must not exceed %d", maxListPageLimit)
	}
	if offset < 0 {
		return errors.New("offset must not be negative")
	}
	return nil
}

func escapeLikePattern(value string) string {
	return strings.NewReplacer(
		"!", "!!",
		"%", "!%",
		"_", "!_",
	).Replace(value)
}

func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	unlockArchive := s.lockMailArchiveAccount(id)
	defer unlockArchive()
	quarantine, err := s.quarantineMailArchiveAccount(id)
	if err != nil {
		return err
	}
	result, err := s.execContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		if restoreErr := quarantine.restore(); restoreErr != nil {
			return fmt.Errorf("delete account: %w", errors.Join(err, restoreErr))
		}
		return fmt.Errorf("delete account: %w", err)
	}
	if err := requireAffected(result, "account"); err != nil {
		if restoreErr := quarantine.restore(); restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		return err
	}
	quarantine.discard()
	return nil
}

func (s *Store) SetAccountSyncStatus(
	ctx context.Context,
	id int64,
	status string,
	syncError string,
	syncedAt *time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin account sync status update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("lock account for sync status update: %w", err)
	}
	nextAccountVersion, err := s.nextAccountVersion(accountVersion)
	if err != nil {
		return fmt.Errorf("advance account version for sync status update: %w", err)
	}
	result, err := s.txExecContext(ctx, tx, `
		UPDATE accounts
		SET last_sync_status = ?, last_sync_error = ?, last_synced_at = ?, updated_at = ?
		WHERE id = ? AND updated_at = ?`,
		sanitizePostgresText(status), sanitizePostgresText(syncError),
		nullableTimestamp(syncedAt), nextAccountVersion, id, accountVersion,
	)
	if err != nil {
		return fmt.Errorf("set account sync status: %w", err)
	}
	if err := requireAffected(result, "account"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit account sync status update: %w", err)
	}
	return nil
}

func (s *Store) UpdateAccountSyncStatus(
	ctx context.Context,
	id int64,
	status string,
	syncError string,
	syncedAt *time.Time,
) error {
	return s.SetAccountSyncStatus(ctx, id, status, syncError, syncedAt)
}

func scanAccount(scanner rowScanner) (domain.Account, error) {
	var account domain.Account
	var enabled bool
	var lastSyncedAt sql.NullInt64
	var createdAt, updatedAt int64
	err := scanner.Scan(
		&account.ID, &account.Name, &account.Email, &account.IMAPHost, &account.IMAPPort,
		&account.IMAPUsername, &account.PasswordCiphertext, &enabled,
		&account.LastSyncStatus, &account.LastSyncError, &lastSyncedAt,
		&createdAt, &updatedAt, &account.AliasCount,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.Account{}, ErrNotFound
		}
		return domain.Account{}, fmt.Errorf("scan account: %w", err)
	}
	account.Enabled = enabled
	account.LastSyncedAt = timePtr(lastSyncedAt)
	account.CreatedAt = timeFromTimestamp(createdAt)
	account.UpdatedAt = timeFromTimestamp(updatedAt)
	return account, nil
}

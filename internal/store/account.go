package store

import (
	"context"
	"database/sql"
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
// credential or re-enabling an account atomically withdraws every alias
// snapshot until the next confirmed sync.
func (s *Store) UpdateAccount(ctx context.Context, account domain.Account) (domain.Account, error) {
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
	var currentEmail, currentUsername string
	var hasAliases bool
	if err := s.txQueryRowContext(ctx, tx, `
		SELECT password_ciphertext, enabled, email, imap_username,
		       EXISTS(SELECT 1 FROM aliases WHERE account_id = accounts.id)
		FROM accounts WHERE id = ?`, account.ID,
	).Scan(&currentPassword, &currentEnabled, &currentEmail, &currentUsername, &hasAliases); err != nil {
		if err == sql.ErrNoRows {
			return domain.Account{}, ErrNotFound
		}
		return domain.Account{}, fmt.Errorf("read account before update: %w", err)
	}
	requestedEmail := domain.NormalizeEmail(sanitizePostgresText(account.Email))
	requestedUsername := strings.TrimSpace(sanitizePostgresText(account.IMAPUsername))
	if hasAliases && (requestedEmail != domain.NormalizeEmail(currentEmail) || requestedUsername != strings.TrimSpace(currentUsername)) {
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
			imap_username = CASE WHEN EXISTS(SELECT 1 FROM aliases WHERE account_id = ?) THEN imap_username ELSE ? END,
			password_ciphertext = CASE WHEN ? = '' THEN password_ciphertext ELSE ? END,
			enabled = ?, updated_at = ?
		WHERE id = ? AND updated_at = ?`,
		strings.TrimSpace(sanitizePostgresText(account.Name)), account.ID, requestedEmail,
		account.ID, requestedUsername,
		account.PasswordCiphertext, account.PasswordCiphertext, account.Enabled,
		nextAccountVersion, account.ID, accountVersion,
	)
	if err != nil {
		return domain.Account{}, fmt.Errorf("update account: %w", err)
	}
	if err := requireAffected(result, "account"); err != nil {
		return domain.Account{}, err
	}

	passwordChanged := account.PasswordCiphertext != "" && account.PasswordCiphertext != currentPassword
	reenabled := !currentEnabled && account.Enabled
	identityChanged := !hasAliases && (requestedEmail != domain.NormalizeEmail(currentEmail) ||
		requestedUsername != strings.TrimSpace(currentUsername))
	if identityChanged {
		if _, err := s.txExecContext(ctx, tx, `DELETE FROM apple_web_sessions WHERE account_id = ?`, account.ID); err != nil {
			return domain.Account{}, fmt.Errorf("delete apple web session after account identity change: %w", err)
		}
	}
	if passwordChanged || reenabled || identityChanged {
		// Pending status keeps the API from serving this snapshot. Preserve the
		// row until the next bounded reset can compare UIDVALIDITY and avoid
		// discarding a valid same-generation message outside its recent window.
		if _, err := s.txExecContext(ctx, tx, `DELETE FROM imap_sync_states WHERE account_id = ?`, account.ID); err != nil {
			return domain.Account{}, fmt.Errorf("delete account IMAP sync state: %w", err)
		}
		if _, err := s.txExecContext(ctx, tx, `
			UPDATE aliases
			SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL, updated_at = ?
			WHERE account_id = ?`, domain.SyncStatusPending, timestamp(now), account.ID); err != nil {
			return domain.Account{}, fmt.Errorf("reset account alias statuses: %w", err)
		}
		if _, err := s.txExecContext(ctx, tx, `
			UPDATE accounts
			SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL, updated_at = ?
			WHERE id = ?`, domain.SyncStatusPending, nextAccountVersion, account.ID); err != nil {
			return domain.Account{}, fmt.Errorf("reset account sync status: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.Account{}, fmt.Errorf("commit account update: %w", err)
	}
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

func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	result, err := s.execContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	return requireAffected(result, "account")
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

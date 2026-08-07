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
	status := strings.TrimSpace(account.LastSyncStatus)
	if status == "" {
		status = domain.SyncStatusPending
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO accounts(
			name, email, imap_host, imap_port, imap_username, password_ciphertext,
			enabled, last_sync_status, last_sync_error, last_synced_at, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(account.Name), domain.NormalizeEmail(account.Email),
		strings.TrimSpace(account.IMAPHost), account.IMAPPort, strings.TrimSpace(account.IMAPUsername),
		account.PasswordCiphertext, account.Enabled, status, account.LastSyncError,
		nullableTimestamp(account.LastSyncedAt), timestamp(now), timestamp(now),
	)
	if err != nil {
		return domain.Account{}, fmt.Errorf("create account: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Account{}, fmt.Errorf("read new account id: %w", err)
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
	lockResult, err := tx.ExecContext(ctx, `UPDATE accounts SET updated_at = updated_at WHERE id = ?`, account.ID)
	if err != nil {
		return domain.Account{}, fmt.Errorf("lock account for update: %w", err)
	}
	if err := requireAffected(lockResult, "account"); err != nil {
		return domain.Account{}, err
	}

	var currentPassword string
	var currentEnabled int
	var currentEmail, currentUsername string
	var hasAliases int
	if err := tx.QueryRowContext(ctx, `
		SELECT password_ciphertext, enabled, email, imap_username,
		       EXISTS(SELECT 1 FROM aliases WHERE account_id = accounts.id)
		FROM accounts WHERE id = ?`, account.ID,
	).Scan(&currentPassword, &currentEnabled, &currentEmail, &currentUsername, &hasAliases); err != nil {
		if err == sql.ErrNoRows {
			return domain.Account{}, ErrNotFound
		}
		return domain.Account{}, fmt.Errorf("read account before update: %w", err)
	}
	requestedEmail := domain.NormalizeEmail(account.Email)
	requestedUsername := strings.TrimSpace(account.IMAPUsername)
	if hasAliases != 0 && (requestedEmail != domain.NormalizeEmail(currentEmail) || requestedUsername != strings.TrimSpace(currentUsername)) {
		return domain.Account{}, ErrAccountIdentityLocked
	}

	now := s.now()
	result, err := tx.ExecContext(ctx, `
		UPDATE accounts SET
			name = ?,
			email = CASE WHEN EXISTS(SELECT 1 FROM aliases WHERE account_id = ?) THEN email ELSE ? END,
			imap_username = CASE WHEN EXISTS(SELECT 1 FROM aliases WHERE account_id = ?) THEN imap_username ELSE ? END,
			password_ciphertext = CASE WHEN ? = '' THEN password_ciphertext ELSE ? END,
			enabled = ?, updated_at = ?
		WHERE id = ?`,
		strings.TrimSpace(account.Name), account.ID, requestedEmail,
		account.ID, requestedUsername,
		account.PasswordCiphertext, account.PasswordCiphertext, account.Enabled, timestamp(now), account.ID,
	)
	if err != nil {
		return domain.Account{}, fmt.Errorf("update account: %w", err)
	}
	if err := requireAffected(result, "account"); err != nil {
		return domain.Account{}, err
	}

	passwordChanged := account.PasswordCiphertext != "" && account.PasswordCiphertext != currentPassword
	reenabled := currentEnabled == 0 && account.Enabled
	identityChanged := hasAliases == 0 && (requestedEmail != domain.NormalizeEmail(currentEmail) ||
		requestedUsername != strings.TrimSpace(currentUsername))
	if identityChanged {
		if _, err := tx.ExecContext(ctx, `DELETE FROM apple_web_sessions WHERE account_id = ?`, account.ID); err != nil {
			return domain.Account{}, fmt.Errorf("delete apple web session after account identity change: %w", err)
		}
	}
	if passwordChanged || reenabled || identityChanged {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM latest_messages
			WHERE alias_id IN (SELECT id FROM aliases WHERE account_id = ?)`, account.ID); err != nil {
			return domain.Account{}, fmt.Errorf("delete account snapshots: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE aliases
			SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL, updated_at = ?
			WHERE account_id = ?`, domain.SyncStatusPending, timestamp(now), account.ID); err != nil {
			return domain.Account{}, fmt.Errorf("reset account alias statuses: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE accounts
			SET last_sync_status = ?, last_sync_error = '', last_synced_at = NULL, updated_at = ?
			WHERE id = ?`, domain.SyncStatusPending, timestamp(now), account.ID); err != nil {
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
	return scanAccount(s.db.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM accounts a WHERE a.id = ?`, id,
	))
}

func (s *Store) GetAccountByEmail(ctx context.Context, email string) (domain.Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM accounts a WHERE a.email = ? COLLATE NOCASE`,
		domain.NormalizeEmail(email),
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
		query += ` WHERE a.enabled = 1`
	}
	query += ` ORDER BY a.email COLLATE NOCASE, a.id`
	rows, err := s.db.QueryContext(ctx,
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
	result, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
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
	result, err := s.db.ExecContext(ctx, `
		UPDATE accounts
		SET last_sync_status = ?, last_sync_error = ?, last_synced_at = ?, updated_at = ?
		WHERE id = ?`,
		status, syncError, nullableTimestamp(syncedAt), timestamp(s.now()), id,
	)
	if err != nil {
		return fmt.Errorf("set account sync status: %w", err)
	}
	return requireAffected(result, "account")
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
	var enabled int
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
	account.Enabled = enabled != 0
	account.LastSyncedAt = timePtr(lastSyncedAt)
	account.CreatedAt = timeFromTimestamp(createdAt)
	account.UpdatedAt = timeFromTimestamp(updatedAt)
	return account, nil
}

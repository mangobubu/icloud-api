package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"icloud-api/internal/domain"
)

func (s *Store) CreateAdmin(ctx context.Context, username, passwordHash string) (domain.Admin, error) {
	username = strings.TrimSpace(sanitizePostgresText(username))
	createdAt := s.now()
	var id int64
	err := s.queryRowContext(ctx,
		`INSERT INTO admins(username, password_hash, password_version, created_at) VALUES(?, ?, 1, ?) RETURNING id`,
		username, passwordHash, timestamp(createdAt),
	).Scan(&id)
	if err != nil {
		return domain.Admin{}, fmt.Errorf("create admin: %w", err)
	}
	return domain.Admin{ID: id, Username: username, PasswordHash: passwordHash, PasswordVersion: 1, CreatedAt: createdAt}, nil
}

func (s *Store) GetAdminByID(ctx context.Context, id int64) (domain.Admin, error) {
	return scanAdmin(s.queryRowContext(ctx,
		`SELECT id, username, password_hash, password_version, created_at FROM admins WHERE id = ?`, id,
	))
}

func (s *Store) GetAdminByUsername(ctx context.Context, username string) (domain.Admin, error) {
	return scanAdmin(s.queryRowContext(ctx,
		`SELECT id, username, password_hash, password_version, created_at FROM admins WHERE username = ?`,
		strings.TrimSpace(sanitizePostgresText(username)),
	))
}

func (s *Store) ListAdmins(ctx context.Context) ([]domain.Admin, error) {
	rows, err := s.queryContext(ctx,
		`SELECT id, username, password_hash, password_version, created_at FROM admins ORDER BY username, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list admins: %w", err)
	}
	defer rows.Close()

	var admins []domain.Admin
	for rows.Next() {
		admin, err := scanAdmin(rows)
		if err != nil {
			return nil, err
		}
		admins = append(admins, admin)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admins: %w", err)
	}
	return admins, nil
}

func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var count int
	if err := s.queryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}
	return count, nil
}

func (s *Store) UpdateAdminPassword(ctx context.Context, id int64, passwordHash string) error {
	result, err := s.execContext(ctx, `UPDATE admins SET password_hash = ?, password_version = password_version + 1 WHERE id = ?`, passwordHash, id)
	if err != nil {
		return fmt.Errorf("update admin password: %w", err)
	}
	return requireAffected(result, "admin")
}

func (s *Store) ChangeAdminPasswordAndRevokeSessions(ctx context.Context, id, expectedVersion int64, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin admin password change: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := s.txExecContext(ctx, tx, `
		UPDATE admins
		SET password_hash = ?, password_version = password_version + 1
		WHERE id = ? AND password_version = ?`, passwordHash, id, expectedVersion)
	if err != nil {
		return fmt.Errorf("update admin password: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read password change result: %w", err)
	}
	if changed == 0 {
		return ErrCredentialsChanged
	}
	if _, err := s.txExecContext(ctx, tx, `DELETE FROM admin_sessions WHERE admin_id = ?`, id); err != nil {
		return fmt.Errorf("revoke admin sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit admin password change: %w", err)
	}
	return nil
}

// ResetAdminCredentialsAndRevokeSessions atomically replaces an administrator's
// login name and password while invalidating every existing session.
func (s *Store) ResetAdminCredentialsAndRevokeSessions(ctx context.Context, id, expectedVersion int64, username, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin admin credentials reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := s.txExecContext(ctx, tx, `
		UPDATE admins
		SET username = ?, password_hash = ?, password_version = password_version + 1
		WHERE id = ? AND password_version = ?`,
		strings.TrimSpace(sanitizePostgresText(username)), passwordHash, id, expectedVersion,
	)
	if err != nil {
		return fmt.Errorf("reset admin credentials: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read admin credentials reset result: %w", err)
	}
	if changed == 0 {
		return ErrCredentialsChanged
	}
	if _, err := s.txExecContext(ctx, tx, `DELETE FROM admin_sessions WHERE admin_id = ?`, id); err != nil {
		return fmt.Errorf("revoke admin sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit admin credentials reset: %w", err)
	}
	return nil
}

func (s *Store) DeleteAdmin(ctx context.Context, id int64) error {
	result, err := s.execContext(ctx, `DELETE FROM admins WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete admin: %w", err)
	}
	return requireAffected(result, "admin")
}

// CreateSession persists only the caller-supplied token hash; raw session
// tokens never cross the storage boundary.
func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, session domain.Session) error {
	if len(tokenHash) == 0 {
		return fmt.Errorf("create session: token hash is empty")
	}
	if session.PasswordVersion < 1 {
		return fmt.Errorf("create session: %w", ErrCredentialsChanged)
	}
	result, err := s.execContext(ctx, `
		INSERT INTO admin_sessions(token_hash, admin_id, password_version, csrf, expires_at, created_at)
		SELECT ?, a.id, a.password_version, ?, ?, ?
		FROM admins a
		WHERE a.id = ? AND a.password_version = ?`,
		tokenHash, session.CSRF, timestamp(session.ExpiresAt), timestamp(s.now()),
		session.AdminID, session.PasswordVersion,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read create session result: %w", err)
	}
	if changed == 0 {
		return fmt.Errorf("create session: %w", ErrCredentialsChanged)
	}
	return nil
}

func (s *Store) CreateAdminSession(ctx context.Context, tokenHash []byte, session domain.Session) error {
	return s.CreateSession(ctx, tokenHash, session)
}

// GetSessionByHash returns only live sessions. Expired rows are deliberately
// indistinguishable from missing rows to keep authentication callers simple.
func (s *Store) GetSessionByHash(ctx context.Context, tokenHash []byte) (domain.Session, error) {
	var session domain.Session
	var expiresAt int64
	err := s.queryRowContext(ctx, `
		SELECT s.admin_id, a.username, s.password_version, s.csrf, s.expires_at
		FROM admin_sessions s
		JOIN admins a ON a.id = s.admin_id AND a.password_version = s.password_version
		WHERE s.token_hash = ? AND s.expires_at > ?`,
		tokenHash, timestamp(s.now()),
	).Scan(&session.AdminID, &session.Username, &session.PasswordVersion, &session.CSRF, &expiresAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.Session{}, ErrNotFound
		}
		return domain.Session{}, fmt.Errorf("get session: %w", err)
	}
	session.ExpiresAt = timeFromTimestamp(expiresAt)
	return session, nil
}

func (s *Store) GetAdminSessionByHash(ctx context.Context, tokenHash []byte) (domain.Session, error) {
	return s.GetSessionByHash(ctx, tokenHash)
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	if _, err := s.execContext(ctx, `DELETE FROM admin_sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (s *Store) DeleteAdminSession(ctx context.Context, tokenHash []byte) error {
	return s.DeleteSession(ctx, tokenHash)
}

func (s *Store) DeleteAdminSessions(ctx context.Context, adminID int64) error {
	if _, err := s.execContext(ctx, `DELETE FROM admin_sessions WHERE admin_id = ?`, adminID); err != nil {
		return fmt.Errorf("delete admin sessions: %w", err)
	}
	return nil
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	result, err := s.execContext(ctx, `DELETE FROM admin_sessions WHERE expires_at <= ?`, timestamp(s.now()))
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted sessions: %w", err)
	}
	return count, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAdmin(scanner rowScanner) (domain.Admin, error) {
	var admin domain.Admin
	var createdAt int64
	if err := scanner.Scan(&admin.ID, &admin.Username, &admin.PasswordHash, &admin.PasswordVersion, &createdAt); err != nil {
		if err == sql.ErrNoRows {
			return domain.Admin{}, ErrNotFound
		}
		return domain.Admin{}, fmt.Errorf("scan admin: %w", err)
	}
	admin.CreatedAt = timeFromTimestamp(createdAt)
	return admin, nil
}

func requireAffected(result sql.Result, resource string) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected %s rows: %w", resource, err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

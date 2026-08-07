package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"icloud-api/internal/domain"
)

const appleWebSessionColumns = `
	account_id, session_ciphertext, apple_id, region, authenticated,
	last_validated_at, created_at, updated_at`

// UpsertAppleWebSession creates or replaces the encrypted Apple web session
// for an account while preserving the original creation time.
func (s *Store) UpsertAppleWebSession(
	ctx context.Context,
	session domain.AppleWebSession,
) (domain.AppleWebSession, error) {
	if session.AccountID < 1 {
		return domain.AppleWebSession{}, fmt.Errorf("upsert apple web session: account id must be positive")
	}
	if strings.TrimSpace(session.Ciphertext) == "" {
		return domain.AppleWebSession{}, fmt.Errorf("upsert apple web session: ciphertext is empty")
	}
	appleID := strings.TrimSpace(session.AppleID)
	if appleID == "" {
		return domain.AppleWebSession{}, fmt.Errorf("upsert apple web session: apple id is empty")
	}

	now := s.now()
	_, err := s.db.ExecContext(ctx, `
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
		session.AccountID, session.Ciphertext, appleID, strings.TrimSpace(session.Region),
		session.Authenticated, nullableTimestamp(session.LastValidatedAt), timestamp(now), timestamp(now),
	)
	if err != nil {
		return domain.AppleWebSession{}, fmt.Errorf("upsert apple web session: %w", err)
	}
	return s.GetAppleWebSession(ctx, session.AccountID)
}

func (s *Store) GetAppleWebSession(ctx context.Context, accountID int64) (domain.AppleWebSession, error) {
	return scanAppleWebSession(s.db.QueryRowContext(ctx, `
		SELECT `+appleWebSessionColumns+`
		FROM apple_web_sessions WHERE account_id = ?`, accountID))
}

func (s *Store) DeleteAppleWebSession(ctx context.Context, accountID int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM apple_web_sessions WHERE account_id = ?`, accountID)
	if err != nil {
		return fmt.Errorf("delete apple web session: %w", err)
	}
	return requireAffected(result, "apple web session")
}

func scanAppleWebSession(scanner rowScanner) (domain.AppleWebSession, error) {
	var session domain.AppleWebSession
	var authenticated int
	var lastValidatedAt sql.NullInt64
	var createdAt, updatedAt int64
	if err := scanner.Scan(
		&session.AccountID, &session.Ciphertext, &session.AppleID, &session.Region,
		&authenticated, &lastValidatedAt, &createdAt, &updatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return domain.AppleWebSession{}, ErrNotFound
		}
		return domain.AppleWebSession{}, fmt.Errorf("scan apple web session: %w", err)
	}
	session.Authenticated = authenticated != 0
	session.LastValidatedAt = timePtr(lastValidatedAt)
	session.CreatedAt = timeFromTimestamp(createdAt)
	session.UpdatedAt = timeFromTimestamp(updatedAt)
	return session, nil
}

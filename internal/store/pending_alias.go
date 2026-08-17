package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"icloud-api/internal/domain"
)

// ListPendingAliasAPIKeysByAccount returns one-time API keys that are ready
// for an administrator to retrieve. Aliases waiting for Apple directory
// confirmation are intentionally withheld until the confirmation workflow
// publishes them.
func (s *Store) ListPendingAliasAPIKeysByAccount(
	ctx context.Context,
	accountID int64,
) ([]domain.PendingAliasAPIKey, error) {
	if accountID < 1 {
		return nil, fmt.Errorf("list pending automatic alias keys: account ID must be positive")
	}
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
		pending, scanErr := scanPendingAliasAPIKey(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, pending)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending automatic alias keys: %w", err)
	}
	return result, nil
}

// ListPendingAliasAPIKeys is the short compatibility spelling used by older
// service adapters.
func (s *Store) ListPendingAliasAPIKeys(ctx context.Context, accountID int64) ([]domain.PendingAliasAPIKey, error) {
	return s.ListPendingAliasAPIKeysByAccount(ctx, accountID)
}

// CountPendingAliasAPIKeysByAccount reports pending keys visible to the
// administrator retrieval flow.
func (s *Store) CountPendingAliasAPIKeysByAccount(ctx context.Context, accountID int64) (int, error) {
	if accountID < 1 {
		return 0, fmt.Errorf("count pending automatic alias keys: account ID must be positive")
	}
	var count int
	if err := s.queryRowContext(ctx, `
		SELECT COUNT(*)
		FROM pending_alias_api_keys p
		JOIN aliases al ON al.id = p.alias_id
		WHERE al.account_id = ?
		  AND NOT (al.enabled = FALSE AND al.last_sync_error = ?)`,
		accountID, domain.AppleAliasConfirmationPending).Scan(&count); err != nil {
		return 0, fmt.Errorf("count pending automatic alias keys: %w", err)
	}
	return count, nil
}

// DeletePendingAliasAPIKeys acknowledges only the supplied aliases for the
// specified account. It never deletes a key belonging to another account.
func (s *Store) DeletePendingAliasAPIKeys(ctx context.Context, accountID int64, aliasIDs []int64) error {
	if accountID < 1 {
		return fmt.Errorf("delete pending automatic alias keys: account ID must be positive")
	}
	if len(aliasIDs) == 0 {
		return nil
	}
	placeholders := make([]string, len(aliasIDs))
	args := make([]any, 0, len(aliasIDs)+3)
	for index, id := range aliasIDs {
		if id < 1 {
			return fmt.Errorf("delete pending automatic alias keys: alias id must be positive")
		}
		placeholders[index] = "?"
		args = append(args, id)
	}
	args = append(args, accountID, domain.AppleAliasConfirmationPending)
	_, err := s.execContext(ctx, `
		DELETE FROM pending_alias_api_keys
		WHERE alias_id IN (`+strings.Join(placeholders, ",")+`)
		  AND alias_id IN (
			SELECT id FROM aliases
			WHERE account_id = ?
			  AND NOT (enabled = FALSE AND last_sync_error = ?)
		  )`, args...)
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
	var groupID sql.NullInt64
	var mailboxUIDValidity, mailboxUIDNext int64
	var lastSyncedAt, lastAccessedAt, latestReceivedAt sql.NullInt64
	var createdAt, updatedAt, pendingCreatedAt int64
	if err := scanner.Scan(
		&alias.ID, &alias.AccountID, &alias.AccountEmail, &alias.Address, &alias.Label,
		&groupID, &alias.GroupName,
		&alias.APIKeyHash, &alias.APIKeyPrefix, &alias.CredentialMode, &alias.CredentialCiphertext,
		&alias.IMAPPasswordHash, &alias.OAuthClientID, &alias.RefreshTokenHash,
		&alias.CredentialVersion, &mailboxUIDValidity, &mailboxUIDNext,
		&enabled, &alias.LastSyncStatus, &alias.LastSyncError,
		&lastSyncedAt, &lastAccessedAt, &createdAt, &updatedAt, &latestReceivedAt,
		&pending.APIKeyCiphertext, &pendingCreatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return domain.PendingAliasAPIKey{}, ErrNotFound
		}
		return domain.PendingAliasAPIKey{}, fmt.Errorf("scan pending automatic alias key: %w", err)
	}
	if mailboxUIDValidity < 1 || mailboxUIDValidity > int64(^uint32(0)) ||
		mailboxUIDNext < 1 || mailboxUIDNext > int64(^uint32(0)) {
		return domain.PendingAliasAPIKey{}, fmt.Errorf("scan pending automatic alias key: invalid mailbox UID state")
	}
	alias.Enabled = enabled
	if groupID.Valid {
		value := groupID.Int64
		if value < 1 {
			return domain.PendingAliasAPIKey{}, fmt.Errorf("scan pending automatic alias key: invalid group ID")
		}
		alias.GroupID = &value
	}
	alias.MailboxUIDValidity = uint32(mailboxUIDValidity)
	alias.MailboxUIDNext = uint32(mailboxUIDNext)
	alias.LastSyncedAt = timePtr(lastSyncedAt)
	alias.LastAccessedAt = timePtr(lastAccessedAt)
	alias.CreatedAt = timeFromTimestamp(createdAt)
	alias.UpdatedAt = timeFromTimestamp(updatedAt)
	alias.LatestReceivedAt = timePtr(latestReceivedAt)
	pending.Alias = alias
	pending.CreatedAt = timeFromTimestamp(pendingCreatedAt)
	return pending, nil
}

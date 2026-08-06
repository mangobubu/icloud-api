package store

import (
	"context"
	"database/sql"
	"fmt"

	"icloud-api/internal/domain"
)

// GetMailboxBindingByAPIKeyHash resolves exactly one alias, its owning account,
// and that alias's sole latest-message row in one consistent read transaction.
// Disabled records are returned so the HTTP layer can apply one uniform policy.
func (s *Store) GetMailboxBindingByAPIKeyHash(
	ctx context.Context,
	apiKeyHash []byte,
) (domain.MailboxBinding, error) {
	if len(apiKeyHash) == 0 {
		return domain.MailboxBinding{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.MailboxBinding{}, fmt.Errorf("begin mailbox binding lookup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var binding domain.MailboxBinding
	var aliasEnabled, accountEnabled int
	var aliasLastSynced, aliasLastAccessed sql.NullInt64
	var aliasCreatedAt, aliasUpdatedAt int64
	var accountLastSynced sql.NullInt64
	var accountCreatedAt, accountUpdatedAt int64
	err = tx.QueryRowContext(ctx, `
		SELECT
			al.id, al.account_id, a.email, al.address, al.label, al.api_key_hash,
			al.api_key_prefix, al.enabled, al.last_sync_status, al.last_sync_error,
			al.last_synced_at, al.last_accessed_at, al.created_at, al.updated_at,
			a.id, a.name, a.email, a.imap_host, a.imap_port, a.imap_username,
			a.password_ciphertext, a.enabled, a.last_sync_status, a.last_sync_error,
			a.last_synced_at, a.created_at, a.updated_at,
			(SELECT COUNT(*) FROM aliases own WHERE own.account_id = a.id)
		FROM aliases al
		JOIN accounts a ON a.id = al.account_id
		WHERE al.api_key_hash = ?`, apiKeyHash,
	).Scan(
		&binding.Alias.ID, &binding.Alias.AccountID, &binding.Alias.AccountEmail,
		&binding.Alias.Address, &binding.Alias.Label, &binding.Alias.APIKeyHash,
		&binding.Alias.APIKeyPrefix, &aliasEnabled, &binding.Alias.LastSyncStatus,
		&binding.Alias.LastSyncError, &aliasLastSynced, &aliasLastAccessed,
		&aliasCreatedAt, &aliasUpdatedAt,
		&binding.Account.ID, &binding.Account.Name, &binding.Account.Email,
		&binding.Account.IMAPHost, &binding.Account.IMAPPort, &binding.Account.IMAPUsername,
		&binding.Account.PasswordCiphertext, &accountEnabled,
		&binding.Account.LastSyncStatus, &binding.Account.LastSyncError,
		&accountLastSynced, &accountCreatedAt, &accountUpdatedAt, &binding.Account.AliasCount,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.MailboxBinding{}, ErrNotFound
		}
		return domain.MailboxBinding{}, fmt.Errorf("get mailbox binding: %w", err)
	}

	binding.Alias.Enabled = aliasEnabled != 0
	binding.Alias.LastSyncedAt = timePtr(aliasLastSynced)
	binding.Alias.LastAccessedAt = timePtr(aliasLastAccessed)
	binding.Alias.CreatedAt = timeFromTimestamp(aliasCreatedAt)
	binding.Alias.UpdatedAt = timeFromTimestamp(aliasUpdatedAt)
	binding.Account.Enabled = accountEnabled != 0
	binding.Account.LastSyncedAt = timePtr(accountLastSynced)
	binding.Account.CreatedAt = timeFromTimestamp(accountCreatedAt)
	binding.Account.UpdatedAt = timeFromTimestamp(accountUpdatedAt)

	message, err := scanLatestMessage(tx.QueryRowContext(ctx,
		`SELECT `+latestMessageColumns+` FROM latest_messages WHERE alias_id = ?`,
		binding.Alias.ID,
	))
	if err != nil && err != ErrNotFound {
		return domain.MailboxBinding{}, fmt.Errorf("get mailbox binding message: %w", err)
	}
	if err == nil {
		binding.Message = &message
		receivedAt := message.InternalDate
		binding.Alias.LatestReceivedAt = &receivedAt
	}

	if err := tx.Commit(); err != nil {
		return domain.MailboxBinding{}, fmt.Errorf("commit mailbox binding lookup: %w", err)
	}
	return binding, nil
}

func (s *Store) MailboxBindingByAPIKeyHash(
	ctx context.Context,
	apiKeyHash []byte,
) (domain.MailboxBinding, error) {
	return s.GetMailboxBindingByAPIKeyHash(ctx, apiKeyHash)
}

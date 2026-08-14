package store

import (
	"context"
	"database/sql"
	"fmt"

	"icloud-api/internal/domain"
)

// GetMailboxBindingByAPIKeyHash resolves exactly one alias and its owning
// account in one consistent read transaction. Disabled records are returned so
// the HTTP layer can apply one uniform policy.
func (s *Store) GetMailboxBindingByAPIKeyHash(
	ctx context.Context,
	apiKeyHash []byte,
) (domain.MailboxBinding, error) {
	if len(apiKeyHash) == 0 {
		return domain.MailboxBinding{}, ErrNotFound
	}
	txOptions := &sql.TxOptions{ReadOnly: true}
	if s.dialect == dialectPostgres {
		txOptions.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.db.BeginTx(ctx, txOptions)
	if err != nil {
		return domain.MailboxBinding{}, fmt.Errorf("begin mailbox binding lookup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var binding domain.MailboxBinding
	binding.Alias, err = scanAlias(s.txQueryRowContext(ctx, tx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.api_key_hash = ?`, apiKeyHash,
	))
	if err != nil {
		if err == ErrNotFound {
			return domain.MailboxBinding{}, ErrNotFound
		}
		return domain.MailboxBinding{}, fmt.Errorf("get mailbox binding: %w", err)
	}
	binding.Account, err = scanAccount(s.txQueryRowContext(ctx, tx,
		`SELECT `+accountColumns+` FROM accounts a WHERE a.id = ?`, binding.Alias.AccountID,
	))
	if err != nil {
		return domain.MailboxBinding{}, fmt.Errorf("get mailbox binding account: %w", err)
	}
	// Legacy aliases keep a compact latest snapshot in the compatibility table.
	// Loading it here lets both restored v1 handlers and embedders use one
	// consistent credential/ownership lookup path. V2 callers simply receive a
	// nil message when no legacy row exists.
	message, messageErr := scanLatestMessage(s.txQueryRowContext(ctx, tx,
		`SELECT `+latestMessageColumns+` FROM latest_messages WHERE alias_id = ?`, binding.Alias.ID,
	))
	if messageErr == nil {
		binding.Message = &message
	} else if messageErr != ErrNotFound {
		return domain.MailboxBinding{}, fmt.Errorf("get mailbox binding latest message: %w", messageErr)
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

func (s *Store) GetMailboxBindingByOAuthClientID(ctx context.Context, clientID string) (domain.MailboxBinding, error) {
	alias, err := s.GetAliasByOAuthClientID(ctx, clientID)
	if err != nil {
		return domain.MailboxBinding{}, err
	}
	account, err := s.GetAccount(ctx, alias.AccountID)
	if err != nil {
		return domain.MailboxBinding{}, err
	}
	return domain.MailboxBinding{Alias: alias, Account: account}, nil
}

func (s *Store) GetMailboxBindingByIMAPPasswordHash(ctx context.Context, hash []byte) (domain.MailboxBinding, error) {
	alias, err := s.GetAliasByIMAPPasswordHash(ctx, hash)
	if err != nil {
		return domain.MailboxBinding{}, err
	}
	account, err := s.GetAccount(ctx, alias.AccountID)
	if err != nil {
		return domain.MailboxBinding{}, err
	}
	return domain.MailboxBinding{Alias: alias, Account: account}, nil
}

func (s *Store) GetMailboxBindingByAddress(ctx context.Context, address string) (domain.MailboxBinding, error) {
	alias, err := s.GetAliasByAddress(ctx, address)
	if err != nil {
		return domain.MailboxBinding{}, err
	}
	account, err := s.GetAccount(ctx, alias.AccountID)
	if err != nil {
		return domain.MailboxBinding{}, err
	}
	return domain.MailboxBinding{Alias: alias, Account: account}, nil
}

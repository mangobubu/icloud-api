package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/mail"
	"strings"

	"icloud-api/internal/domain"
)

// ImportAliases atomically creates aliases that are missing from an account.
// Existing aliases owned by the account are preserved exactly as stored. If
// any address belongs to another account, the full batch is rejected before
// any new alias is inserted.
func (s *Store) ImportAliases(
	ctx context.Context,
	accountID int64,
	candidates []domain.AliasImportCandidate,
) (domain.AliasImportResult, error) {
	normalized, err := normalizeAliasImportCandidates(candidates)
	if err != nil {
		return domain.AliasImportResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return domain.AliasImportResult{}, fmt.Errorf("begin alias import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockResult, err := tx.ExecContext(ctx, `UPDATE accounts SET updated_at = updated_at WHERE id = ?`, accountID)
	if err != nil {
		return domain.AliasImportResult{}, fmt.Errorf("lock account for alias import: %w", err)
	}
	if err := requireAffected(lockResult, "account"); err != nil {
		return domain.AliasImportResult{}, err
	}

	result := domain.AliasImportResult{
		Created:   make([]domain.Alias, 0, len(normalized)),
		Existing:  make([]domain.Alias, 0, len(normalized)),
		Conflicts: make([]domain.AliasImportConflict, 0),
	}
	pending := make([]domain.AliasImportCandidate, 0, len(normalized))
	for _, candidate := range normalized {
		existing, findErr := getAliasByAddressTx(ctx, tx, candidate.Address)
		switch {
		case findErr == nil && existing.AccountID == accountID:
			result.Existing = append(result.Existing, existing)
			continue
		case findErr == nil:
			result.Conflicts = append(result.Conflicts, domain.AliasImportConflict{
				Address:              candidate.Address,
				ExistingAliasID:      existing.ID,
				ExistingAccountID:    existing.AccountID,
				ExistingAccountEmail: existing.AccountEmail,
			})
			continue
		case findErr != ErrNotFound:
			return domain.AliasImportResult{}, fmt.Errorf("find alias %q during import: %w", candidate.Address, findErr)
		}
		pending = append(pending, candidate)
	}
	if len(result.Conflicts) > 0 {
		return result, fmt.Errorf("import aliases: %w", ErrAliasOwnershipConflict)
	}

	var enabledCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM aliases WHERE account_id = ? AND enabled = 1`, accountID,
	).Scan(&enabledCount); err != nil {
		return domain.AliasImportResult{}, fmt.Errorf("count enabled aliases before import: %w", err)
	}

	now := s.now()
	for _, candidate := range pending {
		enabled := candidate.Active && enabledCount < domain.MaxEnabledAliasesPerAccount
		if enabled {
			enabledCount++
		} else if candidate.Active {
			result.ImportedDisabledCount++
		}
		insertResult, err := tx.ExecContext(ctx, `
			INSERT INTO aliases(
				account_id, address, label, api_key_hash, api_key_prefix, enabled,
				last_sync_status, last_sync_error, last_synced_at,
				last_accessed_at, created_at, updated_at
			) VALUES(?, ?, ?, ?, ?, ?, ?, '', NULL, NULL, ?, ?)`,
			accountID, candidate.Address, candidate.Label, candidate.APIKeyHash,
			candidate.APIKeyPrefix, enabled, domain.SyncStatusPending,
			timestamp(now), timestamp(now),
		)
		if err != nil {
			return domain.AliasImportResult{}, fmt.Errorf("import alias %q: %w", candidate.Address, err)
		}
		id, err := insertResult.LastInsertId()
		if err != nil {
			return domain.AliasImportResult{}, fmt.Errorf("read imported alias id for %q: %w", candidate.Address, err)
		}
		created, err := getAliasByIDTx(ctx, tx, id)
		if err != nil {
			return domain.AliasImportResult{}, fmt.Errorf("read imported alias %q: %w", candidate.Address, err)
		}
		result.Created = append(result.Created, created)
	}

	if err := tx.Commit(); err != nil {
		return domain.AliasImportResult{}, fmt.Errorf("commit alias import: %w", err)
	}
	return result, nil
}

func normalizeAliasImportCandidates(candidates []domain.AliasImportCandidate) ([]domain.AliasImportCandidate, error) {
	normalized := make([]domain.AliasImportCandidate, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for index, candidate := range candidates {
		candidate.Address = domain.NormalizeEmail(candidate.Address)
		parsed, err := mail.ParseAddress(candidate.Address)
		if candidate.Address == "" || err != nil || domain.NormalizeEmail(parsed.Address) != candidate.Address {
			return nil, fmt.Errorf("import aliases: candidate %d has invalid address", index)
		}
		if _, exists := seen[candidate.Address]; exists {
			return nil, fmt.Errorf("import aliases: duplicate candidate address %q", candidate.Address)
		}
		if len(candidate.APIKeyHash) == 0 {
			return nil, fmt.Errorf("import aliases: candidate %q has empty api key hash", candidate.Address)
		}
		candidate.APIKeyPrefix = strings.TrimSpace(candidate.APIKeyPrefix)
		if candidate.APIKeyPrefix == "" {
			return nil, fmt.Errorf("import aliases: candidate %q has empty api key prefix", candidate.Address)
		}
		candidate.Label = strings.TrimSpace(candidate.Label)
		candidate.APIKeyHash = append([]byte(nil), candidate.APIKeyHash...)
		seen[candidate.Address] = struct{}{}
		normalized = append(normalized, candidate)
	}
	return normalized, nil
}

func getAliasByAddressTx(ctx context.Context, tx *sql.Tx, address string) (domain.Alias, error) {
	return scanAlias(tx.QueryRowContext(ctx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.address = ? COLLATE NOCASE`, address,
	))
}

func getAliasByIDTx(ctx context.Context, tx *sql.Tx, id int64) (domain.Alias, error) {
	return scanAlias(tx.QueryRowContext(ctx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.id = ?`, id,
	))
}

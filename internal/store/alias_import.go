package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/mail"
	"sort"
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

	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, accountID)
	if err != nil {
		return domain.AliasImportResult{}, fmt.Errorf("lock account for alias import: %w", err)
	}

	result := domain.AliasImportResult{
		Created:   make([]domain.Alias, 0, len(normalized)),
		Existing:  make([]domain.Alias, 0, len(normalized)),
		Conflicts: make([]domain.AliasImportConflict, 0),
	}
	pending := make([]domain.AliasImportCandidate, 0, len(normalized))
	for _, candidate := range normalized {
		existing, findErr := s.getAliasByAddressTx(ctx, tx, candidate.Address)
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
	if err := s.txQueryRowContext(ctx, tx, `
		SELECT COUNT(*) FROM aliases WHERE account_id = ? AND enabled = TRUE`, accountID,
	).Scan(&enabledCount); err != nil {
		return domain.AliasImportResult{}, fmt.Errorf("count enabled aliases before import: %w", err)
	}

	type insertion struct {
		candidate domain.AliasImportCandidate
		enabled   bool
	}
	insertions := make([]insertion, 0, len(pending))
	for _, candidate := range pending {
		enabled := candidate.Active && enabledCount < domain.MaxEnabledAliasesPerAccount
		if enabled {
			enabledCount++
		}
		insertions = append(insertions, insertion{candidate: candidate, enabled: enabled})
	}
	// PostgreSQL takes speculative unique-index locks as rows are inserted.
	// Every batch uses the same address order so overlapping cross-account
	// imports cannot acquire those locks in opposite order and deadlock.
	sort.Slice(insertions, func(left, right int) bool {
		return insertions[left].candidate.Address < insertions[right].candidate.Address
	})

	now := s.now()
	createdEnabled := false
	createdByAddress := make(map[string]domain.Alias, len(insertions))
	for _, item := range insertions {
		candidate := item.candidate
		enabled := item.enabled
		var id int64
		err := s.txQueryRowContext(ctx, tx, `
			INSERT INTO aliases(
				account_id, address, label, api_key_hash, api_key_prefix, enabled,
				last_sync_status, last_sync_error, last_synced_at,
				last_accessed_at, created_at, updated_at
			) VALUES(?, ?, ?, ?, ?, ?, ?, '', NULL, NULL, ?, ?)
			ON CONFLICT(address) DO NOTHING
			RETURNING id`,
			accountID, candidate.Address, candidate.Label, candidate.APIKeyHash,
			candidate.APIKeyPrefix, enabled, domain.SyncStatusPending,
			timestamp(now), timestamp(now),
		).Scan(&id)
		if err == sql.ErrNoRows {
			existing, findErr := s.getAliasByAddressTx(ctx, tx, candidate.Address)
			if findErr != nil {
				return domain.AliasImportResult{}, fmt.Errorf(
					"read concurrent owner of alias %q: %w", candidate.Address, findErr,
				)
			}
			if existing.AccountID == accountID {
				result.Existing = append(result.Existing, existing)
				continue
			}
			result.Created = result.Created[:0]
			result.ImportedDisabledCount = 0
			result.Conflicts = append(result.Conflicts, domain.AliasImportConflict{
				Address:              candidate.Address,
				ExistingAliasID:      existing.ID,
				ExistingAccountID:    existing.AccountID,
				ExistingAccountEmail: existing.AccountEmail,
			})
			return result, fmt.Errorf("import aliases: %w", ErrAliasOwnershipConflict)
		}
		if err != nil {
			return domain.AliasImportResult{}, fmt.Errorf("import alias %q: %w", candidate.Address, err)
		}
		created, err := s.getAliasByIDTx(ctx, tx, id)
		if err != nil {
			return domain.AliasImportResult{}, fmt.Errorf("read imported alias %q: %w", candidate.Address, err)
		}
		createdByAddress[candidate.Address] = created
		if enabled {
			createdEnabled = true
		} else if candidate.Active {
			result.ImportedDisabledCount++
		}
	}
	// Keep the public result in candidate order even though the writes use the
	// deterministic address order above.
	for _, candidate := range pending {
		if created, ok := createdByAddress[candidate.Address]; ok {
			result.Created = append(result.Created, created)
		}
	}
	if createdEnabled {
		if _, err := s.txExecContext(ctx, tx,
			`DELETE FROM imap_sync_states WHERE account_id = ?`, accountID,
		); err != nil {
			return domain.AliasImportResult{}, fmt.Errorf("reset IMAP cursor after alias import: %w", err)
		}
		if _, err := s.bumpAccountVersionTx(ctx, tx, accountID, accountVersion); err != nil {
			return domain.AliasImportResult{}, fmt.Errorf("advance account version after alias import: %w", err)
		}
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
		candidate.Label = strings.TrimSpace(sanitizePostgresText(candidate.Label))
		candidate.APIKeyHash = append([]byte(nil), candidate.APIKeyHash...)
		seen[candidate.Address] = struct{}{}
		normalized = append(normalized, candidate)
	}
	return normalized, nil
}

func (s *Store) getAliasByAddressTx(ctx context.Context, tx *sql.Tx, address string) (domain.Alias, error) {
	return scanAlias(s.txQueryRowContext(ctx, tx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.address = ?`, address,
	))
}

func (s *Store) getAliasByIDTx(ctx context.Context, tx *sql.Tx, id int64) (domain.Alias, error) {
	return scanAlias(s.txQueryRowContext(ctx, tx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.id = ?`, id,
	))
}

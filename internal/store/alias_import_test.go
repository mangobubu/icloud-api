package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestImportAliasesCreatesActiveAndInactiveAliases(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")

	result, err := db.ImportAliases(ctx, account.ID, []domain.AliasImportCandidate{
		{
			Address: "  New.Active@ICLOUD.COM  ", Label: " Active relay ",
			APIKeyHash: []byte("active-import-hash"), APIKeyPrefix: " active-prefix ", Active: true,
		},
		{
			Address: "INACTIVE@icloud.com", Label: " Inactive relay ",
			APIKeyHash: []byte("inactive-import-hash"), APIKeyPrefix: " inactive-prefix ", Active: false,
		},
	})
	if err != nil {
		t.Fatalf("import aliases: %v", err)
	}
	if len(result.Created) != 2 || len(result.Existing) != 0 || len(result.Conflicts) != 0 ||
		result.ImportedDisabledCount != 0 {
		t.Fatalf("import result = %#v, want two created aliases and no capacity-disabled aliases", result)
	}

	createdByAddress := make(map[string]domain.Alias, len(result.Created))
	for _, alias := range result.Created {
		createdByAddress[alias.Address] = alias
	}
	active, ok := createdByAddress["new.active@icloud.com"]
	if !ok {
		t.Fatalf("created aliases cannot be mapped by normalized address: %#v", result.Created)
	}
	if active.AccountID != account.ID || active.AccountEmail != account.Email || !active.Enabled ||
		active.Label != "Active relay" || active.APIKeyPrefix != "active-prefix" ||
		!bytes.Equal(active.APIKeyHash, []byte("active-import-hash")) ||
		active.LastSyncStatus != domain.SyncStatusPending {
		t.Fatalf("active imported alias = %#v", active)
	}
	inactive, ok := createdByAddress["inactive@icloud.com"]
	if !ok {
		t.Fatalf("created aliases cannot be mapped by normalized address: %#v", result.Created)
	}
	if inactive.AccountID != account.ID || inactive.AccountEmail != account.Email || inactive.Enabled ||
		inactive.Label != "Inactive relay" || inactive.APIKeyPrefix != "inactive-prefix" ||
		!bytes.Equal(inactive.APIKeyHash, []byte("inactive-import-hash")) ||
		inactive.LastSyncStatus != domain.SyncStatusPending {
		t.Fatalf("inactive imported alias = %#v", inactive)
	}

	for address, want := range createdByAddress {
		got, err := db.GetAliasByAddress(ctx, address)
		if err != nil {
			t.Fatalf("get imported alias %q: %v", address, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("stored alias %q = %#v, want %#v", address, got, want)
		}
	}
}

func TestImportAliasesPreservesExistingAndRollsBackOnOtherAccountConflict(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	target := createAccount(t, ctx, db, "Target", "target@icloud.com")
	other := createAccount(t, ctx, db, "Other", "other@icloud.com")
	syncedAt := time.Date(2026, 8, 7, 9, 30, 0, 0, time.UTC)
	accessedAt := syncedAt.Add(time.Hour)

	owned, err := db.CreateAlias(ctx, domain.Alias{
		AccountID: target.ID, Address: "kept@icloud.com", Label: "keep every field",
		APIKeyHash: []byte("kept-hash"), APIKeyPrefix: "kept-prefix", Enabled: false,
		LastSyncStatus: domain.SyncStatusError, LastSyncError: "kept error",
		LastSyncedAt: &syncedAt, LastAccessedAt: &accessedAt,
	})
	if err != nil {
		t.Fatalf("create existing target alias: %v", err)
	}
	conflicting := createAlias(t, ctx, db, other.ID, "conflict@icloud.com", []byte("conflict-hash"))

	result, err := db.ImportAliases(ctx, target.ID, []domain.AliasImportCandidate{
		{
			Address: "  KEPT@ICLOUD.COM ", Label: "replacement label",
			APIKeyHash: []byte("replacement-hash"), APIKeyPrefix: "replacement", Active: true,
		},
		{
			Address: "new@icloud.com", Label: "must be rolled back",
			APIKeyHash: []byte("new-after-conflict-hash"), APIKeyPrefix: "new", Active: true,
		},
		{
			Address: " Conflict@iCloud.com ", Label: "causes batch rollback",
			APIKeyHash: []byte("unused-conflict-hash"), APIKeyPrefix: "unused", Active: false,
		},
	})
	if !errors.Is(err, store.ErrAliasOwnershipConflict) {
		t.Fatalf("import aliases with ownership conflict error = %v, want ErrAliasOwnershipConflict", err)
	}
	if len(result.Created) != 0 {
		t.Fatalf("created aliases after ownership conflict = %#v, want none", result.Created)
	}
	if len(result.Existing) != 1 || !reflect.DeepEqual(result.Existing[0], owned) {
		t.Fatalf("existing aliases = %#v, want preserved %#v", result.Existing, owned)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("conflicts = %#v, want one", result.Conflicts)
	}
	conflict := result.Conflicts[0]
	if conflict.Address != "conflict@icloud.com" || conflict.ExistingAliasID != conflicting.ID ||
		conflict.ExistingAccountID != other.ID || conflict.ExistingAccountEmail != other.Email {
		t.Fatalf("address conflict = %#v", conflict)
	}

	stillOwned, err := db.GetAlias(ctx, owned.ID)
	if err != nil {
		t.Fatalf("get existing target alias after import: %v", err)
	}
	if !reflect.DeepEqual(stillOwned, owned) {
		t.Fatalf("existing target alias changed: got %#v, want %#v", stillOwned, owned)
	}
	stillConflicting, err := db.GetAlias(ctx, conflicting.ID)
	if err != nil {
		t.Fatalf("get conflicting alias after import: %v", err)
	}
	if !reflect.DeepEqual(stillConflicting, conflicting) {
		t.Fatalf("conflicting alias changed: got %#v, want %#v", stillConflicting, conflicting)
	}
	if _, err := db.GetAliasByAddress(ctx, "new@icloud.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("new alias from rolled-back batch lookup error = %v, want ErrNotFound", err)
	}
}

func TestImportAliasesRejectsInvalidBatchWithoutWriting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		candidates []domain.AliasImportCandidate
	}{
		{
			name: "duplicate normalized address",
			candidates: []domain.AliasImportCandidate{
				importCandidate("duplicate@icloud.com", "duplicate-one", true),
				importCandidate("  DUPLICATE@ICLOUD.COM ", "duplicate-two", false),
			},
		},
		{
			name: "invalid address",
			candidates: []domain.AliasImportCandidate{
				importCandidate("valid-before-invalid@icloud.com", "valid-before-invalid", true),
				importCandidate("not-an-email", "invalid-address", true),
			},
		},
		{
			name: "empty api key hash",
			candidates: []domain.AliasImportCandidate{
				importCandidate("valid-before-empty-hash@icloud.com", "valid-before-empty-hash", true),
				{Address: "empty-hash@icloud.com", APIKeyPrefix: "empty-hash", Active: true},
			},
		},
		{
			name: "blank api key prefix",
			candidates: []domain.AliasImportCandidate{
				importCandidate("valid-before-blank-prefix@icloud.com", "valid-before-blank-prefix", true),
				{Address: "blank-prefix@icloud.com", APIKeyHash: []byte("blank-prefix-hash"), APIKeyPrefix: "   ", Active: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := openTestStore(t)
			account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")

			if _, err := db.ImportAliases(ctx, account.ID, tt.candidates); err == nil {
				t.Fatal("invalid import batch unexpectedly succeeded")
			}
			assertAliasCounts(t, ctx, db, account.ID, 0, 0)
		})
	}
}

func TestImportAliasesAPIKeyConflictRollsBackEntireBatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	target := createAccount(t, ctx, db, "Target", "target@icloud.com")
	other := createAccount(t, ctx, db, "Other", "other@icloud.com")
	keyOwner := createAlias(t, ctx, db, other.ID, "key-owner@icloud.com", []byte("shared-unique-hash"))

	_, err := db.ImportAliases(ctx, target.ID, []domain.AliasImportCandidate{
		importCandidate("inserted-before-key-conflict@icloud.com", "fresh-hash", true),
		importCandidate("new-address-with-used-key@icloud.com", "shared-unique-hash", true),
	})
	if err == nil {
		t.Fatal("API key conflict import unexpectedly succeeded")
	}
	assertAliasCounts(t, ctx, db, target.ID, 0, 0)
	for _, address := range []string{
		"inserted-before-key-conflict@icloud.com",
		"new-address-with-used-key@icloud.com",
	} {
		if _, lookupErr := db.GetAliasByAddress(ctx, address); !errors.Is(lookupErr, store.ErrNotFound) {
			t.Fatalf("rolled-back alias %q lookup error = %v, want ErrNotFound", address, lookupErr)
		}
	}
	stillOwned, lookupErr := db.GetAlias(ctx, keyOwner.ID)
	if lookupErr != nil || !reflect.DeepEqual(stillOwned, keyOwner) {
		t.Fatalf("existing API key owner changed: alias=%#v err=%v", stillOwned, lookupErr)
	}
}

func TestImportAliasesPreservesAllAliasesAtEnabledCapacity(t *testing.T) {
	t.Parallel()

	t.Run("fills final enabled slot and permits inactive", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		db := openTestStore(t)
		account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
		insertEnabledAliasFixtures(t, ctx, db, account.ID, domain.MaxEnabledAliasesPerAccount-1)

		result, err := db.ImportAliases(ctx, account.ID, []domain.AliasImportCandidate{
			importCandidate("final-enabled@icloud.com", "final-enabled", true),
			importCandidate("inactive-after-limit@icloud.com", "inactive-after-limit", false),
		})
		if err != nil {
			t.Fatalf("import final enabled slot and inactive alias: %v", err)
		}
		if len(result.Created) != 2 {
			t.Fatalf("created aliases = %#v, want two", result.Created)
		}
		if result.ImportedDisabledCount != 0 {
			t.Fatalf("capacity-disabled count = %d, want 0", result.ImportedDisabledCount)
		}
		assertAliasCounts(t, ctx, db, account.ID, domain.MaxEnabledAliasesPerAccount+1, domain.MaxEnabledAliasesPerAccount)
	})

	t.Run("imports active alias as disabled when capacity is full", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		db := openTestStore(t)
		account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
		insertEnabledAliasFixtures(t, ctx, db, account.ID, domain.MaxEnabledAliasesPerAccount)

		inactive, err := db.ImportAliases(ctx, account.ID, []domain.AliasImportCandidate{
			importCandidate("inactive-at-limit@icloud.com", "inactive-at-limit", false),
		})
		if err != nil || len(inactive.Created) != 1 || inactive.Created[0].Enabled {
			t.Fatalf("import inactive alias at enabled limit: result=%#v err=%v", inactive, err)
		}
		if inactive.ImportedDisabledCount != 0 {
			t.Fatalf("inactive alias capacity-disabled count = %d, want 0", inactive.ImportedDisabledCount)
		}

		result, err := db.ImportAliases(ctx, account.ID, []domain.AliasImportCandidate{
			importCandidate("inactive-at-full-capacity@icloud.com", "inactive-at-full-capacity", false),
			importCandidate("enabled-over-limit@icloud.com", "enabled-over-limit", true),
		})
		if err != nil {
			t.Fatalf("import aliases at full enabled capacity: %v", err)
		}
		if len(result.Created) != 2 || result.ImportedDisabledCount != 1 {
			t.Fatalf("full-capacity import result = %#v, want two created and one capacity-disabled", result)
		}
		for _, alias := range result.Created {
			if alias.Enabled {
				t.Fatalf("alias %q was enabled beyond capacity", alias.Address)
			}
		}
		assertAliasCounts(t, ctx, db, account.ID, domain.MaxEnabledAliasesPerAccount+3, domain.MaxEnabledAliasesPerAccount)
	})
}

func TestImportAliasesStoresCompleteListBeyondEnabledCapacity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
	const remoteAliasCount = 1047
	candidates := make([]domain.AliasImportCandidate, 0, remoteAliasCount)
	for index := 0; index < remoteAliasCount; index++ {
		address := fmt.Sprintf("complete-%04d@icloud.com", index)
		key := fmt.Sprintf("complete-key-%04d", index)
		candidates = append(candidates, importCandidate(address, key, true))
	}

	result, err := db.ImportAliases(ctx, account.ID, candidates)
	if err != nil {
		t.Fatalf("import complete 1047-alias list: %v", err)
	}
	wantDisabled := remoteAliasCount - domain.MaxEnabledAliasesPerAccount
	if len(result.Created) != remoteAliasCount || len(result.Existing) != 0 || len(result.Conflicts) != 0 ||
		result.ImportedDisabledCount != wantDisabled {
		t.Fatalf("complete import result sizes = created %d, existing %d, conflicts %d, capacity-disabled %d; want %d, 0, 0, %d",
			len(result.Created), len(result.Existing), len(result.Conflicts), result.ImportedDisabledCount,
			remoteAliasCount, wantDisabled)
	}
	for index, alias := range result.Created {
		wantEnabled := index < domain.MaxEnabledAliasesPerAccount
		if alias.Enabled != wantEnabled {
			t.Fatalf("created alias %d enabled = %t, want %t", index, alias.Enabled, wantEnabled)
		}
	}
	assertAliasCounts(t, ctx, db, account.ID, remoteAliasCount, domain.MaxEnabledAliasesPerAccount)
}

func importCandidate(address, key string, active bool) domain.AliasImportCandidate {
	return domain.AliasImportCandidate{
		Address: address, Label: address, APIKeyHash: []byte(key), APIKeyPrefix: key, Active: active,
	}
}

func assertAliasCounts(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	accountID int64,
	wantTotal, wantEnabled int,
) {
	t.Helper()
	var total, enabled int
	if err := db.DB().QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(enabled), 0)
		FROM aliases WHERE account_id = ?`, accountID).Scan(&total, &enabled); err != nil {
		t.Fatalf("count aliases for account %d: %v", accountID, err)
	}
	if total != wantTotal || enabled != wantEnabled {
		t.Fatalf("alias counts = total %d, enabled %d; want %d, %d", total, enabled, wantTotal, wantEnabled)
	}
}

func insertEnabledAliasFixtures(t *testing.T, ctx context.Context, db *store.Store, accountID int64, count int) {
	t.Helper()
	now := time.Now().UTC().UnixNano()
	_, err := db.DB().ExecContext(ctx, `
		WITH RECURSIVE sequence(number) AS (
			SELECT 1
			UNION ALL
			SELECT number + 1 FROM sequence WHERE number < ?
		)
		INSERT INTO aliases(
			account_id, address, label, api_key_hash, api_key_prefix, enabled,
			last_sync_status, last_sync_error, last_synced_at,
			last_accessed_at, created_at, updated_at
		)
		SELECT ?,
			printf('limit-%d-%04d@icloud.com', ?, number),
			printf('limit fixture %04d', number),
			CAST(printf('limit-hash-%d-%04d', ?, number) AS BLOB),
			printf('limit-%04d', number),
			1, ?, '', NULL, NULL, ?, ?
		FROM sequence`, count, accountID, accountID, accountID, domain.SyncStatusPending, now, now)
	if err != nil {
		t.Fatalf("insert %d enabled alias fixtures: %v", count, err)
	}
	assertAliasCounts(t, ctx, db, accountID, count, count)
}

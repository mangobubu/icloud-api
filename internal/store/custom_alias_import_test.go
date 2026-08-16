package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

func configureStrictImportCredentials(t *testing.T, db *store.Store) {
	t.Helper()
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x79}, 32))
	if err != nil {
		t.Fatalf("create strict import cipher: %v", err)
	}
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, issueErr
	})
	db.ConfigureAliasCredentialRevealFactory(func(aliasID int64, ciphertext string) (string, error) {
		credentials, decryptErr := cipher.DecryptAliasCredentials(aliasID, ciphertext)
		return credentials.APIKey, decryptErr
	})
}

func createCustomImportAccount(t *testing.T, ctx context.Context, db *store.Store, suffix string, enabled bool) domain.Account {
	t.Helper()
	account, err := db.CreateAccount(ctx, domain.Account{
		Name:               "Custom import",
		Email:              "custom@" + suffix,
		MailboxType:        domain.MailboxTypeCustom,
		EmailSuffix:        suffix,
		IMAPHost:           "imap." + suffix,
		IMAPPort:           993,
		IMAPUsername:       "reader@" + suffix,
		PasswordCiphertext: "encrypted",
		Enabled:            enabled,
	})
	if err != nil {
		t.Fatalf("create custom import account: %v", err)
	}
	return account
}

func TestCustomAliasStrictImportChecksModeStateSuffixAndDuplicates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("iCloud mode", func(t *testing.T) {
		db := openTestStore(t)
		configureStrictImportCredentials(t, db)
		account := createAccount(t, ctx, db, "iCloud", "strict-icloud@icloud.com")
		_, _, err := db.ImportCustomAliasesWithCredentialsStrict(ctx, account.ID, []domain.AliasImportCandidate{{
			Address: "abcdefgh@custom.test", Active: true,
		}})
		if !errors.Is(err, store.ErrCustomMailboxRequired) {
			t.Fatalf("iCloud custom import error = %v, want custom mailbox required", err)
		}
	})

	t.Run("disabled custom mode", func(t *testing.T) {
		db := openTestStore(t)
		configureStrictImportCredentials(t, db)
		account := createCustomImportAccount(t, ctx, db, "disabled.test", false)
		_, _, err := db.ImportCustomAliasesWithCredentialsStrict(ctx, account.ID, []domain.AliasImportCandidate{{
			Address: "abcdefgh@disabled.test", Active: true,
		}})
		if !errors.Is(err, store.ErrAccountDisabled) {
			t.Fatalf("disabled custom import error = %v, want account disabled", err)
		}
	})

	t.Run("suffix and duplicate", func(t *testing.T) {
		db := openTestStore(t)
		configureStrictImportCredentials(t, db)
		account := createCustomImportAccount(t, ctx, db, "strict.test", true)
		_, _, err := db.ImportCustomAliasesWithCredentialsStrict(ctx, account.ID, []domain.AliasImportCandidate{{
			Address: "reader@strict.test", Active: true,
		}})
		if !errors.Is(err, store.ErrAliasIdentityConflict) {
			t.Fatalf("account identity import error = %v, want identity conflict", err)
		}
		_, _, err = db.ImportCustomAliasesWithCredentialsStrict(ctx, account.ID, []domain.AliasImportCandidate{{
			Address: "abcdefgh@other.test", Active: true,
		}})
		if !errors.Is(err, store.ErrAliasSuffixMismatch) {
			t.Fatalf("wrong suffix import error = %v, want suffix mismatch", err)
		}
		result, _, err := db.ImportCustomAliasesWithCredentialsStrict(ctx, account.ID, []domain.AliasImportCandidate{
			{Address: "abcdefgh@strict.test", Active: true},
			{Address: "ijklmnop@strict.test", Active: true},
		})
		if err != nil || len(result.Created) != 2 {
			t.Fatalf("strict custom import result = %#v, err=%v", result, err)
		}
		_, _, err = db.ImportCustomAliasesWithCredentialsStrict(ctx, account.ID, []domain.AliasImportCandidate{
			{Address: "qrstuvwx@strict.test", Active: true},
			{Address: "ABCDEFGH@STRICT.TEST", Active: true},
		})
		if !errors.Is(err, store.ErrAliasOwnershipConflict) {
			t.Fatalf("duplicate strict import error = %v, want ownership conflict", err)
		}
		aliases, listErr := db.ListAliasesByAccount(ctx, account.ID)
		if listErr != nil || len(aliases) != 2 {
			t.Fatalf("aliases after rejected batch = %d, err=%v; want 2", len(aliases), listErr)
		}
	})
}

func TestDisabledICloudMailboxStillAllowsDirectoryImports(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	configureStrictImportCredentials(t, db)
	account, err := db.CreateAccount(ctx, domain.Account{
		Name:               "Disabled iCloud import",
		Email:              "disabled-import@icloud.com",
		IMAPHost:           "imap.mail.me.com",
		IMAPPort:           993,
		IMAPUsername:       "disabled-import@icloud.com",
		PasswordCiphertext: "encrypted",
		Enabled:            false,
	})
	if err != nil {
		t.Fatalf("create disabled iCloud account: %v", err)
	}

	legacy, err := db.ImportAliases(ctx, account.ID, []domain.AliasImportCandidate{{
		Address:      "disabled-legacy@icloud.com",
		APIKeyHash:   []byte("disabled-legacy-hash"),
		APIKeyPrefix: "disabled-legacy",
		Active:       true,
	}})
	if err != nil || len(legacy.Created) != 1 {
		t.Fatalf("disabled iCloud legacy import result = %#v, err=%v", legacy, err)
	}
	generated, issued, err := db.ImportAliasesWithCredentials(ctx, account.ID, []domain.AliasImportCandidate{{
		Address: "disabled-v2@icloud.com", Active: true,
	}})
	if err != nil || len(generated.Created) != 1 || len(issued) != 1 {
		t.Fatalf("disabled iCloud credential import result = %#v, issued=%d, err=%v", generated, len(issued), err)
	}
}

func TestCustomAliasStrictImportRejectsOtherAccountEmailAndRollsBackBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	configureStrictImportCredentials(t, db)
	createAccount(t, ctx, db, "Conflicting primary", "collision@shared-domain.test")
	account := createCustomImportAccount(t, ctx, db, "shared-domain.test", true)

	result, issued, err := db.ImportCustomAliasesWithCredentialsStrict(ctx, account.ID, []domain.AliasImportCandidate{
		{Address: "would-be-created@shared-domain.test", Active: true},
		{Address: "COLLISION@SHARED-DOMAIN.TEST", Active: true},
	})
	if !errors.Is(err, store.ErrAliasIdentityConflict) {
		t.Fatalf("other account identity import error = %v, want identity conflict", err)
	}
	if len(result.Created) != 0 || len(issued) != 0 {
		t.Fatalf("rejected identity batch result = %#v, issued=%d", result, len(issued))
	}
	aliases, listErr := db.ListAliasesByAccount(ctx, account.ID)
	if listErr != nil || len(aliases) != 0 {
		t.Fatalf("aliases after rejected identity batch = %d, err=%v; want 0", len(aliases), listErr)
	}
	if _, lookupErr := db.GetAliasByAddress(ctx, "would-be-created@shared-domain.test"); !errors.Is(lookupErr, store.ErrNotFound) {
		t.Fatalf("non-conflicting alias survived rejected identity batch: %v", lookupErr)
	}
}

func TestStrictAliasImportRejectsWholeBatchAtCapacity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	configureStrictImportCredentials(t, db)
	account := createAccount(t, ctx, db, "Capacity", "strict-capacity@icloud.com")

	fill := make([]domain.AliasImportCandidate, 0, domain.MaxEnabledAliasesPerAccount)
	for index := 0; index < domain.MaxEnabledAliasesPerAccount; index++ {
		fill = append(fill, domain.AliasImportCandidate{
			Address:      fmt.Sprintf("capacity-%04d@icloud.com", index),
			APIKeyHash:   []byte(fmt.Sprintf("capacity-hash-%04d", index)),
			APIKeyPrefix: "capacity",
			Active:       true,
		})
	}
	result, err := db.ImportAliases(ctx, account.ID, fill)
	if err != nil || len(result.Created) != domain.MaxEnabledAliasesPerAccount {
		t.Fatalf("fill alias capacity created=%d err=%v", len(result.Created), err)
	}
	_, _, err = db.ImportAliasesWithCredentialsStrict(ctx, account.ID, []domain.AliasImportCandidate{{
		Address: "one-more@icloud.com", Active: true,
	}})
	if !errors.Is(err, store.ErrAliasLimit) {
		t.Fatalf("strict over-capacity error = %v, want alias limit", err)
	}
	aliases, listErr := db.ListAliasesByAccount(ctx, account.ID)
	if listErr != nil || len(aliases) != domain.MaxEnabledAliasesPerAccount {
		t.Fatalf("aliases after over-capacity batch = %d, err=%v", len(aliases), listErr)
	}
}

func TestCustomMailboxRejectsApplePersistencePaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createCustomImportAccount(t, ctx, db, "no-apple.test", true)

	if _, err := db.UpsertAppleWebSession(ctx, domain.AppleWebSession{
		AccountID:     account.ID,
		Ciphertext:    "as1.custom-mailbox",
		AppleID:       account.Email,
		Region:        "global",
		Authenticated: true,
	}); !errors.Is(err, store.ErrICloudMailboxRequired) {
		t.Fatalf("custom Apple session error = %v, want iCloud mailbox required", err)
	}
	_, err := db.ImportAliases(ctx, account.ID, []domain.AliasImportCandidate{{
		Address: "apple-import@icloud.com", APIKeyHash: []byte("apple-import-hash"),
		APIKeyPrefix: "apple-import", Active: true,
	}})
	if !errors.Is(err, store.ErrICloudMailboxRequired) {
		t.Fatalf("custom Apple directory import error = %v, want iCloud mailbox required", err)
	}
}

func TestCustomMailboxSuffixIsUniqueCaseInsensitively(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	createCustomImportAccount(t, ctx, db, "unique-suffix.test", true)

	_, err := db.CreateAccount(ctx, domain.Account{
		Name:               "Duplicate custom suffix",
		Email:              "different-internal-identity@example.invalid",
		MailboxType:        domain.MailboxTypeCustom,
		EmailSuffix:        "UNIQUE-SUFFIX.TEST",
		IMAPHost:           "imap.example.invalid",
		IMAPPort:           993,
		IMAPUsername:       "different-login",
		PasswordCiphertext: "encrypted",
		Enabled:            true,
	})
	if err == nil {
		t.Fatal("duplicate custom mailbox suffix was accepted")
	}
	accounts, listErr := db.ListAccounts(ctx)
	if listErr != nil || len(accounts) != 1 {
		t.Fatalf("accounts after duplicate suffix = %d, err=%v; want 1", len(accounts), listErr)
	}
}

package store_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

func TestAliasCredentialMigrationResumesPerAliasAndIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	account := createAccount(t, ctx, db, "Credential migration", "credential-migration@icloud.com")
	first := createAlias(t, ctx, db, account.ID, "first-credential@icloud.com", bytes.Repeat([]byte{0x01}, 32))
	second := createAlias(t, ctx, db, account.ID, "second-credential@icloud.com", bytes.Repeat([]byte{0x02}, 32))
	if _, err := db.DB().ExecContext(ctx, `UPDATE aliases SET credential_mode = 'v2' WHERE id IN (?, ?)`, first.ID, second.ID); err != nil {
		t.Fatalf("mark v2 migration fixtures: %v", err)
	}

	calls := make(map[int64]int)
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		calls[aliasID]++
		if aliasID == second.ID {
			return domain.AliasCredentialMaterial{}, errors.New("injected migration interruption")
		}
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, issueErr
	})
	if err := db.EnsureAliasCredentials(ctx); err == nil || !strings.Contains(err.Error(), "injected migration interruption") {
		t.Fatalf("interrupted migration error = %v", err)
	}
	migratedFirst, err := db.GetAlias(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	pendingSecond, err := db.GetAlias(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if migratedFirst.CredentialVersion != 1 || migratedFirst.CredentialCiphertext == "" {
		t.Fatalf("first alias was not committed before interruption: %#v", migratedFirst)
	}
	if pendingSecond.CredentialVersion != 0 || pendingSecond.CredentialCiphertext != "" {
		t.Fatalf("second alias unexpectedly migrated: %#v", pendingSecond)
	}
	firstCiphertext := migratedFirst.CredentialCiphertext

	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		calls[aliasID]++
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, issueErr
	})
	if err := db.EnsureAliasCredentials(ctx); err != nil {
		t.Fatalf("resume credential migration: %v", err)
	}
	if err := db.EnsureAliasCredentials(ctx); err != nil {
		t.Fatalf("repeat credential migration: %v", err)
	}
	firstAfter, _ := db.GetAlias(ctx, first.ID)
	secondAfter, _ := db.GetAlias(ctx, second.ID)
	if firstAfter.CredentialCiphertext != firstCiphertext || firstAfter.CredentialVersion != 1 {
		t.Fatal("resumed migration rotated an already committed alias")
	}
	if secondAfter.CredentialVersion != 1 || secondAfter.CredentialCiphertext == "" {
		t.Fatalf("resumed migration did not finish second alias: %#v", secondAfter)
	}
	if calls[first.ID] != 1 || calls[second.ID] != 2 {
		t.Fatalf("credential issuer calls = first:%d second:%d", calls[first.ID], calls[second.ID])
	}
}

func TestEnsureAliasCredentialsLocksAccountBeforePublishingBundle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Credential lock order", "credential-lock-order@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "credential-lock-order-alias@icloud.com", bytes.Repeat([]byte{0x01}, 32))
	if _, err := db.DB().ExecContext(ctx, `
		UPDATE aliases SET credential_mode = 'v2', credential_version = 0,
			credential_ciphertext = '', imap_password_hash = X'',
			oauth_client_id = '', refresh_token_hash = X''
		WHERE id = ?`, alias.ID); err != nil {
		t.Fatalf("reset incomplete v2 alias: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE credential_lock_order(position INTEGER PRIMARY KEY AUTOINCREMENT, event TEXT NOT NULL)`,
		`CREATE TRIGGER record_credential_account_lock BEFORE UPDATE ON accounts
			BEGIN INSERT INTO credential_lock_order(event) VALUES('account-lock'); END`,
		`CREATE TRIGGER require_credential_account_lock BEFORE UPDATE OF credential_ciphertext ON aliases
			BEGIN
				SELECT CASE WHEN NOT EXISTS(
					SELECT 1 FROM credential_lock_order WHERE event = 'account-lock'
				) THEN RAISE(ABORT, 'alias credential published before account lock') END;
				INSERT INTO credential_lock_order(event) VALUES('alias-update');
			END`,
	} {
		if _, err := db.DB().ExecContext(ctx, statement); err != nil {
			t.Fatalf("install credential lock-order fixture: %v", err)
		}
	}
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, issueErr
	})
	if err := db.EnsureAliasCredentials(ctx); err != nil {
		t.Fatalf("ensure credentials without account lock: %v", err)
	}
	var mode string
	if err := db.DB().QueryRowContext(ctx, `SELECT credential_mode FROM aliases WHERE id = ?`, alias.ID).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != domain.AliasCredentialModeV2 {
		t.Fatalf("credential mode after initialization = %q", mode)
	}
}

func TestEnsureAliasCredentialsRejectsFactoryVersionMismatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Credential version validation", "credential-version-validation@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "credential-version-validation-alias@icloud.com", bytes.Repeat([]byte{0x02}, 32))
	if _, err := db.DB().ExecContext(ctx, `
		UPDATE aliases SET credential_mode = 'v2', credential_version = 0,
			credential_ciphertext = '' WHERE id = ?`, alias.ID); err != nil {
		t.Fatalf("reset incomplete v2 alias: %v", err)
	}
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x5b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version+1)
		return material, issueErr
	})
	before, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureAliasCredentials(ctx); err == nil || !strings.Contains(err.Error(), "does not match requested version") {
		t.Fatalf("factory version mismatch error = %v", err)
	}
	after, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("alias changed after rejected factory version: before=%#v after=%#v", before, after)
	}
}

func TestEnsureAliasCredentialsDiscardsStaleLegacyPendingKeyAndContinues(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x5c}, 32))
	if err != nil {
		t.Fatal(err)
	}
	account := createAccount(t, ctx, db, "Stale pending key", "stale-pending-key@icloud.com")
	currentKey, currentHash, currentPrefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	pendingKey, pendingHash, _, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := db.CreateAlias(ctx, domain.Alias{
		AccountID: account.ID, Address: "stale-pending-legacy@icloud.com",
		APIKeyHash: currentHash, APIKeyPrefix: currentPrefix,
		CredentialMode: domain.AliasCredentialModeLegacy, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create legacy alias with current key: %v", err)
	}
	pendingCiphertext, err := cipher.EncryptPendingAliasAPIKey(pendingKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
		VALUES(?, ?, 1)`, legacy.ID, pendingCiphertext); err != nil {
		t.Fatalf("seed stale pending key: %v", err)
	}

	// Give the initializer another alias after the stale row so the test also
	// proves cleanup does not stop the global migration loop.
	v2 := createAlias(
		t, ctx, db, account.ID, "stale-pending-v2@icloud.com", bytes.Repeat([]byte{0x5d}, 32),
	)
	if _, err := db.DB().ExecContext(ctx, `
		UPDATE aliases SET credential_mode = 'v2', credential_version = 0,
			credential_ciphertext = '', imap_password_hash = X'',
			oauth_client_id = '', refresh_token_hash = X''
		WHERE id = ?`, v2.ID); err != nil {
		t.Fatalf("prepare incomplete v2 alias: %v", err)
	}
	legacyBefore, err := db.GetAlias(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}

	generatedCalls := 0
	reuseCalls := 0
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		generatedCalls++
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, issueErr
	})
	db.ConfigureAliasCredentialReuseFactory(func(aliasID, version int64, ciphertext string) (domain.AliasCredentialMaterial, error) {
		reuseCalls++
		rawKey, decryptErr := cipher.DecryptPendingAliasAPIKey(ciphertext)
		if decryptErr != nil {
			return domain.AliasCredentialMaterial{}, decryptErr
		}
		_, material, issueErr := secure.NewAliasCredentialMaterialWithAPIKey(cipher, aliasID, version, rawKey)
		return material, issueErr
	})
	if err := db.EnsureAliasCredentials(ctx); err != nil {
		t.Fatalf("initialize credentials with stale legacy pending key: %v", err)
	}

	legacyAfter, err := db.GetAlias(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacyAfter, legacyBefore) {
		t.Fatalf("stale pending cleanup changed live legacy credentials: before=%#v after=%#v", legacyBefore, legacyAfter)
	}
	if legacyAfter.CredentialMode != domain.AliasCredentialModeLegacy ||
		!secure.HashEqual(legacyAfter.APIKeyHash, currentHash) || legacyAfter.APIKeyPrefix != currentKey[:12] {
		t.Fatalf("live legacy API key was not preserved: %#v", legacyAfter)
	}
	var pendingCount int
	if err := db.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_alias_api_keys WHERE alias_id = ?`, legacy.ID,
	).Scan(&pendingCount); err != nil {
		t.Fatal(err)
	}
	if pendingCount != 0 {
		t.Fatalf("stale pending rows after initialization = %d, want 0", pendingCount)
	}
	if _, err := db.GetAliasByAPIKeyHash(ctx, pendingHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale pending key lookup = %v, want ErrNotFound", err)
	}
	if current, err := db.GetAliasByAPIKeyHash(ctx, currentHash); err != nil || current.ID != legacy.ID {
		t.Fatalf("current legacy key lookup = alias %#v, err=%v", current, err)
	}

	migratedV2, err := db.GetAlias(ctx, v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if migratedV2.CredentialMode != domain.AliasCredentialModeV2 ||
		migratedV2.CredentialVersion != 1 || migratedV2.CredentialCiphertext == "" {
		t.Fatalf("later v2 alias was not initialized: %#v", migratedV2)
	}
	if reuseCalls != 1 || generatedCalls != 1 {
		t.Fatalf("issuer calls after initialization = reuse:%d generated:%d, want 1 each", reuseCalls, generatedCalls)
	}
	if err := db.EnsureAliasCredentials(ctx); err != nil {
		t.Fatalf("repeat credential initialization: %v", err)
	}
	if reuseCalls != 1 || generatedCalls != 1 {
		t.Fatalf("repeat initialization reissued credentials = reuse:%d generated:%d", reuseCalls, generatedCalls)
	}
}

func TestEnsureAliasCredentialsRetainsMismatchedPendingKeyForNonCanonicalState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		address    string
		mode       string
		version    int64
		ciphertext string
	}{
		{
			name: "incomplete v2", address: "pending-mismatch-v2@icloud.com",
			mode: domain.AliasCredentialModeV2,
		},
		{
			name: "legacy version without bundle", address: "pending-mismatch-legacy-version@icloud.com",
			mode: domain.AliasCredentialModeLegacy, version: 1,
		},
		{
			name: "legacy bundle without version", address: "pending-mismatch-legacy-bundle@icloud.com",
			mode: domain.AliasCredentialModeLegacy, ciphertext: "ac1.partial",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db := openTestStore(t)
			cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x5e}, 32))
			if err != nil {
				t.Fatal(err)
			}
			account := createAccount(t, ctx, db, test.name, test.address)
			_, currentHash, currentPrefix, err := secure.NewAPIKey()
			if err != nil {
				t.Fatal(err)
			}
			pendingKey, _, _, err := secure.NewAPIKey()
			if err != nil {
				t.Fatal(err)
			}
			alias, err := db.CreateAlias(ctx, domain.Alias{
				AccountID: account.ID, Address: "alias-" + test.address,
				APIKeyHash: currentHash, APIKeyPrefix: currentPrefix,
				CredentialMode: domain.AliasCredentialModeLegacy, Enabled: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB().ExecContext(ctx, `
				UPDATE aliases SET credential_mode = ?, credential_version = ?, credential_ciphertext = ?
				WHERE id = ?`, test.mode, test.version, test.ciphertext, alias.ID); err != nil {
				t.Fatal(err)
			}
			pendingCiphertext, err := cipher.EncryptPendingAliasAPIKey(pendingKey)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB().ExecContext(ctx, `
				INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
				VALUES(?, ?, 1)`, alias.ID, pendingCiphertext); err != nil {
				t.Fatal(err)
			}
			before, err := db.GetAlias(ctx, alias.ID)
			if err != nil {
				t.Fatal(err)
			}

			db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
				_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
				return material, issueErr
			})
			db.ConfigureAliasCredentialReuseFactory(func(aliasID, version int64, ciphertext string) (domain.AliasCredentialMaterial, error) {
				rawKey, decryptErr := cipher.DecryptPendingAliasAPIKey(ciphertext)
				if decryptErr != nil {
					return domain.AliasCredentialMaterial{}, decryptErr
				}
				_, material, issueErr := secure.NewAliasCredentialMaterialWithAPIKey(cipher, aliasID, version, rawKey)
				return material, issueErr
			})
			if err := db.EnsureAliasCredentials(ctx); err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("non-canonical pending mismatch error = %v", err)
			}
			after, err := db.GetAlias(ctx, alias.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("non-canonical alias changed: before=%#v after=%#v", before, after)
			}
			var pendingCount int
			if err := db.DB().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM pending_alias_api_keys WHERE alias_id = ?`, alias.ID,
			).Scan(&pendingCount); err != nil {
				t.Fatal(err)
			}
			if pendingCount != 1 {
				t.Fatalf("pending rows after rejected mismatch = %d, want 1", pendingCount)
			}
		})
	}
}

func TestRotateAliasCredentialsReplacesEveryAuthenticatorAndPreservesMailboxIdentity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x52}, 32))
	if err != nil {
		t.Fatal(err)
	}
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, issueErr
	})
	account := createAccount(t, ctx, db, "Credential rotation", "credential-rotation@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "rotate-credential@icloud.com", nil)
	oldCredentials, err := cipher.DecryptAliasCredentials(alias.ID, alias.CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}

	rotated, err := db.RotateAliasCredentials(ctx, alias.ID)
	if err != nil {
		t.Fatalf("rotate alias credentials: %v", err)
	}
	newCredentials, err := cipher.DecryptAliasCredentials(rotated.ID, rotated.CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.CredentialVersion != alias.CredentialVersion+1 ||
		rotated.MailboxUIDValidity != alias.MailboxUIDValidity ||
		rotated.MailboxUIDNext != alias.MailboxUIDNext {
		t.Fatalf("rotated alias identity = %#v; original=%#v", rotated, alias)
	}
	oldValues := []string{oldCredentials.APIKey, oldCredentials.IMAPPassword, oldCredentials.ClientID, oldCredentials.RefreshToken}
	newValues := []string{newCredentials.APIKey, newCredentials.IMAPPassword, newCredentials.ClientID, newCredentials.RefreshToken}
	for index := range oldValues {
		if oldValues[index] == newValues[index] {
			t.Fatalf("credential field %d was not rotated", index)
		}
	}
	for name, lookup := range map[string]func() error{
		"API Key": func() error {
			_, lookupErr := db.GetAliasByAPIKeyHash(ctx, secure.HashToken(oldCredentials.APIKey))
			return lookupErr
		},
		"IMAP password": func() error {
			_, lookupErr := db.GetAliasByIMAPPasswordHash(ctx, secure.HashToken(oldCredentials.IMAPPassword))
			return lookupErr
		},
		"OAuth client": func() error {
			_, lookupErr := db.GetAliasByOAuthClientID(ctx, oldCredentials.ClientID)
			return lookupErr
		},
	} {
		if err := lookup(); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("old %s lookup error = %v", name, err)
		}
	}
	if current, err := db.GetAliasByAPIKeyHash(ctx, secure.HashToken(newCredentials.APIKey)); err != nil || current.ID != alias.ID {
		t.Fatalf("new API Key lookup = alias %#v error %v", current, err)
	}
}

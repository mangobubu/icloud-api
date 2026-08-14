package store_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

func TestCreateV2AliasRequiresCredentialFactory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "V2 factory required", "v2-factory-required@icloud.com")

	_, err := db.CreateAlias(ctx, domain.Alias{
		AccountID:      account.ID,
		Address:        "v2-factory-required-alias@icloud.com",
		CredentialMode: domain.AliasCredentialModeV2,
		Enabled:        true,
	})
	if err == nil || !strings.Contains(err.Error(), "v2 credential factory is not configured") {
		t.Fatalf("v2 create without factory error = %v", err)
	}
	aliases, err := db.ListAliasesByAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 0 {
		t.Fatalf("aliases after rejected v2 create = %d, want 0", len(aliases))
	}
}

func TestCreateV2AliasPersistsGeneratedAPIKeyPrefix(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x6d}, 32))
	if err != nil {
		t.Fatal(err)
	}
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, issueErr
	})
	account := createAccount(t, ctx, db, "V2 prefix", "v2-prefix@icloud.com")
	alias, err := db.CreateAlias(ctx, domain.Alias{
		AccountID:      account.ID,
		Address:        "v2-prefix-alias@icloud.com",
		CredentialMode: domain.AliasCredentialModeV2,
		Enabled:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := cipher.DecryptAliasCredentials(alias.ID, alias.CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	if alias.APIKeyPrefix != credentials.APIKey[:12] ||
		!secure.HashEqual(alias.APIKeyHash, secure.HashToken(credentials.APIKey)) {
		t.Fatalf("generated credential metadata mismatch: prefix=%q key_prefix=%q hash_equal=%t", alias.APIKeyPrefix, credentials.APIKey[:12], secure.HashEqual(alias.APIKeyHash, secure.HashToken(credentials.APIKey)))
	}
}

func TestCreateAliasRejectsUnsupportedCredentialMode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Invalid credential mode", "invalid-mode@icloud.com")

	_, err := db.CreateAlias(ctx, domain.Alias{
		AccountID:      account.ID,
		Address:        "invalid-mode-alias@icloud.com",
		CredentialMode: "future",
		Enabled:        true,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported credential mode") {
		t.Fatalf("create alias error = %v, want unsupported credential mode", err)
	}

	aliases, listErr := db.ListAliasesByAccount(ctx, account.ID)
	if listErr != nil {
		t.Fatalf("list aliases after rejected create: %v", listErr)
	}
	if len(aliases) != 0 {
		t.Fatalf("aliases after rejected create = %d, want 0", len(aliases))
	}
}

func TestCreateLegacyAliasRequiresEstablishedAPIKeyHash(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Legacy credential validation", "legacy-validation@icloud.com")

	for _, hash := range [][]byte{nil, bytes.Repeat([]byte{0x61}, 31), bytes.Repeat([]byte{0x62}, 33)} {
		_, err := db.CreateAlias(ctx, domain.Alias{
			AccountID:      account.ID,
			Address:        "legacy-validation-alias@icloud.com",
			APIKeyHash:     hash,
			CredentialMode: domain.AliasCredentialModeLegacy,
			Enabled:        true,
		})
		if err == nil || !strings.Contains(err.Error(), "requires a 32-byte API key hash") {
			t.Fatalf("create legacy alias with %d-byte hash error = %v", len(hash), err)
		}
	}
}

func TestCreateLegacyAliasPreservesEstablishedCredential(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Legacy credential", "legacy-credential@icloud.com")
	wantHash := bytes.Repeat([]byte{0x63}, 32)

	alias, err := db.CreateAlias(ctx, domain.Alias{
		AccountID:      account.ID,
		Address:        "legacy-credential-alias@icloud.com",
		APIKeyHash:     wantHash,
		APIKeyPrefix:   "legacy-key-p",
		CredentialMode: domain.AliasCredentialModeLegacy,
		Enabled:        true,
	})
	if err != nil {
		t.Fatalf("create legacy alias: %v", err)
	}
	if alias.CredentialMode != domain.AliasCredentialModeLegacy ||
		!bytes.Equal(alias.APIKeyHash, wantHash) || alias.APIKeyPrefix != "legacy-key-p" ||
		alias.CredentialVersion != 0 || alias.CredentialCiphertext != "" {
		t.Fatalf("created legacy alias = %#v", alias)
	}
}

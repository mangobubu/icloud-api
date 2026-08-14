package store_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

func TestRotateLegacyAliasAPIKeyPreservesCompatibilityState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Legacy API key rotation", "legacy-key-rotation@icloud.com")
	oldKey, oldHash, oldPrefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	alias, err := db.CreateAlias(ctx, domain.Alias{
		AccountID: account.ID, Address: "legacy-key-rotation-alias@icloud.com",
		APIKeyHash: oldHash, APIKeyPrefix: oldPrefix,
		CredentialMode: domain.AliasCredentialModeLegacy, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create legacy alias: %v", err)
	}

	for _, statement := range []string{
		`INSERT INTO latest_messages(alias_id, uid_validity, uid, internal_date, synced_at)
			VALUES(?, 71, 9, 100, 101)`,
		`INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at)
			VALUES(?, 71, 9, 102)`,
		`INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at)
			VALUES(?, 71, 9, 103)`,
		`INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
			VALUES(?, 'ak1.stale', 104)`,
	} {
		argument := alias.ID
		if strings.Contains(statement, "imap_seen_tasks") {
			argument = account.ID
		}
		if _, err := db.DB().ExecContext(ctx, statement, argument); err != nil {
			t.Fatalf("seed legacy compatibility state: %v", err)
		}
	}
	before, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeCounts := aliasCompatibilityStateCounts(t, ctx, db, alias.ID, account.ID)

	newKey, newHash, newPrefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := db.RotateAliasAPIKeyWithRawKey(ctx, alias.ID, newHash, newPrefix, newKey)
	if err != nil {
		t.Fatalf("rotate legacy API key: %v", err)
	}
	if oldKey == newKey || !secure.HashEqual(rotated.APIKeyHash, newHash) || rotated.APIKeyPrefix != newPrefix {
		t.Fatalf("legacy API key was not replaced: %#v", rotated)
	}
	if rotated.CredentialMode != domain.AliasCredentialModeLegacy ||
		rotated.CredentialVersion != before.CredentialVersion ||
		rotated.CredentialCiphertext != before.CredentialCiphertext ||
		!bytes.Equal(rotated.IMAPPasswordHash, before.IMAPPasswordHash) ||
		rotated.OAuthClientID != before.OAuthClientID ||
		!bytes.Equal(rotated.RefreshTokenHash, before.RefreshTokenHash) ||
		rotated.Enabled != before.Enabled || rotated.LastSyncStatus != before.LastSyncStatus ||
		rotated.LastSyncError != before.LastSyncError ||
		rotated.MailboxUIDValidity != before.MailboxUIDValidity || rotated.MailboxUIDNext != before.MailboxUIDNext {
		t.Fatalf("legacy API key rotation changed compatibility state: before=%#v after=%#v", before, rotated)
	}
	afterCounts := aliasCompatibilityStateCounts(t, ctx, db, alias.ID, account.ID)
	if beforeCounts.latest != afterCounts.latest || beforeCounts.consumed != afterCounts.consumed ||
		beforeCounts.seen != afterCounts.seen || afterCounts.pending != 0 {
		t.Fatalf("legacy state counts changed: before=%#v after=%#v", beforeCounts, afterCounts)
	}
}

func TestRotateV2AliasAPIKeyPreservesIMAPOAuthAndAccessTokens(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x74}, 32))
	if err != nil {
		t.Fatal(err)
	}
	configureAPIKeyRotationTestFactories(db, cipher, nil, nil)
	account := createAccount(t, ctx, db, "V2 API key rotation", "v2-key-rotation@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "v2-key-rotation-alias@icloud.com", nil)
	beforeCredentials, err := cipher.DecryptAliasCredentials(alias.ID, alias.CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	accessToken, err := cipher.IssueAliasAccessToken(alias.ID, alias.CredentialVersion, alias.RefreshTokenHash, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	oldDirectLink, err := cipher.DirectLinkToken(alias.ID, alias.APIKeyHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
		VALUES(?, 'ak1.stale', 1)`, alias.ID); err != nil {
		t.Fatalf("seed stale pending API key: %v", err)
	}

	newKey, newHash, newPrefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := db.RotateAliasAPIKeyWithRawKey(ctx, alias.ID, newHash, newPrefix, newKey)
	if err != nil {
		t.Fatalf("rotate v2 API key: %v", err)
	}
	afterCredentials, err := cipher.DecryptAliasCredentials(rotated.ID, rotated.CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	if afterCredentials.APIKey != newKey || afterCredentials.APIKey == beforeCredentials.APIKey ||
		afterCredentials.IMAPPassword != beforeCredentials.IMAPPassword ||
		afterCredentials.ClientID != beforeCredentials.ClientID ||
		afterCredentials.RefreshToken != beforeCredentials.RefreshToken {
		t.Fatalf("v2 API key rotation changed the wrong fields: before=%#v after=%#v", beforeCredentials, afterCredentials)
	}
	if rotated.CredentialMode != domain.AliasCredentialModeV2 ||
		rotated.CredentialVersion != alias.CredentialVersion ||
		rotated.MailboxUIDValidity != alias.MailboxUIDValidity || rotated.MailboxUIDNext != alias.MailboxUIDNext ||
		!bytes.Equal(rotated.IMAPPasswordHash, alias.IMAPPasswordHash) ||
		rotated.OAuthClientID != alias.OAuthClientID || !bytes.Equal(rotated.RefreshTokenHash, alias.RefreshTokenHash) {
		t.Fatalf("v2 API key rotation changed credential identity: before=%#v after=%#v", alias, rotated)
	}
	if !cipher.VerifyAliasAccessToken(accessToken, rotated.ID, rotated.CredentialVersion,
		rotated.RefreshTokenHash, time.Now().UTC()) {
		t.Fatal("existing OAuth access token stopped working after API-key-only rotation")
	}
	if cipher.VerifyDirectLinkToken(oldDirectLink, rotated.ID, rotated.APIKeyHash) {
		t.Fatal("old direct link remained valid after API key rotation")
	}
	if counts := aliasCompatibilityStateCounts(t, ctx, db, alias.ID, account.ID); counts.pending != 0 {
		t.Fatalf("stale pending API key rows after v2 rotation = %d", counts.pending)
	}
}

func TestRotateV2AliasAPIKeyRejectsMismatchedPrefix(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x79}, 32))
	if err != nil {
		t.Fatal(err)
	}
	configureAPIKeyRotationTestFactories(db, cipher, nil, nil)
	account := createAccount(t, ctx, db, "V2 prefix validation", "v2-prefix-validation@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "v2-prefix-validation-alias@icloud.com", nil)
	before, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	newKey, newHash, _, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	wrongPrefix := "icm_wrongpre"
	if len(wrongPrefix) != 12 {
		t.Fatalf("test prefix length = %d", len(wrongPrefix))
	}
	if _, err := db.RotateAliasAPIKeyWithRawKey(ctx, alias.ID, newHash, wrongPrefix, newKey); err == nil || !strings.Contains(err.Error(), "issuer changed non-API credentials") {
		t.Fatalf("mismatched prefix rotation error = %v", err)
	}
	after, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("alias changed after mismatched prefix rejection: before=%#v after=%#v", before, after)
	}
}

func TestRotateAliasCredentialsDeletesStalePendingAPIKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x7a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	configureAPIKeyRotationTestFactories(db, cipher, nil, nil)
	account := createAccount(t, ctx, db, "Bundle pending cleanup", "bundle-pending-cleanup@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "bundle-pending-cleanup-alias@icloud.com", nil)
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
		VALUES(?, 'ak1.stale', 1)`, alias.ID); err != nil {
		t.Fatalf("seed stale pending API key: %v", err)
	}

	rotated, err := db.RotateAliasCredentials(ctx, alias.ID)
	if err != nil {
		t.Fatalf("rotate alias credentials: %v", err)
	}
	if rotated.CredentialVersion != alias.CredentialVersion+1 {
		t.Fatalf("credential version = %d, want %d", rotated.CredentialVersion, alias.CredentialVersion+1)
	}
	pending, err := db.ListPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("list pending API keys after credential rotation: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("stale pending API keys remained claimable after credential rotation: %#v", pending)
	}
}

func TestRotateAliasCredentialsRejectsLegacyModeWithoutMutation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x75}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var credentialCalls int
	configureAPIKeyRotationTestFactories(db, cipher, &credentialCalls, nil)
	account := createAccount(t, ctx, db, "Legacy bundle rejection", "legacy-bundle-rejection@icloud.com")
	rawKey, hash, prefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	alias, err := db.CreateAlias(ctx, domain.Alias{
		AccountID: account.ID, Address: "legacy-bundle-rejection-alias@icloud.com",
		APIKeyHash: hash, APIKeyPrefix: prefix,
		CredentialMode: domain.AliasCredentialModeLegacy, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = rawKey
	before, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.RotateAliasCredentials(ctx, alias.ID)
	if !errors.Is(err, store.ErrAliasCredentialMode) {
		t.Fatalf("legacy full credential rotation error = %v, want ErrAliasCredentialMode", err)
	}
	after, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) || credentialCalls != 0 {
		t.Fatalf("legacy alias changed after rejected bundle rotation: before=%#v after=%#v calls=%d",
			before, after, credentialCalls)
	}
}

func TestAliasRotationRejectsConfirmationPendingWithoutMutation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x76}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var credentialCalls, apiKeyCalls int
	configureAPIKeyRotationTestFactories(db, cipher, &credentialCalls, &apiKeyCalls)
	account := createAccount(t, ctx, db, "Pending rotation", "pending-rotation@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "pending-rotation-alias@icloud.com", nil)
	credentialCalls = 0
	if _, err := db.DB().ExecContext(ctx, `
		UPDATE aliases SET enabled = FALSE, last_sync_error = ? WHERE id = ?`,
		domain.AppleAliasConfirmationPending, alias.ID); err != nil {
		t.Fatalf("mark alias confirmation pending: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
		VALUES(?, 'ak1.pending', 1)`, alias.ID); err != nil {
		t.Fatalf("seed pending key: %v", err)
	}
	before, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, hash, prefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.RotateAliasAPIKeyWithRawKey(ctx, alias.ID, hash, prefix, "icm_rejected"); !errors.Is(err, store.ErrAliasConfirmationPending) {
		t.Fatalf("pending API key rotation error = %v", err)
	}
	if _, err := db.RotateAliasCredentials(ctx, alias.ID); !errors.Is(err, store.ErrAliasConfirmationPending) {
		t.Fatalf("pending credential rotation error = %v", err)
	}
	after, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) || credentialCalls != 0 || apiKeyCalls != 0 {
		t.Fatalf("pending alias changed after rejected rotations: before=%#v after=%#v credential_calls=%d api_key_calls=%d",
			before, after, credentialCalls, apiKeyCalls)
	}
	if counts := aliasCompatibilityStateCounts(t, ctx, db, alias.ID, account.ID); counts.pending != 1 {
		t.Fatalf("pending key rows after rejected rotations = %d", counts.pending)
	}
}

func TestRotateAliasAPIKeyRollsBackWhenPendingKeyDeletionFails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Rotation rollback", "rotation-rollback@icloud.com")
	_, oldHash, oldPrefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	alias, err := db.CreateAlias(ctx, domain.Alias{
		AccountID: account.ID, Address: "rotation-rollback-alias@icloud.com",
		APIKeyHash: oldHash, APIKeyPrefix: oldPrefix,
		CredentialMode: domain.AliasCredentialModeLegacy, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
		VALUES(?, 'ak1.pending', 1)`, alias.ID); err != nil {
		t.Fatalf("seed pending API key: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_pending_key_delete
		BEFORE DELETE ON pending_alias_api_keys
		BEGIN SELECT RAISE(ABORT, 'injected pending delete failure'); END`); err != nil {
		t.Fatalf("install pending delete failure: %v", err)
	}
	before, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	newKey, newHash, newPrefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.RotateAliasAPIKeyWithRawKey(ctx, alias.ID, newHash, newPrefix, newKey)
	if err == nil || !strings.Contains(err.Error(), "delete stale pending alias key") {
		t.Fatalf("rotation deletion error = %v", err)
	}
	after, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("API key update survived rolled-back transaction: before=%#v after=%#v", before, after)
	}
	if counts := aliasCompatibilityStateCounts(t, ctx, db, alias.ID, account.ID); counts.pending != 1 {
		t.Fatalf("pending key rows after rollback = %d", counts.pending)
	}
}

func configureAPIKeyRotationTestFactories(
	db *store.Store,
	cipher *secure.Cipher,
	credentialCalls, apiKeyCalls *int,
) {
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		if credentialCalls != nil {
			*credentialCalls++
		}
		_, material, err := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, err
	})
	db.ConfigureAliasAPIKeyRotationFactory(func(
		aliasID, version int64,
		credentialCiphertext, apiKey string,
	) (domain.AliasCredentialMaterial, error) {
		if apiKeyCalls != nil {
			*apiKeyCalls++
		}
		credentials, err := cipher.DecryptAliasCredentials(aliasID, credentialCiphertext)
		if err != nil {
			return domain.AliasCredentialMaterial{}, err
		}
		credentials.APIKey = apiKey
		ciphertext, err := cipher.EncryptAliasCredentials(aliasID, credentials)
		if err != nil {
			return domain.AliasCredentialMaterial{}, err
		}
		return domain.AliasCredentialMaterial{
			Ciphertext:       ciphertext,
			APIKeyHash:       secure.HashToken(credentials.APIKey),
			APIKeyPrefix:     credentials.APIKey[:12],
			IMAPPasswordHash: secure.HashToken(credentials.IMAPPassword),
			OAuthClientID:    credentials.ClientID,
			RefreshTokenHash: secure.HashToken(credentials.RefreshToken),
			Version:          version,
		}, nil
	})
}

type aliasCompatibilityCounts struct {
	latest   int
	consumed int
	seen     int
	pending  int
}

func aliasCompatibilityStateCounts(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	aliasID, accountID int64,
) aliasCompatibilityCounts {
	t.Helper()
	queries := []struct {
		target *int
		query  string
		arg    int64
	}{}
	counts := aliasCompatibilityCounts{}
	queries = append(queries,
		struct {
			target *int
			query  string
			arg    int64
		}{&counts.latest, `SELECT COUNT(*) FROM latest_messages WHERE alias_id = ?`, aliasID},
		struct {
			target *int
			query  string
			arg    int64
		}{&counts.consumed, `SELECT COUNT(*) FROM consumed_messages WHERE alias_id = ?`, aliasID},
		struct {
			target *int
			query  string
			arg    int64
		}{&counts.seen, `SELECT COUNT(*) FROM imap_seen_tasks WHERE account_id = ?`, accountID},
		struct {
			target *int
			query  string
			arg    int64
		}{&counts.pending, `SELECT COUNT(*) FROM pending_alias_api_keys WHERE alias_id = ?`, aliasID},
	)
	for _, query := range queries {
		if err := db.DB().QueryRowContext(ctx, query.query, query.arg).Scan(query.target); err != nil {
			t.Fatalf("count alias compatibility state: %v", err)
		}
	}
	return counts
}

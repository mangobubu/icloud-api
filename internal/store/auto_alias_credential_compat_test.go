package store_test

import (
	"bytes"
	"context"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

func TestAutoAliasPendingKeyMatchesV2BundleAndRequiresAcknowledgement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x74}, 32))
	if err != nil {
		t.Fatal(err)
	}
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, issueErr
	})
	db.ConfigureAliasCredentialReuseFactory(func(aliasID, version int64, pendingCiphertext string) (domain.AliasCredentialMaterial, error) {
		apiKey, decryptErr := cipher.DecryptPendingAliasAPIKey(pendingCiphertext)
		if decryptErr != nil {
			return domain.AliasCredentialMaterial{}, decryptErr
		}
		_, material, issueErr := secure.NewAliasCredentialMaterialWithAPIKey(cipher, aliasID, version, apiKey)
		return material, issueErr
	})

	account := createAccount(t, ctx, db, "Automatic compatibility", "auto-compat@icloud.com")
	rawKey, hash, prefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	pendingCiphertext, err := cipher.EncryptPendingAliasAPIKey(rawKey)
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := db.CreateAliasWithPendingAPIKey(ctx, domain.AppleWebSession{
		AccountID: account.ID, Ciphertext: "as1.auto-compat", AppleID: account.Email,
		Region: "global", Authenticated: true,
	}, domain.Alias{
		AccountID: account.ID, Address: "auto-compat-alias@icloud.com",
		APIKeyHash: hash, APIKeyPrefix: prefix, Enabled: false,
	}, pendingCiphertext)
	if err != nil {
		t.Fatalf("create automatic alias: %v", err)
	}
	credentials, err := cipher.DecryptAliasCredentials(created.ID, created.CredentialCiphertext)
	if err != nil {
		t.Fatalf("decrypt v2 bundle: %v", err)
	}
	if credentials.APIKey != rawKey || !secure.HashEqual(created.APIKeyHash, hash) ||
		created.APIKeyPrefix != prefix || created.CredentialMode != domain.AliasCredentialModeV2 {
		t.Fatalf("created alias credential mismatch: alias=%#v bundle_key_equal=%t", created, credentials.APIKey == rawKey)
	}

	confirmation, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID)
	if err != nil || confirmation.ID != created.ID {
		t.Fatalf("pending confirmation = %#v, err=%v", confirmation, err)
	}
	confirmed, _, err := db.ConfirmPendingAutoAlias(ctx, domain.AppleWebSession{
		AccountID: account.ID, Ciphertext: "as1.auto-compat-confirmed", AppleID: account.Email,
		Region: "global", Authenticated: true,
	}, created.ID)
	if err != nil {
		t.Fatalf("confirm automatic alias: %v", err)
	}
	if !confirmed.Enabled {
		t.Fatal("confirmed automatic alias is disabled")
	}
	pending, err := db.ListPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending keys after confirmation = %#v, err=%v", pending, err)
	}
	decryptedPending, err := cipher.DecryptPendingAliasAPIKey(pending[0].APIKeyCiphertext)
	if err != nil || decryptedPending != credentials.APIKey {
		t.Fatalf("pending key does not match v2 bundle: equal=%t err=%v", decryptedPending == credentials.APIKey, err)
	}
	if err := db.DeletePendingAliasAPIKeys(ctx, account.ID, []int64{created.ID}); err != nil {
		t.Fatalf("acknowledge pending key: %v", err)
	}
	if count, err := db.CountPendingAliasAPIKeysByAccount(ctx, account.ID); err != nil || count != 0 {
		t.Fatalf("pending key count after acknowledgement = %d, err=%v", count, err)
	}
}

func TestImportAliasesWithCredentialsReturnsBundleAPIKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x75}, 32))
	if err != nil {
		t.Fatal(err)
	}
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, issueErr
	})
	db.ConfigureAliasCredentialRevealFactory(func(aliasID int64, ciphertext string) (string, error) {
		credentials, decryptErr := cipher.DecryptAliasCredentials(aliasID, ciphertext)
		return credentials.APIKey, decryptErr
	})
	account := createAccount(t, ctx, db, "Import compatibility", "import-compat@icloud.com")

	result, issued, err := db.ImportAliasesWithCredentials(ctx, account.ID, []domain.AliasImportCandidate{{
		Address: "import-compat-alias@icloud.com", Active: true,
	}})
	if err != nil {
		t.Fatalf("import alias with credentials: %v", err)
	}
	if len(result.Created) != 1 || len(issued) != 1 || issued[0].Alias.ID != result.Created[0].ID || issued[0].APIKey == "" {
		t.Fatalf("import result=%#v issued=%#v", result, issued)
	}
	credentials, err := cipher.DecryptAliasCredentials(result.Created[0].ID, result.Created[0].CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	if issued[0].APIKey != credentials.APIKey || !secure.HashEqual(result.Created[0].APIKeyHash, secure.HashToken(issued[0].APIKey)) {
		t.Fatal("returned import API key does not match the persisted v2 bundle")
	}
}

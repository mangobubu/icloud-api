package httpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

// TestLegacyMailRoutesPreserveExistingKeyDirectLinkAndConsumption captures the
// upgrade contract for aliases imported from the v1 service. Existing aliases
// keep their API key and old mailbox tables; the v2 archive may be populated
// alongside them, but the public v1 responses and one-shot semantics remain.
func TestLegacyMailRoutesPreserveExistingKeyDirectLinkAndConsumption(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.sync = nil
	if err := env.store.ConfigureMailArchive(t.TempDir(), 1<<20); err != nil {
		t.Fatalf("configure mail archive: %v", err)
	}

	ctx := context.Background()
	env.store.ConfigureAliasCredentialFactory(nil)
	account := adminAPITestCreateAccount(t, env, "legacy-compat@icloud.com")
	rawKey := legacyCompatAPIKey(0x4a)
	syncedAt := time.Date(2026, 8, 12, 2, 0, 0, 0, time.UTC)
	alias, err := env.store.CreateAlias(ctx, domain.Alias{
		AccountID:      account.ID,
		Address:        "legacy-compat-alias@icloud.com",
		Label:          "legacy compatibility",
		APIKeyHash:     secure.HashToken(rawKey),
		APIKeyPrefix:   rawKey[:12],
		CredentialMode: domain.AliasCredentialModeLegacy,
		Enabled:        true,
		LastSyncStatus: domain.SyncStatusOK,
		LastSyncedAt:   &syncedAt,
	})
	if err != nil {
		t.Fatalf("create legacy alias: %v", err)
	}
	legacyHash := append([]byte(nil), alias.APIKeyHash...)
	legacyDirectLink, err := env.cipher.DirectLinkToken(alias.ID, legacyHash)
	if err != nil {
		t.Fatalf("derive legacy direct link: %v", err)
	}

	// Startup credential initialization must leave imported aliases alone. The
	// factory is restored afterwards so the same test environment still models
	// the normal process bootstrap for new aliases.
	env.store.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, issueErr := secure.NewAliasCredentialMaterial(env.cipher, aliasID, version)
		return material, issueErr
	})
	if err := env.store.EnsureAliasCredentials(ctx); err != nil {
		t.Fatalf("initialize alias credentials: %v", err)
	}
	unchanged, err := env.store.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("reload legacy alias after credential initialization: %v", err)
	}
	if unchanged.CredentialMode != domain.AliasCredentialModeLegacy ||
		unchanged.CredentialVersion != 0 || strings.TrimSpace(unchanged.CredentialCiphertext) != "" ||
		!secure.HashEqual(unchanged.APIKeyHash, legacyHash) {
		t.Fatalf("legacy credentials changed during startup: mode=%q version=%d ciphertext=%q hash_equal=%t",
			unchanged.CredentialMode, unchanged.CredentialVersion, unchanged.CredentialCiphertext,
			secure.HashEqual(unchanged.APIKeyHash, legacyHash))
	}

	receivedAt := syncedAt.Add(-2 * time.Minute)
	sentAt := receivedAt.Add(-time.Minute)
	message := domain.ArchivedMessage{
		AccountID: account.ID, UIDValidity: 77, UID: 9,
		MessageID: "<legacy-compat@example.test>", InternalDate: receivedAt, HeaderDate: &sentAt,
		Subject: "Legacy compatibility subject", TextBody: "Legacy compatibility body",
		RawMIME: []byte("Message-ID: <legacy-compat@example.test>\r\n" +
			"Date: Wed, 12 Aug 2026 01:00:00 +0000\r\n" +
			"Subject: Legacy compatibility subject\r\n\r\n" +
			"Legacy compatibility body"),
		AliasIDs: []int64{alias.ID}, SyncedAt: syncedAt,
	}
	account, err = env.store.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("reload account before mailbox sync: %v", err)
	}
	if err := env.store.ApplyMailboxSync(ctx, account.ID, account.UpdatedAt, []domain.Alias{unchanged}, domain.MailboxSyncResult{
		ArchivedMessages: []domain.ArchivedMessage{message},
		State:            domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 77, LastUID: 9, UpdatedAt: syncedAt},
		Reset:            true,
	}, syncedAt); err != nil {
		t.Fatalf("publish legacy mailbox fixture: %v", err)
	}

	env.server.now = func() time.Time { return syncedAt }
	env.server.cfg.Timezone = time.FixedZone("UTC+8", 8*60*60)
	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build public router: %v", err)
	}

	latest := serveV2Request(router, http.MethodGet, "/api/v1/mail/latest", "", map[string]string{
		"Authorization": "Bearer " + rawKey,
	})
	if latest.Code != http.StatusOK {
		t.Fatalf("legacy Bearer latest status = %d; body=%s", latest.Code, latest.Body.String())
	}
	legacyCompatAssertLatest(t, latest.Body.Bytes(), alias.Address, "77-9", message.Subject, message.TextBody)

	firstRecent := serveV2Request(router, http.MethodGet,
		"/api/v1/mail/recent?api_key="+url.QueryEscape(legacyDirectLink), "", nil)
	if firstRecent.Code != http.StatusOK {
		t.Fatalf("legacy direct-link status = %d; body=%s", firstRecent.Code, firstRecent.Body.String())
	}
	legacyCompatAssertRecent(t, firstRecent.Body.Bytes(), alias.Address, message.Subject, message.TextBody,
		sentAt.In(env.server.cfg.Timezone).Format(time.RFC3339))

	secondRecent := serveV2Request(router, http.MethodGet,
		"/api/v1/mail/recent/?api_key="+url.QueryEscape(legacyDirectLink), "", nil)
	legacyCompatAssertAPIError(t, secondRecent, http.StatusNotFound, "MAIL_NOT_FOUND")

	// Consumption is scoped to the compact endpoint. The complete legacy
	// snapshot remains repeatable even after the direct link has been used.
	repeatedLatest := serveV2Request(router, http.MethodGet, "/api/v1/mail/latest", "", map[string]string{
		"Authorization": "Bearer " + rawKey,
	})
	if repeatedLatest.Code != http.StatusOK {
		t.Fatalf("legacy latest after direct consumption status = %d; body=%s", repeatedLatest.Code, repeatedLatest.Body.String())
	}
	legacyCompatAssertLatest(t, repeatedLatest.Body.Bytes(), alias.Address, "77-9", message.Subject, message.TextBody)
	legacyCompatAssertStateRows(t, env, alias.ID, account.ID, 77, 9)
}

// TestPendingLegacyAPIKeyIsReusedWhenAliasUpgradesToV2 distinguishes an
// unpublished one-time key from credentials that are already in use. A
// pending key may gain the rest of the v2 bundle during startup, but the key
// waiting to be shown to the administrator must not be silently replaced.
func TestPendingLegacyAPIKeyIsReusedWhenAliasUpgradesToV2(t *testing.T) {
	env := newAdminAPITestEnv(t)
	ctx := context.Background()
	env.store.ConfigureAliasCredentialFactory(nil)
	account := adminAPITestCreateAccount(t, env, "pending-key-compat@icloud.com")
	pendingKey := legacyCompatAPIKey(0x5b)
	alias, err := env.store.CreateAlias(ctx, domain.Alias{
		AccountID:      account.ID,
		Address:        "pending-key-compat-alias@icloud.com",
		Label:          "pending key compatibility",
		APIKeyHash:     secure.HashToken(pendingKey),
		APIKeyPrefix:   pendingKey[:12],
		CredentialMode: domain.AliasCredentialModeLegacy,
		Enabled:        true,
	})
	if err != nil {
		t.Fatalf("create pending-key alias: %v", err)
	}
	pendingCiphertext, err := env.cipher.EncryptPendingAliasAPIKey(pendingKey)
	if err != nil {
		t.Fatalf("encrypt pending API Key: %v", err)
	}
	if _, err := env.store.DB().ExecContext(ctx, `
		INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
		VALUES(?, ?, ?)`, alias.ID, pendingCiphertext, time.Now().UTC().UnixNano()); err != nil {
		t.Fatalf("store pending API Key: %v", err)
	}

	env.store.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, issueErr := secure.NewAliasCredentialMaterial(env.cipher, aliasID, version)
		return material, issueErr
	})
	env.store.ConfigureAliasCredentialReuseFactory(func(aliasID, version int64, ciphertext string) (domain.AliasCredentialMaterial, error) {
		rawAPIKey, decryptErr := env.cipher.DecryptPendingAliasAPIKey(ciphertext)
		if decryptErr != nil {
			return domain.AliasCredentialMaterial{}, decryptErr
		}
		_, material, issueErr := secure.NewAliasCredentialMaterialWithAPIKey(env.cipher, aliasID, version, rawAPIKey)
		return material, issueErr
	})
	if err := env.store.EnsureAliasCredentials(ctx); err != nil {
		t.Fatalf("upgrade alias with pending API Key: %v", err)
	}

	upgraded, err := env.store.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("reload pending-key alias: %v", err)
	}
	credentials, err := env.cipher.DecryptAliasCredentials(upgraded.ID, upgraded.CredentialCiphertext)
	if err != nil {
		t.Fatalf("decrypt upgraded credential bundle: %v", err)
	}
	if upgraded.CredentialMode != domain.AliasCredentialModeV2 || upgraded.CredentialVersion != 1 ||
		credentials.APIKey != pendingKey || !secure.HashEqual(upgraded.APIKeyHash, secure.HashToken(pendingKey)) ||
		upgraded.APIKeyPrefix != pendingKey[:12] {
		t.Fatalf("pending API Key upgrade = mode:%q version:%d key_equal:%t hash_equal:%t prefix:%q",
			upgraded.CredentialMode, upgraded.CredentialVersion, credentials.APIKey == pendingKey,
			secure.HashEqual(upgraded.APIKeyHash, secure.HashToken(pendingKey)), upgraded.APIKeyPrefix)
	}
	var pendingCount int
	if err := env.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_alias_api_keys WHERE alias_id = ?`, alias.ID,
	).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending API Key after upgrade: %v", err)
	}
	if pendingCount != 1 {
		t.Fatalf("pending API Key rows after upgrade = %d, want 1 until administrator acknowledgement", pendingCount)
	}
	if err := env.store.DeletePendingAliasAPIKeys(ctx, account.ID, []int64{alias.ID}); err != nil {
		t.Fatalf("acknowledge upgraded pending API Key: %v", err)
	}
	if err := env.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_alias_api_keys WHERE alias_id = ?`, alias.ID,
	).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending API Key after acknowledgement: %v", err)
	}
	if pendingCount != 0 {
		t.Fatalf("pending API Key rows after acknowledgement = %d, want 0", pendingCount)
	}
}

func TestPendingLegacyAPIKeyMismatchIsDiscardedWithoutReplacingExistingKey(t *testing.T) {
	env := newAdminAPITestEnv(t)
	ctx := context.Background()
	env.store.ConfigureAliasCredentialFactory(nil)
	account := adminAPITestCreateAccount(t, env, "pending-key-mismatch@icloud.com")
	existingKey := legacyCompatAPIKey(0x6b)
	pendingKey := legacyCompatAPIKey(0x6c)
	alias, err := env.store.CreateAlias(ctx, domain.Alias{
		AccountID:      account.ID,
		Address:        "pending-key-mismatch-alias@icloud.com",
		APIKeyHash:     secure.HashToken(existingKey),
		APIKeyPrefix:   existingKey[:12],
		CredentialMode: domain.AliasCredentialModeLegacy,
		Enabled:        true,
	})
	if err != nil {
		t.Fatalf("create pending-key mismatch alias: %v", err)
	}
	pendingCiphertext, err := env.cipher.EncryptPendingAliasAPIKey(pendingKey)
	if err != nil {
		t.Fatalf("encrypt mismatched pending API Key: %v", err)
	}
	if _, err := env.store.DB().ExecContext(ctx, `
		INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
		VALUES(?, ?, ?)`, alias.ID, pendingCiphertext, time.Now().UTC().UnixNano()); err != nil {
		t.Fatalf("store mismatched pending API Key: %v", err)
	}

	env.store.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, issueErr := secure.NewAliasCredentialMaterial(env.cipher, aliasID, version)
		return material, issueErr
	})
	env.store.ConfigureAliasCredentialReuseFactory(func(aliasID, version int64, ciphertext string) (domain.AliasCredentialMaterial, error) {
		rawAPIKey, decryptErr := env.cipher.DecryptPendingAliasAPIKey(ciphertext)
		if decryptErr != nil {
			return domain.AliasCredentialMaterial{}, decryptErr
		}
		_, material, issueErr := secure.NewAliasCredentialMaterialWithAPIKey(env.cipher, aliasID, version, rawAPIKey)
		return material, issueErr
	})
	if err := env.store.EnsureAliasCredentials(ctx); err != nil {
		t.Fatalf("initialize credentials with mismatched pending API Key: %v", err)
	}
	if err := env.store.EnsureAliasCredentials(ctx); err != nil {
		t.Fatalf("repeat initialization after stale pending cleanup: %v", err)
	}

	unchanged, err := env.store.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("reload mismatched pending-key alias: %v", err)
	}
	if unchanged.CredentialMode != domain.AliasCredentialModeLegacy ||
		unchanged.CredentialVersion != 0 || unchanged.CredentialCiphertext != "" ||
		!secure.HashEqual(unchanged.APIKeyHash, secure.HashToken(existingKey)) {
		t.Fatalf("alias changed after rejected pending-key upgrade = %#v", unchanged)
	}
	var pendingCount int
	if err := env.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_alias_api_keys WHERE alias_id = ?`, alias.ID,
	).Scan(&pendingCount); err != nil {
		t.Fatalf("count mismatched pending API Key: %v", err)
	}
	if pendingCount != 0 {
		t.Fatalf("mismatched pending API Key rows = %d, want 0 after safe cleanup", pendingCount)
	}
	if current, err := env.store.GetAliasByAPIKeyHash(ctx, secure.HashToken(existingKey)); err != nil || current.ID != alias.ID {
		t.Fatalf("existing API Key lookup = alias %#v, err=%v", current, err)
	}
	if _, err := env.store.GetAliasByAPIKeyHash(ctx, secure.HashToken(pendingKey)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale pending API Key lookup = %v, want ErrNotFound", err)
	}
}

func legacyCompatAPIKey(fill byte) string {
	return "icm_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func legacyCompatAssertLatest(t *testing.T, body []byte, address, id, subject, text string) {
	t.Helper()
	var payload struct {
		Data struct {
			Alias   string `json:"alias"`
			Message struct {
				ID      string `json:"id"`
				Subject string `json:"subject"`
				Text    string `json:"text"`
			} `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode legacy latest response: %v; body=%s", err, body)
	}
	if payload.Data.Alias != address || payload.Data.Message.ID != id ||
		payload.Data.Message.Subject != subject || payload.Data.Message.Text != text {
		t.Fatalf("legacy latest payload = %#v, want alias=%q id=%q subject=%q text=%q",
			payload, address, id, subject, text)
	}
}

func legacyCompatAssertRecent(t *testing.T, body []byte, address, subject, snippet, sentAt string) {
	t.Helper()
	var envelope struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode legacy recent response: %v; body=%s", err, body)
	}
	if len(envelope.Data) != 4 || envelope.Data["address"] != address ||
		envelope.Data["subject"] != subject || envelope.Data["snippet"] != snippet ||
		envelope.Data["sent_at"] != sentAt {
		t.Fatalf("legacy recent payload = %#v, want address=%q subject=%q snippet=%q sent_at=%q",
			envelope.Data, address, subject, snippet, sentAt)
	}
}

func legacyCompatAssertAPIError(t *testing.T, response *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("legacy compatibility error status = %d, want %d; body=%s", response.Code, wantStatus, response.Body.String())
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode legacy compatibility error: %v; body=%s", err, response.Body.String())
	}
	if payload.Error.Code != wantCode {
		t.Fatalf("legacy compatibility error code = %q, want %q; body=%s", payload.Error.Code, wantCode, response.Body.String())
	}
}

func legacyCompatAssertStateRows(t *testing.T, env *adminAPITestEnv, aliasID, accountID int64, uidValidity, uid uint32) {
	t.Helper()
	var consumed, seen int
	if err := env.store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM consumed_messages WHERE alias_id = ? AND uid_validity = ? AND uid = ?`,
		aliasID, int64(uidValidity), int64(uid)).Scan(&consumed); err != nil {
		t.Fatalf("count legacy consumed row: %v", err)
	}
	if err := env.store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM imap_seen_tasks WHERE account_id = ? AND uid_validity = ? AND uid = ?`,
		accountID, int64(uidValidity), int64(uid)).Scan(&seen); err != nil {
		t.Fatalf("count legacy seen row: %v", err)
	}
	if consumed != 1 || seen != 1 {
		t.Fatalf("legacy state rows = consumed:%d seen:%d, want 1 each", consumed, seen)
	}
}

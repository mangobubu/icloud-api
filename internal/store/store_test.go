package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestMailboxBindingIsIsolatedByAPIKeyHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)

	accountOne := createAccount(t, ctx, db, "Primary one", "primary-one@icloud.com")
	accountTwo := createAccount(t, ctx, db, "Primary two", "primary-two@icloud.com")
	aliasOne := createAlias(t, ctx, db, accountOne.ID, "relay-one@icloud.com", []byte("hash-one"))
	aliasTwo := createAlias(t, ctx, db, accountTwo.ID, "relay-two@icloud.com", []byte("hash-two"))

	receivedOne := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	receivedTwo := receivedOne.Add(time.Hour)
	mustUpsert(t, ctx, db, domain.LatestMessage{
		AliasID: aliasOne.ID, UIDValidity: 100, UID: 11, Subject: "only one",
		InternalDate: receivedOne, SyncedAt: receivedOne,
	})
	mustUpsert(t, ctx, db, domain.LatestMessage{
		AliasID: aliasTwo.ID, UIDValidity: 200, UID: 22, Subject: "only two",
		InternalDate: receivedTwo, SyncedAt: receivedTwo,
	})

	binding, err := db.GetMailboxBindingByAPIKeyHash(ctx, []byte("hash-one"))
	if err != nil {
		t.Fatalf("lookup first binding: %v", err)
	}
	if binding.Alias.ID != aliasOne.ID || binding.Account.ID != accountOne.ID {
		t.Fatalf("binding crossed ownership boundary: %#v", binding)
	}
	if binding.Message == nil || binding.Message.AliasID != aliasOne.ID || binding.Message.Subject != "only one" {
		t.Fatalf("binding exposed another alias message: %#v", binding.Message)
	}
	if bytes.Equal(binding.Alias.APIKeyHash, aliasTwo.APIKeyHash) {
		t.Fatal("binding returned the other alias API key hash")
	}

	if _, err := db.GetMailboxBindingByAPIKeyHash(ctx, []byte("unknown-hash")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown API key error = %v, want ErrNotFound", err)
	}

	rotatedHash := []byte("hash-one-rotated")
	if _, err := db.RotateAliasAPIKey(ctx, aliasOne.ID, rotatedHash, "rotated"); err != nil {
		t.Fatalf("rotate first alias key: %v", err)
	}
	if _, err := db.GetMailboxBindingByAPIKeyHash(ctx, []byte("hash-one")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old API key error after rotation = %v, want ErrNotFound", err)
	}
	rotatedBinding, err := db.GetMailboxBindingByAPIKeyHash(ctx, rotatedHash)
	if err != nil {
		t.Fatalf("lookup rotated binding: %v", err)
	}
	if rotatedBinding.Alias.ID != aliasOne.ID || rotatedBinding.Message.Subject != "only one" {
		t.Fatalf("rotated key resolved wrong binding: %#v", rotatedBinding)
	}

	accountOne.Enabled = false
	if _, err := db.UpdateAccount(ctx, accountOne); err != nil {
		t.Fatalf("disable account: %v", err)
	}
	aliasOne.Enabled = false
	if _, err := db.UpdateAlias(ctx, aliasOne); err != nil {
		t.Fatalf("disable alias: %v", err)
	}
	disabledBinding, err := db.GetMailboxBindingByAPIKeyHash(ctx, rotatedHash)
	if err != nil {
		t.Fatalf("lookup disabled binding: %v", err)
	}
	if disabledBinding.Account.Enabled || disabledBinding.Alias.Enabled {
		t.Fatalf("disabled state was hidden from caller: %#v", disabledBinding)
	}
}

func TestUpsertLatestMessageRejectsOlderMailboxPosition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "relay@icloud.com", []byte("hash"))
	baseTime := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

	changed, err := db.UpsertLatestMessage(ctx, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 100, UID: 20, Subject: "initial",
		InternalDate: baseTime, SyncedAt: baseTime,
	})
	if err != nil || !changed {
		t.Fatalf("insert initial latest message: changed=%v err=%v", changed, err)
	}

	stale := domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 100, UID: 19, Subject: "older uid",
		InternalDate: baseTime.Add(time.Hour), SyncedAt: baseTime.Add(time.Hour),
	}
	changed, err = db.UpsertLatestMessage(ctx, stale)
	if err != nil {
		t.Fatalf("upsert stale message %q: %v", stale.Subject, err)
	}
	if changed {
		t.Fatalf("stale message %q unexpectedly replaced latest", stale.Subject)
	}
	assertLatestSubject(t, ctx, db, alias.ID, "initial", 100, 20)

	changed, err = db.UpsertLatestMessage(ctx, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 100, UID: 21, Subject: "newer uid",
		InternalDate: baseTime.Add(3 * time.Hour), SyncedAt: baseTime.Add(3 * time.Hour),
	})
	if err != nil || !changed {
		t.Fatalf("upsert newer uid: changed=%v err=%v", changed, err)
	}
	assertLatestSubject(t, ctx, db, alias.ID, "newer uid", 100, 21)

	changed, err = db.UpsertLatestMessage(ctx, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 7, UID: 10, Subject: "new mailbox generation",
		InternalDate: baseTime.Add(4 * time.Hour), SyncedAt: baseTime.Add(4 * time.Hour),
	})
	if err != nil || !changed {
		t.Fatalf("upsert new UIDVALIDITY: changed=%v err=%v", changed, err)
	}
	assertLatestSubject(t, ctx, db, alias.ID, "new mailbox generation", 7, 10)

	if err := db.ReplaceLatestMessage(ctx, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 7, UID: 1, Subject: "authoritative lower uid",
		InternalDate: baseTime.Add(5 * time.Hour), SyncedAt: baseTime.Add(5 * time.Hour),
	}); err != nil {
		t.Fatalf("replace authoritative snapshot: %v", err)
	}
	assertLatestSubject(t, ctx, db, alias.ID, "authoritative lower uid", 7, 1)

	var count int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM latest_messages WHERE alias_id = ?`, alias.ID).Scan(&count); err != nil {
		t.Fatalf("count latest rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("latest message row count = %d, want 1", count)
	}
}

func TestLatestMessageRejectsZeroIMAPIdentifiers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "relay@icloud.com", []byte("position-hash"))

	for name, message := range map[string]domain.LatestMessage{
		"zero uid validity": {AliasID: alias.ID, UID: 1},
		"zero uid":          {AliasID: alias.ID, UIDValidity: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if changed, err := db.UpsertLatestMessage(ctx, message); err == nil || changed {
				t.Fatalf("invalid upsert result: changed=%v err=%v", changed, err)
			}
			if err := db.ReplaceLatestMessage(ctx, message); err == nil {
				t.Fatal("invalid replace unexpectedly succeeded")
			}
		})
	}
}

func TestListMetadataAndCascade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "Primary@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "Relay@icloud.com", []byte("list-hash"))
	receivedAt := time.Date(2026, 8, 6, 12, 30, 0, 0, time.UTC)
	mustUpsert(t, ctx, db, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 1, UID: 1,
		InternalDate: receivedAt, SyncedAt: receivedAt,
	})

	accounts, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].AliasCount != 1 {
		t.Fatalf("account alias metadata = %#v", accounts)
	}
	aliases, err := db.ListAliases(ctx, account.ID)
	if err != nil {
		t.Fatalf("list aliases: %v", err)
	}
	if len(aliases) != 1 || aliases[0].AccountEmail != "primary@icloud.com" || aliases[0].LatestReceivedAt == nil || !aliases[0].LatestReceivedAt.Equal(receivedAt) {
		t.Fatalf("alias joined metadata = %#v", aliases)
	}

	if err := db.DeleteAccount(ctx, account.ID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if _, err := db.GetAlias(ctx, alias.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cascaded alias lookup error = %v, want ErrNotFound", err)
	}
	if _, err := db.GetLatestMessage(ctx, alias.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cascaded message lookup error = %v, want ErrNotFound", err)
	}
}

func TestSetAliasEnabledClearsSnapshotWhenReenabled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "relay@icloud.com", []byte("reenable-hash"))
	syncedAt := time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC)
	mustUpsert(t, ctx, db, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 10, UID: 20, Subject: "stale snapshot",
		InternalDate: syncedAt, SyncedAt: syncedAt,
	})
	if err := db.UpdateAliasSyncStatus(ctx, alias.ID, domain.SyncStatusError, "old failure", &syncedAt); err != nil {
		t.Fatalf("set alias sync state: %v", err)
	}

	if err := db.SetAliasEnabled(ctx, alias.ID, false); err != nil {
		t.Fatalf("disable alias: %v", err)
	}
	if err := db.SetAliasEnabled(ctx, alias.ID, true); err != nil {
		t.Fatalf("re-enable alias: %v", err)
	}

	assertAliasSnapshotReset(t, ctx, db, alias.ID, true)
}

func TestUpdateAccountResetsAliasSnapshotsAfterSourceChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		update func(*testing.T, context.Context, *store.Store, domain.Account)
	}{
		{
			name: "re-enable account",
			update: func(t *testing.T, ctx context.Context, db *store.Store, account domain.Account) {
				account.Enabled = false
				if _, err := db.UpdateAccount(ctx, account); err != nil {
					t.Fatalf("disable account: %v", err)
				}
				account.Enabled = true
				if _, err := db.UpdateAccount(ctx, account); err != nil {
					t.Fatalf("re-enable account: %v", err)
				}
			},
		},
		{
			name: "update IMAP password",
			update: func(t *testing.T, ctx context.Context, db *store.Store, account domain.Account) {
				account.PasswordCiphertext = "new-encrypted-password"
				updated, err := db.UpdateAccount(ctx, account)
				if err != nil {
					t.Fatalf("update IMAP password: %v", err)
				}
				if updated.PasswordCiphertext != account.PasswordCiphertext {
					t.Fatalf("password ciphertext = %q, want updated value", updated.PasswordCiphertext)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := openTestStore(t)
			account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
			aliasOne := createAlias(t, ctx, db, account.ID, "relay-one@icloud.com", []byte("account-reset-one"))
			aliasTwo := createAlias(t, ctx, db, account.ID, "relay-two@icloud.com", []byte("account-reset-two"))
			otherAccount := createAccount(t, ctx, db, "Other", "other@icloud.com")
			otherAlias := createAlias(t, ctx, db, otherAccount.ID, "other-relay@icloud.com", []byte("account-reset-other"))
			syncedAt := time.Date(2026, 8, 6, 15, 0, 0, 0, time.UTC)

			for index, alias := range []domain.Alias{aliasOne, aliasTwo, otherAlias} {
				mustUpsert(t, ctx, db, domain.LatestMessage{
					AliasID: alias.ID, UIDValidity: 20, UID: uint32(index + 1),
					Subject: "snapshot", InternalDate: syncedAt, SyncedAt: syncedAt,
				})
				if err := db.UpdateAliasSyncStatus(ctx, alias.ID, domain.SyncStatusError, "old failure", &syncedAt); err != nil {
					t.Fatalf("set alias %d sync state: %v", alias.ID, err)
				}
			}
			if err := db.UpdateAccountSyncStatus(ctx, account.ID, domain.SyncStatusError, "old failure", &syncedAt); err != nil {
				t.Fatalf("set account sync state: %v", err)
			}
			if err := db.SetAliasEnabled(ctx, aliasTwo.ID, false); err != nil {
				t.Fatalf("disable second alias: %v", err)
			}

			tt.update(t, ctx, db, account)

			assertAliasSnapshotReset(t, ctx, db, aliasOne.ID, true)
			assertAliasSnapshotReset(t, ctx, db, aliasTwo.ID, false)
			updatedAccount, err := db.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatalf("get reset account: %v", err)
			}
			if updatedAccount.LastSyncStatus != domain.SyncStatusPending || updatedAccount.LastSyncError != "" || updatedAccount.LastSyncedAt != nil {
				t.Fatalf("reset account state = %#v", updatedAccount)
			}

			other, err := db.GetAlias(ctx, otherAlias.ID)
			if err != nil {
				t.Fatalf("get unrelated alias: %v", err)
			}
			if other.LastSyncStatus != domain.SyncStatusError || other.LastSyncError != "old failure" || other.LastSyncedAt == nil {
				t.Fatalf("unrelated alias state changed: %#v", other)
			}
			assertLatestSubject(t, ctx, db, otherAlias.ID, "snapshot", 20, 3)
		})
	}
}

func TestUpdateAccountRollsBackCredentialWhenSnapshotResetFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "relay@icloud.com", []byte("rollback-hash"))
	syncedAt := time.Date(2026, 8, 6, 16, 0, 0, 0, time.UTC)
	mustUpsert(t, ctx, db, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 30, UID: 40, Subject: "retained snapshot",
		InternalDate: syncedAt, SyncedAt: syncedAt,
	})
	if _, err := db.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_alias_status_reset
		BEFORE UPDATE OF last_sync_status ON aliases
		BEGIN SELECT RAISE(ABORT, 'forced reset failure'); END`); err != nil {
		t.Fatalf("create reset failure trigger: %v", err)
	}

	account.PasswordCiphertext = "new-encrypted-password"
	if _, err := db.UpdateAccount(ctx, account); err == nil {
		t.Fatal("credential update unexpectedly succeeded")
	}
	stored, err := db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("get rolled back account: %v", err)
	}
	if stored.PasswordCiphertext != "encrypted" {
		t.Fatalf("credential changed despite rollback: %q", stored.PasswordCiphertext)
	}
	assertLatestSubject(t, ctx, db, alias.ID, "retained snapshot", 30, 40)
}

func TestUpdateAccountIdentityChangeResetsAccountStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
	syncedAt := time.Date(2026, 8, 6, 17, 0, 0, 0, time.UTC)
	if err := db.UpdateAccountSyncStatus(ctx, account.ID, domain.SyncStatusOK, "", &syncedAt); err != nil {
		t.Fatalf("mark account synced: %v", err)
	}

	account.Email = "replacement@icloud.com"
	account.IMAPUsername = "replacement@icloud.com"
	updated, err := db.UpdateAccount(ctx, account)
	if err != nil {
		t.Fatalf("update account identity: %v", err)
	}
	if updated.Email != account.Email || updated.IMAPUsername != account.IMAPUsername {
		t.Fatalf("updated identity = (%q, %q)", updated.Email, updated.IMAPUsername)
	}
	if updated.LastSyncStatus != domain.SyncStatusPending || updated.LastSyncError != "" || updated.LastSyncedAt != nil {
		t.Fatalf("identity change retained old sync state: %#v", updated)
	}
}

func TestUpdateAccountRejectsIdentityChangeAfterAliasCreation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
	createAlias(t, ctx, db, account.ID, "relay@icloud.com", []byte("identity-lock-hash"))

	account.Name = "must roll back too"
	account.Email = "replacement@icloud.com"
	account.IMAPUsername = "replacement@icloud.com"
	if _, err := db.UpdateAccount(ctx, account); !errors.Is(err, store.ErrAccountIdentityLocked) {
		t.Fatalf("identity update error = %v, want ErrAccountIdentityLocked", err)
	}
	stored, err := db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if stored.Email != "primary@icloud.com" || stored.IMAPUsername != "primary@icloud.com" || stored.Name != "Primary" {
		t.Fatalf("identity conflict partially updated account: %#v", stored)
	}
}

func TestEnabledAliasLimitIsEnforcedOnCreateAndReenable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
	for index := 0; index < domain.MaxEnabledAliasesPerAccount; index++ {
		createAlias(t, ctx, db, account.ID, fmt.Sprintf("relay-%03d@icloud.com", index), []byte(fmt.Sprintf("limit-hash-%03d", index)))
	}

	if _, err := db.CreateAlias(ctx, domain.Alias{
		AccountID: account.ID, Address: "overflow@icloud.com", APIKeyHash: []byte("overflow-hash"),
		APIKeyPrefix: "overflow", Enabled: true,
	}); !errors.Is(err, store.ErrAliasLimit) {
		t.Fatalf("257th enabled alias error = %v, want ErrAliasLimit", err)
	}
	disabled, err := db.CreateAlias(ctx, domain.Alias{
		AccountID: account.ID, Address: "disabled-overflow@icloud.com", APIKeyHash: []byte("disabled-overflow-hash"),
		APIKeyPrefix: "disabled", Enabled: false,
	})
	if err != nil {
		t.Fatalf("create disabled alias beyond enabled limit: %v", err)
	}
	if err := db.SetAliasEnabled(ctx, disabled.ID, true); !errors.Is(err, store.ErrAliasLimit) {
		t.Fatalf("re-enable alias beyond limit error = %v, want ErrAliasLimit", err)
	}
	stored, err := db.GetAlias(ctx, disabled.ID)
	if err != nil {
		t.Fatalf("get rejected alias: %v", err)
	}
	if stored.Enabled {
		t.Fatal("rejected alias was enabled")
	}
}

func TestPasswordVersionRejectsLateSessionAndConcurrentPasswordChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	admin, err := db.CreateAdmin(ctx, "admin", "old-hash")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	oldVersion := admin.PasswordVersion
	if err := db.ChangeAdminPasswordAndRevokeSessions(ctx, admin.ID, oldVersion, "new-hash"); err != nil {
		t.Fatalf("change password: %v", err)
	}

	lateSession := domain.Session{
		AdminID: admin.ID, PasswordVersion: oldVersion, CSRF: "csrf",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := db.CreateSession(ctx, []byte("late-token-hash"), lateSession); !errors.Is(err, store.ErrCredentialsChanged) {
		t.Fatalf("late session error = %v, want ErrCredentialsChanged", err)
	}
	if err := db.ChangeAdminPasswordAndRevokeSessions(ctx, admin.ID, oldVersion, "racing-hash"); !errors.Is(err, store.ErrCredentialsChanged) {
		t.Fatalf("stale password change error = %v, want ErrCredentialsChanged", err)
	}

	updated, err := db.GetAdminByID(ctx, admin.ID)
	if err != nil {
		t.Fatalf("get updated admin: %v", err)
	}
	if updated.PasswordHash != "new-hash" || updated.PasswordVersion != oldVersion+1 {
		t.Fatalf("updated credentials = %#v", updated)
	}
	currentSession := domain.Session{
		AdminID: admin.ID, PasswordVersion: updated.PasswordVersion, CSRF: "csrf-current",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := db.CreateSession(ctx, []byte("current-token-hash"), currentSession); err != nil {
		t.Fatalf("create current session: %v", err)
	}
	if _, err := db.GetSessionByHash(ctx, []byte("current-token-hash")); err != nil {
		t.Fatalf("get current session: %v", err)
	}
}

func TestResetAdminCredentialsRevokesSessionsAtomically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("successful reset", func(t *testing.T) {
		db := openTestStore(t)
		admin, err := db.CreateAdmin(ctx, "old-admin", "old-hash")
		if err != nil {
			t.Fatalf("create admin: %v", err)
		}
		tokenHash := []byte("session-before-reset")
		if err := db.CreateSession(ctx, tokenHash, domain.Session{
			AdminID: admin.ID, PasswordVersion: admin.PasswordVersion, CSRF: "csrf-before-reset",
			ExpiresAt: time.Now().UTC().Add(time.Hour),
		}); err != nil {
			t.Fatalf("create session: %v", err)
		}

		if err := db.ResetAdminCredentialsAndRevokeSessions(ctx, admin.ID, admin.PasswordVersion, "new-admin", "new-hash"); err != nil {
			t.Fatalf("reset admin credentials: %v", err)
		}
		updated, err := db.GetAdminByID(ctx, admin.ID)
		if err != nil {
			t.Fatalf("get reset admin: %v", err)
		}
		if updated.Username != "new-admin" || updated.PasswordHash != "new-hash" || updated.PasswordVersion != admin.PasswordVersion+1 {
			t.Fatalf("reset admin = %#v", updated)
		}
		if _, err := db.GetSessionByHash(ctx, tokenHash); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("session after credentials reset error = %v, want ErrNotFound", err)
		}
	})

	t.Run("username conflict rolls back", func(t *testing.T) {
		db := openTestStore(t)
		if _, err := db.CreateAdmin(ctx, "first-admin", "first-hash"); err != nil {
			t.Fatalf("create first admin: %v", err)
		}
		second, err := db.CreateAdmin(ctx, "second-admin", "second-hash")
		if err != nil {
			t.Fatalf("create second admin: %v", err)
		}
		tokenHash := []byte("session-that-must-survive")
		if err := db.CreateSession(ctx, tokenHash, domain.Session{
			AdminID: second.ID, PasswordVersion: second.PasswordVersion, CSRF: "csrf-before-conflict",
			ExpiresAt: time.Now().UTC().Add(time.Hour),
		}); err != nil {
			t.Fatalf("create second admin session: %v", err)
		}

		if err := db.ResetAdminCredentialsAndRevokeSessions(ctx, second.ID, second.PasswordVersion, "first-admin", "changed-hash"); err == nil {
			t.Fatal("conflicting credentials reset unexpectedly succeeded")
		}
		unchanged, err := db.GetAdminByID(ctx, second.ID)
		if err != nil {
			t.Fatalf("get second admin after rollback: %v", err)
		}
		if unchanged.Username != second.Username || unchanged.PasswordHash != second.PasswordHash || unchanged.PasswordVersion != second.PasswordVersion {
			t.Fatalf("conflicting reset changed admin: %#v", unchanged)
		}
		if _, err := db.GetSessionByHash(ctx, tokenHash); err != nil {
			t.Fatalf("conflicting reset revoked session: %v", err)
		}
	})
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test store: %v", err)
		}
	})
	return db
}

func createAccount(t *testing.T, ctx context.Context, db *store.Store, name, email string) domain.Account {
	t.Helper()
	account, err := db.CreateAccount(ctx, domain.Account{
		Name: name, Email: email, IMAPHost: "imap.mail.me.com", IMAPPort: 993,
		IMAPUsername: email, PasswordCiphertext: "encrypted", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create account %q: %v", email, err)
	}
	return account
}

func createAlias(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	accountID int64,
	address string,
	hash []byte,
) domain.Alias {
	t.Helper()
	alias, err := db.CreateAlias(ctx, domain.Alias{
		AccountID: accountID, Address: address, Label: address,
		APIKeyHash: hash, APIKeyPrefix: "test", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create alias %q: %v", address, err)
	}
	return alias
}

func mustUpsert(t *testing.T, ctx context.Context, db *store.Store, message domain.LatestMessage) {
	t.Helper()
	changed, err := db.UpsertLatestMessage(ctx, message)
	if err != nil {
		t.Fatalf("upsert latest message: %v", err)
	}
	if !changed {
		t.Fatal("latest message upsert unexpectedly ignored")
	}
}

func assertLatestSubject(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	aliasID int64,
	subject string,
	uidValidity, uid uint32,
) {
	t.Helper()
	message, err := db.GetLatestMessage(ctx, aliasID)
	if err != nil {
		t.Fatalf("get latest message: %v", err)
	}
	if message.Subject != subject || message.UIDValidity != uidValidity || message.UID != uid {
		t.Fatalf("latest message = (%q, %d, %d), want (%q, %d, %d)",
			message.Subject, message.UIDValidity, message.UID, subject, uidValidity, uid)
	}
}

func assertAliasSnapshotReset(t *testing.T, ctx context.Context, db *store.Store, aliasID int64, wantEnabled bool) {
	t.Helper()
	alias, err := db.GetAlias(ctx, aliasID)
	if err != nil {
		t.Fatalf("get reset alias: %v", err)
	}
	if alias.Enabled != wantEnabled {
		t.Fatalf("reset alias enabled = %v, want %v", alias.Enabled, wantEnabled)
	}
	if alias.LastSyncStatus != domain.SyncStatusPending || alias.LastSyncError != "" || alias.LastSyncedAt != nil {
		t.Fatalf("reset alias state = %#v", alias)
	}
	if alias.LatestReceivedAt != nil {
		t.Fatalf("reset alias still exposes latest received time: %v", alias.LatestReceivedAt)
	}
	if _, err := db.GetLatestMessage(ctx, aliasID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("latest message after reset error = %v, want ErrNotFound", err)
	}
}

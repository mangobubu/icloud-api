package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestHTTPWriteTimeoutCoversConfiguredSyncTimeout(t *testing.T) {
	for _, syncTimeout := range []time.Duration{10 * time.Second, 2 * time.Minute, 30 * time.Minute} {
		writeTimeout := httpWriteTimeout(syncTimeout)
		if writeTimeout <= syncTimeout {
			t.Fatalf("同步时限 %v 对应的 HTTP 写超时 = %v", syncTimeout, writeTimeout)
		}
		if writeTimeout-syncTimeout != 10*time.Second {
			t.Fatalf("HTTP 写超时余量 = %v, want %v", writeTimeout-syncTimeout, 10*time.Second)
		}
	}
}

func TestBootstrapAdminDoesNotOverwriteExistingCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openMainTestStore(t)
	const oldPassword = "old-password-123"
	const newPassword = "new-password-456"

	if err := bootstrapAdmin(ctx, db, "old-admin", oldPassword); err != nil {
		t.Fatalf("bootstrap initial admin: %v", err)
	}
	if err := bootstrapAdmin(ctx, db, "new-admin", newPassword); err != nil {
		t.Fatalf("bootstrap existing database: %v", err)
	}
	admins, err := db.ListAdmins(ctx)
	if err != nil {
		t.Fatalf("list admins: %v", err)
	}
	if len(admins) != 1 || admins[0].Username != "old-admin" {
		t.Fatalf("existing admin was replaced: %#v", admins)
	}
	if bcrypt.CompareHashAndPassword([]byte(admins[0].PasswordHash), []byte(oldPassword)) != nil {
		t.Fatal("existing password was replaced")
	}
	if matches, err := configuredAdminMatches(ctx, db, "new-admin", newPassword); err != nil || matches {
		t.Fatalf("new environment credentials match = %v, err = %v; want false", matches, err)
	}
}

func TestResetAdminCredentialsPreservesBusinessDataAndRevokesSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openMainTestStore(t)
	const oldPassword = "old-password-123"
	const newPassword = "new-password-456"

	if err := bootstrapAdmin(ctx, db, "old-admin", oldPassword); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	admin, err := db.GetAdminByUsername(ctx, "old-admin")
	if err != nil {
		t.Fatalf("get initial admin: %v", err)
	}
	tokenHash := []byte("session-before-cli-reset")
	if err := db.CreateSession(ctx, tokenHash, domain.Session{
		AdminID: admin.ID, PasswordVersion: admin.PasswordVersion, CSRF: "csrf-before-cli-reset",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	account, err := db.CreateAccount(ctx, domain.Account{
		Name: "Primary", Email: "primary@icloud.com", IMAPHost: "imap.mail.me.com", IMAPPort: 993,
		IMAPUsername: "primary@icloud.com", PasswordCiphertext: "encrypted", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create business data: %v", err)
	}

	username, err := resetAdminCredentials(ctx, db, "new-admin", newPassword)
	if err != nil {
		t.Fatalf("reset admin credentials: %v", err)
	}
	if username != "new-admin" {
		t.Fatalf("reset username = %q", username)
	}
	updated, err := db.GetAdminByUsername(ctx, "new-admin")
	if err != nil {
		t.Fatalf("get reset admin: %v", err)
	}
	if updated.ID != admin.ID || updated.PasswordVersion != admin.PasswordVersion+1 {
		t.Fatalf("reset admin identity/version = %#v", updated)
	}
	if bcrypt.CompareHashAndPassword([]byte(updated.PasswordHash), []byte(newPassword)) != nil {
		t.Fatal("reset password hash does not match configured password")
	}
	if _, err := db.GetSessionByHash(ctx, tokenHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old session after reset error = %v, want ErrNotFound", err)
	}
	if _, err := db.GetAccount(ctx, account.ID); err != nil {
		t.Fatalf("business data was not preserved: %v", err)
	}
	if matches, err := configuredAdminMatches(ctx, db, "new-admin", newPassword); err != nil || !matches {
		t.Fatalf("reset environment credentials match = %v, err = %v; want true", matches, err)
	}
}

func TestResetAdminCredentialsRejectsAmbiguousAdminSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openMainTestStore(t)
	if _, err := db.CreateAdmin(ctx, "first-admin", "first-hash"); err != nil {
		t.Fatalf("create first admin: %v", err)
	}
	if _, err := db.CreateAdmin(ctx, "second-admin", "second-hash"); err != nil {
		t.Fatalf("create second admin: %v", err)
	}

	_, err := resetAdminCredentials(ctx, db, "unknown-admin", "new-password-456")
	if err == nil || !strings.Contains(err.Error(), "无法安全确定") {
		t.Fatalf("ambiguous reset error = %v", err)
	}

	if _, err := resetAdminCredentials(ctx, db, "second-admin", "new-password-456"); err != nil {
		t.Fatalf("reset explicitly selected admin: %v", err)
	}
	first, err := db.GetAdminByUsername(ctx, "first-admin")
	if err != nil {
		t.Fatalf("get untouched admin: %v", err)
	}
	second, err := db.GetAdminByUsername(ctx, "second-admin")
	if err != nil {
		t.Fatalf("get selected admin: %v", err)
	}
	if first.PasswordHash != "first-hash" || second.PasswordVersion != 2 {
		t.Fatalf("selected reset changed wrong administrator: first=%#v second=%#v", first, second)
	}
}

func TestValidateAdminCredentialsBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		username string
		password string
		wantErr  bool
	}{
		{name: "valid minimum", username: "admin", password: strings.Repeat("a", 12)},
		{name: "valid maximum", username: "admin", password: strings.Repeat("a", 72)},
		{name: "empty username", username: "", password: strings.Repeat("a", 12), wantErr: true},
		{name: "long username", username: strings.Repeat("u", 129), password: strings.Repeat("a", 12), wantErr: true},
		{name: "short password", username: "admin", password: strings.Repeat("a", 11), wantErr: true},
		{name: "long password", username: "admin", password: strings.Repeat("a", 73), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAdminCredentials(test.username, test.password)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateAdminCredentials() error = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}

func openMainTestStore(t *testing.T) *store.Store {
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

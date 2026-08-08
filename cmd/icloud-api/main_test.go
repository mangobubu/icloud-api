package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

func TestHTTPWriteTimeoutCoversConfiguredSyncTimeout(t *testing.T) {
	for _, syncTimeout := range []time.Duration{10 * time.Second, 2 * time.Minute, 30 * time.Minute} {
		writeTimeout := httpWriteTimeout(syncTimeout)
		if writeTimeout <= 2*syncTimeout {
			t.Fatalf("同步时限 %v 对应的 HTTP 写超时 = %v", syncTimeout, writeTimeout)
		}
		if writeTimeout-(2*syncTimeout) != 10*time.Second {
			t.Fatalf("HTTP 写超时余量 = %v, want %v", writeTimeout-(2*syncTimeout), 10*time.Second)
		}
	}
}

func TestSeenOperationTimeoutIsShortAndTracksIMAPTimeout(t *testing.T) {
	tests := []struct {
		name        string
		imapTimeout time.Duration
		want        time.Duration
	}{
		{name: "non-positive", imapTimeout: 0, want: 2 * time.Minute},
		{name: "small", imapTimeout: 5 * time.Second, want: 2 * time.Minute},
		{name: "minimum range", imapTimeout: 18 * time.Second, want: 2 * time.Minute},
		{name: "default", imapTimeout: 25 * time.Second, want: 160 * time.Second},
		{name: "middle", imapTimeout: 45 * time.Second, want: 280 * time.Second},
		{name: "maximum range", imapTimeout: 50 * time.Second, want: 5 * time.Minute},
		{name: "large", imapTimeout: 90 * time.Second, want: 5 * time.Minute},
		{name: "configured maximum", imapTimeout: 5 * time.Minute, want: 5 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := seenOperationTimeout(test.imapTimeout); got != test.want {
				t.Fatalf("seen operation timeout = %v, want %v", got, test.want)
			}
		})
	}
}

type shutdownerStub struct {
	shutdown func(context.Context) error
	close    func() error
}

func (s shutdownerStub) Shutdown(ctx context.Context) error {
	return s.shutdown(ctx)
}

func (s shutdownerStub) Close() error {
	if s.close == nil {
		return nil
	}
	return s.close()
}

func TestShutdownHTTPWaitsForBackgroundAfterShutdownError(t *testing.T) {
	wantErr := errors.New("shutdown failed")
	shutdownCalled := make(chan struct{})
	closeCalled := make(chan struct{})
	requestDone := make(chan struct{})
	close(requestDone)
	backgroundDone := make(chan struct{})
	result := make(chan error, 1)

	go func() {
		result <- shutdownHTTPAndWaitForBackground(
			context.Background(),
			shutdownerStub{
				shutdown: func(context.Context) error {
					close(shutdownCalled)
					return wantErr
				},
				close: func() error {
					close(closeCalled)
					return nil
				},
			},
			requestDone,
			backgroundDone,
			nil,
		)
	}()

	<-shutdownCalled
	<-closeCalled
	select {
	case err := <-result:
		t.Fatalf("background still running, shutdown returned early: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	close(backgroundDone)
	select {
	case err := <-result:
		if !errors.Is(err, wantErr) {
			t.Fatalf("shutdown error = %v, want wrapping %v", err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after background stopped")
	}
}

func TestShutdownDeadlineReturnsWithoutWaitingForeverForOwners(t *testing.T) {
	requestDone := make(chan struct{})
	close(requestDone)
	backgroundDone := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)

	go func() {
		result <- shutdownHTTPAndWaitForBackground(
			ctx,
			shutdownerStub{shutdown: func(context.Context) error { return nil }},
			requestDone,
			backgroundDone,
			nil,
		)
	}()

	<-ctx.Done()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after deadline")
	}
}

func TestShutdownDeadlineForcesHTTPCloseWhenShutdownStalls(t *testing.T) {
	started := make(chan struct{})
	releaseShutdown := make(chan struct{})
	closeCalled := make(chan struct{})
	requestDone := make(chan struct{})
	close(requestDone)
	backgroundDone := make(chan struct{})
	close(backgroundDone)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)

	go func() {
		result <- shutdownHTTPAndWaitForBackground(
			ctx,
			shutdownerStub{
				shutdown: func(context.Context) error {
					close(started)
					<-releaseShutdown
					return nil
				},
				close: func() error {
					close(closeCalled)
					return nil
				},
			},
			requestDone,
			backgroundDone,
			nil,
		)
	}()
	<-started
	select {
	case <-closeCalled:
	case <-time.After(time.Second):
		t.Fatal("shutdown deadline did not force HTTP close")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after forcing HTTP close")
	}
	close(releaseShutdown)
}

func TestDrainingHandlerWaitsForActiveRequestsAndRejectsNewOnes(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan struct{})
	handler := newDrainingHandler(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		response.WriteHeader(http.StatusNoContent)
	}))

	go func() {
		defer close(firstDone)
		handler.ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	}()
	<-entered
	drained := handler.Stop()

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("request after draining status = %d, want %d", second.Code, http.StatusServiceUnavailable)
	}
	select {
	case <-drained:
		t.Fatal("handler reported drained while a request was active")
	default:
	}

	close(release)
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("handler did not drain after active request finished")
	}
	<-firstDone
}

func TestInitializeCipherImportsValidatedLegacyDataBeforeFirstBinding(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x51}, 32)
	wantKey := append([]byte(nil), key...)
	fixtureCipher, err := secure.NewCipher(wantKey)
	if err != nil {
		t.Fatal(err)
	}
	imapCiphertext, err := fixtureCipher.Encrypt("app-specific-password")
	if err != nil {
		t.Fatal(err)
	}
	appleCiphertext, err := fixtureCipher.EncryptAppleSession(`{"token":"legacy"}`)
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeStartupStore{
		expectedKey:           wantKey,
		legacyIMAPCiphertext:  imapCiphertext,
		legacyAppleCiphertext: appleCiphertext,
	}

	cipher, err := initializeCipherWithStore(context.Background(), database, key, "legacy.db")
	if err != nil {
		t.Fatalf("initialize cipher with legacy data: %v", err)
	}
	if got := strings.Join(database.events, ","); got != "initialize" {
		t.Fatalf("startup operations = %q, want one atomic initialize call", got)
	}
	if !bytes.Equal(key, make([]byte, len(key))) {
		t.Fatal("master key was not cleared after initialization")
	}
	encrypted, err := cipher.Encrypt("credential")
	if err != nil {
		t.Fatalf("encrypt after clearing source key: %v", err)
	}
	if plaintext, err := cipher.Decrypt(encrypted); err != nil || plaintext != "credential" {
		t.Fatalf("decrypt after clearing source key = %q, %v", plaintext, err)
	}
}

func TestInitializeCipherDoesNotBindFingerprintWhenLegacyKeyIsWrong(t *testing.T) {
	t.Parallel()
	correctKey := bytes.Repeat([]byte{0x53}, 32)
	fixtureCipher, err := secure.NewCipher(correctKey)
	if err != nil {
		t.Fatal(err)
	}
	legacyCiphertext, err := fixtureCipher.Encrypt("app-specific-password")
	if err != nil {
		t.Fatal(err)
	}
	wrongKey := bytes.Repeat([]byte{0x54}, 32)
	database := &fakeStartupStore{
		expectedKey:          append([]byte(nil), wrongKey...),
		legacyIMAPCiphertext: legacyCiphertext,
	}

	cipher, err := initializeCipherWithStore(context.Background(), database, wrongKey, "legacy.db")
	if cipher != nil || err == nil {
		t.Fatalf("wrong legacy key result = (%v, %v)", cipher, err)
	}
	if got := strings.Join(database.events, ","); got != "initialize" {
		t.Fatalf("wrong-key startup operations = %q, want one atomic initialize call", got)
	}
	if !bytes.Equal(wrongKey, make([]byte, len(wrongKey))) {
		t.Fatal("wrong master key was not cleared after legacy validation failure")
	}
}

func TestInitializeCipherStopsOnStoredMismatchBeforeImportAndClearsKey(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x52}, 32)
	database := &fakeStartupStore{
		expectedKey: append([]byte(nil), key...),
		storedErr:   store.ErrMasterKeyMismatch,
	}

	cipher, err := initializeCipherWithStore(context.Background(), database, key, "legacy.db")
	if cipher != nil || !errors.Is(err, store.ErrMasterKeyMismatch) {
		t.Fatalf("mismatch result = (%v, %v), want nil cipher and ErrMasterKeyMismatch", cipher, err)
	}
	if got := strings.Join(database.events, ","); got != "initialize" {
		t.Fatalf("stored mismatch operations = %q, want one atomic initialize call", got)
	}
	if !strings.Contains(err.Error(), "keys 卷") {
		t.Fatalf("mismatch error lacks recovery guidance: %v", err)
	}
	if !bytes.Equal(key, make([]byte, len(key))) {
		t.Fatal("master key was not cleared after mismatch")
	}
}

func TestInitializeCipherLabelsLegacySQLiteInitializationError(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x55}, 32)
	sourceErr := errors.New("read legacy SQLite database: unable to open database file (14)")
	database := &fakeStartupStore{
		expectedKey: append([]byte(nil), key...),
		storedErr:   fmt.Errorf("%w: %w", store.ErrLegacySQLiteImport, sourceErr),
	}

	cipher, err := initializeCipherWithStore(
		context.Background(), database, key, " /app/legacy/icloud-api.db ",
	)
	if cipher != nil || err == nil {
		t.Fatalf("legacy SQLite initialization result = (%v, %v), want nil cipher and error", cipher, err)
	}
	if !errors.Is(err, sourceErr) {
		t.Fatalf("legacy SQLite initialization error does not wrap source: %v", err)
	}
	for _, want := range []string{
		"迁移旧 SQLite 数据并校验 PostgreSQL 主密钥",
		`/app/legacy/icloud-api.db`,
		"read legacy SQLite database",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("legacy SQLite initialization error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "校验 PostgreSQL 主密钥指纹:") {
		t.Fatalf("legacy SQLite initialization retained misleading fingerprint-only prefix: %v", err)
	}
	if !bytes.Equal(key, make([]byte, len(key))) {
		t.Fatal("master key was not cleared after legacy SQLite initialization failure")
	}
}

func TestMasterKeyVerificationErrorKeepsFingerprintContextForPostgreSQLErrors(t *testing.T) {
	t.Parallel()
	for _, legacyPath := range []string{"", "/app/legacy/icloud-api.db"} {
		sourceErr := errors.New("read stored master key fingerprint: unavailable")
		err := masterKeyVerificationError(sourceErr, legacyPath)
		if !errors.Is(err, sourceErr) {
			t.Fatalf("master key verification error does not wrap source: %v", err)
		}
		if !strings.Contains(err.Error(), "校验 PostgreSQL 主密钥指纹:") {
			t.Fatalf("master key verification error lacks fingerprint context: %v", err)
		}
		if strings.Contains(err.Error(), "旧 SQLite") {
			t.Fatalf("PostgreSQL error unexpectedly labeled as legacy SQLite migration: %v", err)
		}
	}
}

func TestAdminResetInitializesStoreBeforeMutationAndClearsKey(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x63}, 32)
	database := &fakeStartupStore{expectedKey: append([]byte(nil), key...)}
	resetCalled := false

	username, err := initializeAndResetAdmin(
		context.Background(),
		database,
		key,
		"legacy.db",
		func() (string, error) {
			resetCalled = true
			if got := strings.Join(database.events, ","); got != "initialize" {
				t.Fatalf("operations before admin mutation = %q, want initialize", got)
			}
			return "admin", nil
		},
	)
	if err != nil || username != "admin" {
		t.Fatalf("initialized admin reset = (%q, %v)", username, err)
	}
	if !resetCalled {
		t.Fatal("admin reset callback was not called after initialization")
	}
	if database.legacyPath != "legacy.db" {
		t.Fatalf("legacy import path = %q, want legacy.db", database.legacyPath)
	}
	if !bytes.Equal(key, make([]byte, len(key))) {
		t.Fatal("admin reset master key was not cleared")
	}
}

func TestAdminResetInitializationFailurePreventsMutation(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x64}, 32)
	database := &fakeStartupStore{
		expectedKey: append([]byte(nil), key...),
		storedErr:   store.ErrMasterKeyMismatch,
	}
	resetCalled := false

	username, err := initializeAndResetAdmin(
		context.Background(),
		database,
		key,
		"legacy.db",
		func() (string, error) {
			resetCalled = true
			return "admin", nil
		},
	)
	if username != "" || !errors.Is(err, store.ErrMasterKeyMismatch) {
		t.Fatalf("failed initialization reset = (%q, %v), want master key mismatch", username, err)
	}
	if resetCalled {
		t.Fatal("admin reset mutated data after startup initialization failed")
	}
	if !bytes.Equal(key, make([]byte, len(key))) {
		t.Fatal("failed admin reset did not clear the master key")
	}
}

func TestConcurrentInitializeCipherCannotSplitLegacyImportAndFingerprintBinding(t *testing.T) {
	legacyKey := bytes.Repeat([]byte{0x71}, 32)
	emptyKey := bytes.Repeat([]byte{0x72}, 32)
	legacyCipher, err := secure.NewCipher(legacyKey)
	if err != nil {
		t.Fatal(err)
	}
	legacyCiphertext, err := legacyCipher.Encrypt("legacy-password")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		firstKey   []byte
		firstPath  string
		secondKey  []byte
		secondPath string
	}{
		{
			name:     "legacy import wins",
			firstKey: legacyKey, firstPath: "legacy.db",
			secondKey: emptyKey,
		},
		{
			name:      "empty startup wins",
			firstKey:  emptyKey,
			secondKey: legacyKey, secondPath: "legacy.db",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			release := make(chan struct{})
			database := &serializedStartupStore{
				attempted:        make(chan byte, 2),
				firstLocked:      make(chan struct{}),
				releaseFirst:     release,
				firstKeyMarker:   test.firstKey[0],
				legacyCiphertext: legacyCiphertext,
			}
			type result struct {
				cipher *secure.Cipher
				err    error
			}
			firstResult := make(chan result, 1)
			secondResult := make(chan result, 1)
			go func() {
				cipher, err := initializeCipherWithStore(
					context.Background(), database, append([]byte(nil), test.firstKey...), test.firstPath,
				)
				firstResult <- result{cipher: cipher, err: err}
			}()
			<-database.firstLocked
			go func() {
				cipher, err := initializeCipherWithStore(
					context.Background(), database, append([]byte(nil), test.secondKey...), test.secondPath,
				)
				secondResult <- result{cipher: cipher, err: err}
			}()
			<-database.attempted
			<-database.attempted
			close(release)

			first := <-firstResult
			second := <-secondResult
			if first.err != nil || first.cipher == nil {
				t.Fatalf("first initialization = (%v, %v), want success", first.cipher, first.err)
			}
			if second.cipher != nil || !errors.Is(second.err, store.ErrMasterKeyMismatch) {
				t.Fatalf("second initialization = (%v, %v), want master key mismatch", second.cipher, second.err)
			}
			if !bytes.Equal(database.fingerprintKey, test.firstKey) {
				t.Fatalf("fingerprint key = %x, want first key %x", database.fingerprintKey, test.firstKey)
			}
			if test.firstPath == "" {
				if len(database.ciphertextKey) != 0 {
					t.Fatalf("losing legacy startup imported ciphertext under %x", database.ciphertextKey)
				}
			} else if !bytes.Equal(database.ciphertextKey, database.fingerprintKey) {
				t.Fatalf("ciphertext key %x differs from fingerprint key %x", database.ciphertextKey, database.fingerprintKey)
			}
		})
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

func TestStartupVerificationChecksAdminBootstrapWithoutWriting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openMainTestStore(t)

	status, err := checkAdminBootstrap(ctx, db, "admin", "generated-password-123")
	if err != nil {
		t.Fatalf("check empty admin bootstrap: %v", err)
	}
	if status != adminBootstrapRequired {
		t.Fatalf("empty admin bootstrap status = %q, want %q", status, adminBootstrapRequired)
	}
	if count, err := db.CountAdmins(ctx); err != nil || count != 0 {
		t.Fatalf("startup verification wrote admins: count=%d err=%v", count, err)
	}

	if err := bootstrapAdmin(ctx, db, "legacy-admin", "legacy-password-123"); err != nil {
		t.Fatalf("create legacy admin: %v", err)
	}
	status, err = checkAdminBootstrap(ctx, db, "admin", "")
	if err != nil {
		t.Fatalf("check imported admin without configured password: %v", err)
	}
	if status != adminBootstrapExisting {
		t.Fatalf("imported admin bootstrap status = %q, want %q", status, adminBootstrapExisting)
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

type fakeStartupStore struct {
	events                []string
	expectedKey           []byte
	legacyPath            string
	storedErr             error
	bindErr               error
	legacyIMAPCiphertext  string
	legacyAppleCiphertext string
}

type serializedStartupStore struct {
	mu               sync.Mutex
	attempted        chan byte
	firstLocked      chan struct{}
	releaseFirst     <-chan struct{}
	firstKeyMarker   byte
	legacyCiphertext string
	fingerprintKey   []byte
	ciphertextKey    []byte
}

func (database *serializedStartupStore) InitializeMasterKeyWithLegacySQLite(
	_ context.Context,
	key []byte,
	legacyPath string,
	validator store.MasterKeyCipherValidator,
) error {
	database.attempted <- key[0]
	database.mu.Lock()
	defer database.mu.Unlock()

	if key[0] == database.firstKeyMarker {
		close(database.firstLocked)
		<-database.releaseFirst
	}
	if len(database.fingerprintKey) != 0 && !bytes.Equal(database.fingerprintKey, key) {
		return store.ErrMasterKeyMismatch
	}
	if legacyPath != "" {
		if _, err := validator.Decrypt(database.legacyCiphertext); err != nil {
			return err
		}
		database.ciphertextKey = append([]byte(nil), key...)
	}
	database.fingerprintKey = append([]byte(nil), key...)
	return nil
}

func (database *fakeStartupStore) InitializeMasterKeyWithLegacySQLite(
	_ context.Context,
	key []byte,
	legacyPath string,
	validator store.MasterKeyCipherValidator,
) error {
	database.events = append(database.events, "initialize")
	database.legacyPath = legacyPath
	if !bytes.Equal(key, database.expectedKey) {
		return errors.New("initialization received a modified key")
	}
	if database.storedErr != nil {
		return database.storedErr
	}
	if database.legacyIMAPCiphertext != "" {
		if _, err := validator.Decrypt(database.legacyIMAPCiphertext); err != nil {
			return err
		}
	}
	if database.legacyAppleCiphertext != "" {
		if _, err := validator.DecryptAppleSession(database.legacyAppleCiphertext); err != nil {
			return err
		}
	}
	return database.bindErr
}

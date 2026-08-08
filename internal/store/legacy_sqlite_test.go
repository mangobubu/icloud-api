package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"icloud-api/internal/secure"
)

var _ LegacySQLiteCipherValidator = (*secure.Cipher)(nil)

type legacyCipherValidatorStub struct {
	credentialValues []string
	appleValues      []string
	err              error
}

func (validator *legacyCipherValidatorStub) Decrypt(value string) (string, error) {
	validator.credentialValues = append(validator.credentialValues, value)
	return "", validator.err
}

func (validator *legacyCipherValidatorStub) DecryptAppleSession(value string) (string, error) {
	validator.appleValues = append(validator.appleValues, value)
	return "", validator.err
}

func TestLegacySQLiteReadOnlyDSN(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE fixture(value TEXT NOT NULL)`); err != nil {
		_ = db.Close()
		t.Fatalf("create fixture: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO fixture(value) VALUES('preserved')`); err != nil {
		_ = db.Close()
		t.Fatalf("write fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	dsn, resolvedPath, err := legacySQLiteReadOnlyDSN(databasePath)
	if err != nil {
		t.Fatalf("build read-only DSN: %v", err)
	}
	if resolvedPath != databasePath {
		t.Fatalf("resolved path = %q, want %q", resolvedPath, databasePath)
	}
	if !strings.Contains(dsn, "mode=ro") {
		t.Fatalf("read-only DSN does not contain mode=ro: %q", dsn)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse read-only DSN: %v", err)
	}
	if parsed.Query().Get("immutable") != "1" {
		t.Fatalf("clean read-only snapshot is not immutable: %q", dsn)
	}

	readOnly, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open read-only fixture: %v", err)
	}
	t.Cleanup(func() { _ = readOnly.Close() })
	readTx, err := readOnly.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read-only snapshot: %v", err)
	}
	var value string
	if err := readTx.QueryRowContext(ctx, `SELECT value FROM fixture`).Scan(&value); err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if value != "preserved" {
		t.Fatalf("fixture value = %q, want preserved", value)
	}
	if err := readTx.Commit(); err != nil {
		t.Fatalf("commit read-only snapshot: %v", err)
	}
	if _, err := readOnly.ExecContext(ctx, `DELETE FROM fixture`); err == nil {
		t.Fatal("read-only fixture unexpectedly accepted a write")
	}
}

func TestLegacySQLiteReadOnlyDSNOpensCleanWALDatabaseFromReadOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only directory permissions are POSIX-specific")
	}

	ctx := context.Background()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "legacy.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open WAL fixture: %v", err)
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		_ = db.Close()
		t.Fatalf("enable WAL fixture: %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		_ = db.Close()
		t.Fatalf("fixture journal mode = %q, want wal", journalMode)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE fixture(value TEXT NOT NULL);
		INSERT INTO fixture(value) VALUES('preserved');
	`); err != nil {
		_ = db.Close()
		t.Fatalf("write WAL fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close WAL fixture: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(databasePath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cleanly closed fixture retained %s sidecar: %v", suffix, err)
		}
	}

	if err := os.Chmod(databasePath, 0o400); err != nil {
		t.Fatalf("make WAL fixture read-only: %v", err)
	}
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatalf("make WAL fixture directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })

	dsn, _, err := legacySQLiteReadOnlyDSN(databasePath)
	if err != nil {
		t.Fatalf("build read-only WAL DSN: %v", err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse read-only WAL DSN: %v", err)
	}
	if parsed.Query().Get("immutable") != "1" {
		t.Fatalf("clean WAL snapshot is not immutable: %q", dsn)
	}
	readOnly, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open read-only WAL fixture: %v", err)
	}
	defer readOnly.Close()
	var value string
	if err := readOnly.QueryRowContext(ctx, `SELECT value FROM fixture`).Scan(&value); err != nil {
		t.Fatalf("read clean WAL fixture from read-only directory: %v", err)
	}
	if value != "preserved" {
		t.Fatalf("read-only WAL fixture value = %q, want preserved", value)
	}
}

func TestLegacySQLiteReadOnlyDSNReadsUncheckpointedWALFromReadOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only directory permissions are POSIX-specific")
	}

	ctx := context.Background()
	sourceDirectory := t.TempDir()
	sourcePath := filepath.Join(sourceDirectory, "legacy.db")
	sourceDB, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatalf("open WAL source: %v", err)
	}
	t.Cleanup(func() { _ = sourceDB.Close() })
	sourceConn, err := sourceDB.Conn(ctx)
	if err != nil {
		t.Fatalf("reserve WAL source connection: %v", err)
	}
	t.Cleanup(func() { _ = sourceConn.Close() })

	var journalMode string
	if err := sourceConn.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		t.Fatalf("enable WAL source: %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Fatalf("source journal mode = %q, want wal", journalMode)
	}
	if _, err := sourceConn.ExecContext(ctx, `CREATE TABLE fixture(value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create WAL source schema: %v", err)
	}
	var busy, logFrames, checkpointedFrames int
	if err := sourceConn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(
		&busy, &logFrames, &checkpointedFrames,
	); err != nil {
		t.Fatalf("checkpoint WAL source schema: %v", err)
	}
	if busy != 0 || logFrames != 0 || checkpointedFrames != 0 {
		t.Fatalf(
			"schema checkpoint result = busy:%d log:%d checkpointed:%d, want 0:0:0",
			busy, logFrames, checkpointedFrames,
		)
	}
	if _, err := sourceConn.ExecContext(ctx, `PRAGMA wal_autocheckpoint = 0`); err != nil {
		t.Fatalf("disable automatic WAL checkpoints: %v", err)
	}
	if _, err := sourceConn.ExecContext(ctx, `INSERT INTO fixture(value) VALUES('wal-only')`); err != nil {
		t.Fatalf("insert WAL-only row: %v", err)
	}
	if info, err := os.Stat(sourcePath + "-wal"); err != nil {
		t.Fatalf("inspect uncheckpointed WAL: %v", err)
	} else if info.Size() <= 32 {
		t.Fatalf("uncheckpointed WAL size = %d, want header and at least one frame", info.Size())
	}

	copyFixture := func(source, destination string) {
		t.Helper()
		contents, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read SQLite snapshot file %s: %v", source, err)
		}
		if err := os.WriteFile(destination, contents, 0o600); err != nil {
			t.Fatalf("write SQLite snapshot file %s: %v", destination, err)
		}
	}

	mainOnlyDirectory := t.TempDir()
	mainOnlyPath := filepath.Join(mainOnlyDirectory, "legacy.db")
	copyFixture(sourcePath, mainOnlyPath)
	mainOnlyURL := &url.URL{Scheme: "file", Path: filepath.ToSlash(mainOnlyPath)}
	mainOnlyQuery := mainOnlyURL.Query()
	mainOnlyQuery.Set("mode", "ro")
	mainOnlyQuery.Set("immutable", "1")
	mainOnlyURL.RawQuery = mainOnlyQuery.Encode()
	mainOnly, err := sql.Open("sqlite", mainOnlyURL.String())
	if err != nil {
		t.Fatalf("open main-only snapshot: %v", err)
	}
	var mainOnlyRows int
	if err := mainOnly.QueryRowContext(ctx, `SELECT COUNT(*) FROM fixture`).Scan(&mainOnlyRows); err != nil {
		_ = mainOnly.Close()
		t.Fatalf("read main-only snapshot: %v", err)
	}
	if err := mainOnly.Close(); err != nil {
		t.Fatalf("close main-only snapshot: %v", err)
	}
	if mainOnlyRows != 0 {
		t.Fatalf("main database already contains %d rows, want WAL-only row", mainOnlyRows)
	}

	snapshotDirectory := t.TempDir()
	snapshotPath := filepath.Join(snapshotDirectory, "legacy.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		copyFixture(sourcePath+suffix, snapshotPath+suffix)
	}
	for _, path := range []string{snapshotPath, snapshotPath + "-wal", snapshotPath + "-shm"} {
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatalf("make SQLite snapshot file read-only: %v", err)
		}
	}
	if err := os.Chmod(snapshotDirectory, 0o500); err != nil {
		t.Fatalf("make SQLite snapshot directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(snapshotDirectory, 0o700) })

	dsn, _, err := legacySQLiteReadOnlyDSN(snapshotPath)
	if err != nil {
		t.Fatalf("build WAL snapshot DSN: %v", err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse WAL snapshot DSN: %v", err)
	}
	if parsed.Query().Get("immutable") != "" {
		t.Fatalf("DSN would ignore an uncheckpointed WAL: %q", dsn)
	}

	readOnly, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open WAL snapshot: %v", err)
	}
	defer readOnly.Close()
	var value string
	if err := readOnly.QueryRowContext(ctx, `SELECT value FROM fixture`).Scan(&value); err != nil {
		t.Fatalf("read row from uncheckpointed WAL: %v", err)
	}
	if value != "wal-only" {
		t.Fatalf("WAL snapshot value = %q, want wal-only", value)
	}
}

func TestLegacySQLiteReadOnlyDSNDecodesFileURIPaths(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "legacy 100%.db")
	fixture, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open encoded-path fixture: %v", err)
	}
	if _, err := fixture.Exec(`CREATE TABLE fixture(value TEXT); INSERT INTO fixture(value) VALUES('ok')`); err != nil {
		_ = fixture.Close()
		t.Fatalf("create encoded-path fixture: %v", err)
	}
	if err := fixture.Close(); err != nil {
		t.Fatalf("close encoded-path fixture: %v", err)
	}
	normalizedPath := filepath.ToSlash(databasePath)
	if !strings.HasPrefix(normalizedPath, "/") {
		normalizedPath = "/" + normalizedPath
	}

	opaquePath := filepath.ToSlash(databasePath)
	for _, rawURI := range []string{
		(&url.URL{Scheme: "file", Path: normalizedPath}).String(),
		"file:" + url.PathEscape(opaquePath),
	} {
		dsn, resolvedPath, err := legacySQLiteReadOnlyDSN(rawURI)
		if err != nil {
			t.Fatalf("build read-only DSN for %q: %v", rawURI, err)
		}
		if resolvedPath != databasePath {
			t.Fatalf("resolved path for %q = %q, want %q", rawURI, resolvedPath, databasePath)
		}
		if !strings.Contains(dsn, "mode=ro") {
			t.Fatalf("read-only DSN for %q does not contain mode=ro: %q", rawURI, dsn)
		}
		readOnly, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("open read-only DSN for %q: %v", rawURI, err)
		}
		var value string
		if err := readOnly.QueryRow(`SELECT value FROM fixture`).Scan(&value); err != nil {
			_ = readOnly.Close()
			t.Fatalf("read encoded-path fixture for %q: %v", rawURI, err)
		}
		if err := readOnly.Close(); err != nil {
			t.Fatalf("close read-only DSN for %q: %v", rawURI, err)
		}
		if value != "ok" {
			t.Fatalf("encoded-path fixture value for %q = %q", rawURI, value)
		}
	}
}

func TestLegacySQLiteCopySpecsHandleOlderSchemas(t *testing.T) {
	t.Parallel()

	v1 := legacySQLiteCopySpecs(false, false, false)
	if len(v1) != 6 {
		t.Fatalf("v1 copy spec count = %d, want 6", len(v1))
	}
	if !strings.Contains(v1[0].selectSQL, "password_hash, 1, created_at") {
		t.Fatalf("v1 admin query does not default password_version: %q", v1[0].selectSQL)
	}
	if !strings.Contains(v1[1].selectSQL, "admin_id, 1, csrf") {
		t.Fatalf("v1 session query does not default password_version: %q", v1[1].selectSQL)
	}

	v3 := legacySQLiteCopySpecs(true, true, true)
	if len(v3) != 7 || v3[len(v3)-1].table != "apple_web_sessions" {
		t.Fatalf("v3 copy specs do not include Apple sessions: %#v", v3)
	}
	for _, spec := range v3 {
		if spec.table == "imap_sync_states" {
			t.Fatal("legacy importer must not synthesize or copy an IMAP cursor")
		}
	}
}

func TestLegacySQLiteReadOnlyDSNDropsUnsafeURIOptions(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "legacy.db")
	normalizedPath := filepath.ToSlash(databasePath)
	if !strings.HasPrefix(normalizedPath, "/") {
		normalizedPath = "/" + normalizedPath
	}
	legacyURL := &url.URL{Scheme: "file", Path: normalizedPath}
	query := legacyURL.Query()
	query.Set("immutable", "1")
	query.Add("_pragma", "journal_mode(WAL)")
	legacyURL.RawQuery = query.Encode()
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(databasePath+suffix, []byte("pending WAL state"), 0o600); err != nil {
			t.Fatalf("create %s fixture: %v", suffix, err)
		}
	}

	dsn, resolvedPath, err := legacySQLiteReadOnlyDSN(legacyURL.String())
	if err != nil {
		t.Fatalf("build read-only URI: %v", err)
	}
	if resolvedPath != databasePath {
		t.Fatalf("resolved path = %q, want %q", resolvedPath, databasePath)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse read-only URI: %v", err)
	}
	if parsed.Query().Get("immutable") != "" {
		t.Fatalf("read-only URI retained immutable option: %q", dsn)
	}
	for _, pragma := range parsed.Query()["_pragma"] {
		if strings.Contains(strings.ToLower(pragma), "journal_mode") {
			t.Fatalf("read-only URI retained journal_mode pragma: %q", dsn)
		}
	}
}

func TestLegacySQLiteReadOnlyDSNNeverIgnoresSQLiteJournals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		suffixes []string
	}{
		{name: "WAL snapshot", suffixes: []string{"-wal", "-shm"}},
		{name: "rollback journal", suffixes: []string{"-journal"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			databasePath := filepath.Join(t.TempDir(), "legacy.db")
			for _, suffix := range test.suffixes {
				if err := os.WriteFile(databasePath+suffix, []byte("pending SQLite sidecar"), 0o600); err != nil {
					t.Fatalf("create %s fixture: %v", suffix, err)
				}
			}
			dsn, _, err := legacySQLiteReadOnlyDSN(databasePath)
			if err != nil {
				t.Fatalf("build read-only DSN: %v", err)
			}
			parsed, err := url.Parse(dsn)
			if err != nil {
				t.Fatalf("parse read-only DSN: %v", err)
			}
			if parsed.Query().Get("immutable") != "" {
				t.Fatalf("DSN would ignore existing %s: %q", test.name, dsn)
			}
		})
	}
}

func TestLegacySQLiteReadOnlyDSNRejectsIncompleteJournalState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		suffixes []string
	}{
		{name: "WAL without SHM", suffixes: []string{"-wal"}},
		{name: "SHM without WAL", suffixes: []string{"-shm"}},
		{name: "WAL mixed with rollback journal", suffixes: []string{"-wal", "-shm", "-journal"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			databasePath := filepath.Join(t.TempDir(), "legacy.db")
			for _, suffix := range test.suffixes {
				if err := os.WriteFile(databasePath+suffix, []byte("SQLite sidecar"), 0o600); err != nil {
					t.Fatalf("create %s fixture: %v", suffix, err)
				}
			}
			if _, _, err := legacySQLiteReadOnlyDSN(databasePath); err == nil {
				t.Fatalf("incomplete journal state %v was accepted", test.suffixes)
			}
		})
	}
}

func TestInspectLegacySQLiteSourceRejectsOrphanSidecars(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "legacy.db")
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(databasePath+suffix, []byte("orphan WAL state"), 0o600); err != nil {
			t.Fatalf("create orphan %s: %v", suffix, err)
		}
	}
	if _, _, err := inspectLegacySQLiteSource(databasePath); err == nil ||
		!strings.Contains(err.Error(), "database is missing") {
		t.Fatalf("orphan SQLite source error = %v", err)
	}
}

func TestVerifyLegacySQLiteSourceUnchangedDetectsChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "database content",
			mutate: func(t *testing.T, databasePath string) {
				t.Helper()
				if err := os.WriteFile(databasePath, []byte("changed source contents"), 0o600); err != nil {
					t.Fatalf("change source fixture: %v", err)
				}
			},
		},
		{
			name: "new journal sidecar",
			mutate: func(t *testing.T, databasePath string) {
				t.Helper()
				if err := os.WriteFile(databasePath+"-journal", []byte("new journal"), 0o600); err != nil {
					t.Fatalf("create concurrent journal fixture: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			databasePath := filepath.Join(t.TempDir(), "legacy.db")
			if err := os.WriteFile(databasePath, []byte("stable source"), 0o600); err != nil {
				t.Fatalf("create source fixture: %v", err)
			}
			state, exists, err := inspectLegacySQLiteSource(databasePath)
			if err != nil || !exists {
				t.Fatalf("inspect source fixture = (%v, %v), want existing source", exists, err)
			}
			if err := verifyLegacySQLiteSourceUnchanged(databasePath, state); err != nil {
				t.Fatalf("unchanged source rejected: %v", err)
			}

			test.mutate(t, databasePath)
			if err := verifyLegacySQLiteSourceUnchanged(databasePath, state); err == nil ||
				!strings.Contains(err.Error(), "source changed during import") {
				t.Fatalf("changed source verification error = %v", err)
			}
		})
	}
}

func TestLegacySQLiteReadOnlyDSNRejectsNonRegularSidecars(t *testing.T) {
	tests := []struct {
		name                   string
		suffix                 string
		kind                   string
		precedingRegularSuffix string
		windowsSkip            bool
	}{
		{name: "WAL directory", suffix: "-wal", kind: "directory"},
		{
			name:                   "SHM symbolic link after regular WAL",
			suffix:                 "-shm",
			kind:                   "symlink",
			precedingRegularSuffix: "-wal",
			windowsSkip:            true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.windowsSkip && runtime.GOOS == "windows" {
				t.Skip("symbolic link creation may require elevated privileges on Windows")
			}

			directory := t.TempDir()
			databasePath := filepath.Join(directory, "legacy.db")
			sidecarPath := databasePath + test.suffix
			if test.precedingRegularSuffix != "" {
				if err := os.WriteFile(
					databasePath+test.precedingRegularSuffix,
					[]byte("regular preceding sidecar"),
					0o600,
				); err != nil {
					t.Fatalf("create preceding regular sidecar: %v", err)
				}
			}
			switch test.kind {
			case "directory":
				if err := os.Mkdir(sidecarPath, 0o700); err != nil {
					t.Fatalf("create sidecar directory: %v", err)
				}
			case "symlink":
				targetPath := filepath.Join(directory, "sidecar-target")
				if err := os.WriteFile(targetPath, []byte("sidecar"), 0o600); err != nil {
					t.Fatalf("create sidecar link target: %v", err)
				}
				if err := os.Symlink(targetPath, sidecarPath); err != nil {
					t.Fatalf("create sidecar symbolic link: %v", err)
				}
			default:
				t.Fatalf("unknown sidecar fixture kind %q", test.kind)
			}

			if _, _, err := legacySQLiteReadOnlyDSN(databasePath); err == nil {
				t.Fatalf("non-regular %s sidecar was accepted", test.suffix)
			} else if !strings.Contains(err.Error(), "not a regular file") ||
				!strings.Contains(err.Error(), sidecarPath) {
				t.Fatalf("non-regular %s sidecar error = %v", test.suffix, err)
			}
		})
	}
}

func TestImportLegacySQLiteRequiresCiphertextValidator(t *testing.T) {
	t.Parallel()

	database := &Store{}
	if err := database.ImportLegacySQLite(context.Background(), "legacy.db"); err == nil ||
		!strings.Contains(err.Error(), "ciphertext validator") {
		t.Fatalf("unvalidated legacy import error = %v", err)
	}
}

func TestWrapLegacySQLiteImportErrorPreservesClassificationAndCause(t *testing.T) {
	t.Parallel()

	sourceErr := errors.New("legacy source unavailable")
	wrapped := wrapLegacySQLiteImportError(sourceErr)
	if !errors.Is(wrapped, ErrLegacySQLiteImport) || !errors.Is(wrapped, sourceErr) {
		t.Fatalf("wrapped legacy error does not preserve classification and cause: %v", wrapped)
	}
	if rewrapped := wrapLegacySQLiteImportError(wrapped); rewrapped != wrapped {
		t.Fatalf("already classified legacy error was wrapped again: %v", rewrapped)
	}
}

func TestLegacySQLiteSequenceKeepsDeletedHighWatermark(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE admins(id INTEGER PRIMARY KEY AUTOINCREMENT);
		INSERT INTO admins(id) VALUES(42);
		DELETE FROM admins WHERE id = 42;`); err != nil {
		t.Fatalf("create sequence fixture: %v", err)
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin fixture snapshot: %v", err)
	}
	defer tx.Rollback()
	sequence, err := legacySQLiteSequence(ctx, tx, "admins")
	if err != nil {
		t.Fatalf("read legacy sequence: %v", err)
	}
	if sequence != 42 {
		t.Fatalf("legacy sequence = %d, want 42", sequence)
	}
}

func TestValidateLegacySQLiteCiphertext(t *testing.T) {
	t.Parallel()

	validator := &legacyCipherValidatorStub{}
	account := make([]any, 13)
	account[6] = "v1.account-ciphertext"
	if err := validateLegacySQLiteCiphertext("accounts", account, validator); err != nil {
		t.Fatalf("validate account ciphertext: %v", err)
	}
	appleSession := make([]any, 8)
	appleSession[1] = "as1.apple-ciphertext"
	if err := validateLegacySQLiteCiphertext("apple_web_sessions", appleSession, validator); err != nil {
		t.Fatalf("validate Apple session ciphertext: %v", err)
	}
	if len(validator.credentialValues) != 1 || validator.credentialValues[0] != account[6] {
		t.Fatalf("validated account ciphertexts = %#v", validator.credentialValues)
	}
	if len(validator.appleValues) != 1 || validator.appleValues[0] != appleSession[1] {
		t.Fatalf("validated Apple ciphertexts = %#v", validator.appleValues)
	}

	validator.err = sql.ErrNoRows
	if err := validateLegacySQLiteCiphertext("accounts", account, validator); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("validator error = %v, want wrapped sentinel", err)
	}
	if err := validateLegacySQLiteCiphertext("aliases", []any{"not encrypted"}, validator); err != nil {
		t.Fatalf("unrelated table validation error: %v", err)
	}
}

func TestPrepareLegacySQLiteValuesNormalizesPostgresTextAfterCipherValidation(t *testing.T) {
	t.Parallel()

	validator := &legacyCipherValidatorStub{}
	account := make([]any, 13)
	account[6] = "raw\x00cipher"
	if err := prepareLegacySQLiteValues("accounts", account, validator); err != nil {
		t.Fatalf("prepare account values: %v", err)
	}
	if len(validator.credentialValues) != 1 || validator.credentialValues[0] != "raw\x00cipher" {
		t.Fatalf("ciphertext was not validated before normalization: %#v", validator.credentialValues)
	}
	if account[6] != "raw\x00cipher" {
		t.Fatalf("ciphertext changed after validation: %q", account[6])
	}

	message := make([]any, 15)
	message[3] = "message\x00id"
	message[9] = string([]byte{'s', 0xff, 'u', 'b'})
	message[10] = "text\x00body"
	message[11] = string([]byte{0xfe, 'h', 't', 'm', 'l'})
	if err := prepareLegacySQLiteValues("latest_messages", message, validator); err != nil {
		t.Fatalf("prepare latest message values: %v", err)
	}
	for index, want := range map[int]string{
		3:  "message\uFFFDid",
		9:  "s\uFFFDub",
		10: "text\uFFFDbody",
		11: "\uFFFDhtml",
	} {
		if message[index] != want {
			t.Fatalf("latest_messages value %d = %q, want %q", index, message[index], want)
		}
	}
}

func TestValidateLegacySQLiteCiphertextRejectsWrongMasterKey(t *testing.T) {
	t.Parallel()

	correct, err := secure.NewCipher(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatalf("create correct cipher: %v", err)
	}
	wrong, err := secure.NewCipher(bytes.Repeat([]byte{0x32}, 32))
	if err != nil {
		t.Fatalf("create wrong cipher: %v", err)
	}
	credential, err := correct.Encrypt("app-specific-password")
	if err != nil {
		t.Fatalf("encrypt credential: %v", err)
	}
	account := make([]any, 13)
	account[6] = credential
	if err := validateLegacySQLiteCiphertext("accounts", account, correct); err != nil {
		t.Fatalf("correct master key rejected: %v", err)
	}
	if err := validateLegacySQLiteCiphertext("accounts", account, wrong); err == nil {
		t.Fatal("wrong master key accepted legacy ciphertext")
	}
}

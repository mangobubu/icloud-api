package docker

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const (
	testInstallationID   = "0123456789abcdef0123456789abcdef"
	testDatabaseUser     = "icloud_0123456789abcdef"
	testDatabaseName     = "icloud_fedcba9876543210"
	testDatabasePassword = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestFreshBootstrapExportsInstallationIDInsteadOfPGDataBinding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PostgreSQL container entrypoint requires a POSIX shell")
	}
	t.Parallel()

	tempDir := t.TempDir()
	fakeBin := filepath.Join(tempDir, "bin")
	postgresData := filepath.Join(tempDir, "postgres-data")
	databaseConfig := filepath.Join(tempDir, "database-config")
	installationState := filepath.Join(tempDir, "installation-state")

	for _, directory := range []string{fakeBin, postgresData} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create test directory %s: %v", directory, err)
		}
	}

	writeEntrypointFakes(t, fakeBin)

	output, exported, err := runPostgresEntrypoint(
		t, tempDir, fakeBin, postgresData, databaseConfig, installationState,
	)
	if err != nil {
		t.Fatalf("fresh PostgreSQL bootstrap failed: %v\n%s", err, output)
	}

	exportedID := strings.TrimSuffix(string(exported), "\n")
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(exportedID) {
		t.Fatalf("exported ICLOUD_API_INSTALLATION_ID = %q, want 32 lowercase hex characters", exportedID)
	}
	if strings.Contains(exportedID, ":") {
		t.Fatalf("exported ICLOUD_API_INSTALLATION_ID contains PGDATA binding: %q", exportedID)
	}

	credentials, err := os.ReadFile(filepath.Join(databaseConfig, "credentials"))
	if err != nil {
		t.Fatalf("read generated database credentials: %v", err)
	}
	credentialLines := strings.Split(strings.TrimSuffix(string(credentials), "\n"), "\n")
	if len(credentialLines) != 4 {
		t.Fatalf("generated credential lines = %d, want 4", len(credentialLines))
	}
	if exportedID != credentialLines[3] {
		t.Fatalf("exported ICLOUD_API_INSTALLATION_ID = %q, credential installation ID = %q", exportedID, credentialLines[3])
	}

	expectedBinding := exportedID + ":41:42"
	for _, marker := range []string{
		filepath.Join(databaseConfig, "cluster-state", "pgdata-bootstrap-binding"),
		filepath.Join(installationState, "cluster-state", "pgdata-bootstrap-binding"),
	} {
		contents, err := os.ReadFile(marker)
		if err != nil {
			t.Fatalf("read PGDATA binding marker %s: %v", marker, err)
		}
		if value := strings.TrimSuffix(string(contents), "\n"); value != expectedBinding {
			t.Fatalf("PGDATA binding marker %s = %q, want %q", marker, value, expectedBinding)
		}
	}
	for _, marker := range []string{
		filepath.Join(databaseConfig, "cluster-state", "allow-cluster-bootstrap"),
		filepath.Join(installationState, "cluster-state", "allow-cluster-bootstrap"),
		filepath.Join(installationState, "app-state", "allow-key-bootstrap"),
	} {
		contents, err := os.ReadFile(marker)
		if err != nil {
			t.Fatalf("read installation marker %s: %v", marker, err)
		}
		if value := strings.TrimSuffix(string(contents), "\n"); value != exportedID {
			t.Fatalf("installation marker %s = %q, want raw installation ID %q", marker, value, exportedID)
		}
	}
}

func TestRepairsLegacyPGDataBindingInstallationID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PostgreSQL container entrypoint requires a POSIX shell")
	}
	t.Parallel()

	binding := testInstallationID + ":41:42"
	fixture := newLegacyRepairFixture(t, binding, testInstallationID)
	output, exported, err := fixture.run(t)
	if err != nil {
		t.Fatalf("legacy installation ID repair failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "已修复旧版 PGDATA 绑定变量污染的安装标识") {
		t.Fatalf("repair output did not report the legacy migration:\n%s", output)
	}
	fixture.requireRepaired(t, exported)
}

func TestLegacyRepairResumesAfterCommitFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PostgreSQL container entrypoint requires a POSIX shell")
	}
	t.Parallel()

	binding := testInstallationID + ":41:42"
	tests := []struct {
		name                   string
		failure                string
		wantConfigMarker       string
		wantInstallationMarker string
		wantMVCount            int
		wantSyncCount          int
		stateCommittedOnFail   bool
	}{
		{
			name:                   "second marker rename",
			failure:                "FAIL_MV_AT=2",
			wantConfigMarker:       binding,
			wantInstallationMarker: testInstallationID,
			wantMVCount:            2,
			wantSyncCount:          2,
		},
		{
			name:                   "marker group sync",
			failure:                "FAIL_SYNC_AT=3",
			wantConfigMarker:       testInstallationID,
			wantInstallationMarker: testInstallationID,
			wantMVCount:            2,
			wantSyncCount:          3,
		},
		{
			name:                   "PGDATA state rename",
			failure:                "FAIL_MV_AT=3",
			wantConfigMarker:       testInstallationID,
			wantInstallationMarker: testInstallationID,
			wantMVCount:            3,
			wantSyncCount:          4,
		},
		{
			name:                   "final state sync",
			failure:                "FAIL_SYNC_AT=5",
			wantConfigMarker:       testInstallationID,
			wantInstallationMarker: testInstallationID,
			wantMVCount:            3,
			wantSyncCount:          5,
			stateCommittedOnFail:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newLegacyRepairFixture(t, binding, binding)
			mvCountFile := filepath.Join(fixture.tempDir, "mv-count")
			syncCountFile := filepath.Join(fixture.tempDir, "sync-count")
			output, exported, err := fixture.run(t,
				test.failure,
				"MV_COUNT_FILE="+mvCountFile,
				"SYNC_COUNT_FILE="+syncCountFile,
			)
			if err == nil {
				t.Fatalf("injected repair failure unexpectedly succeeded:\n%s", output)
			}
			if len(exported) != 0 {
				t.Fatalf("official PostgreSQL entrypoint ran after injected repair failure")
			}

			stateContents, readErr := os.ReadFile(filepath.Join(
				fixture.postgresData, ".icloud-api-database-credentials",
			))
			if readErr != nil {
				t.Fatalf("read PGDATA state after injected failure: %v", readErr)
			}
			wantState := testCredentialContents(binding)
			if test.stateCommittedOnFail {
				wantState = testCredentialContents(testInstallationID)
			}
			if string(stateContents) != wantState {
				t.Fatalf("PGDATA state reached an unsafe commit point after injected failure")
			}
			requireFileContents(t,
				filepath.Join(fixture.databaseConfig, "cluster-state", "cluster-initialized"),
				test.wantConfigMarker+"\n",
			)
			requireFileContents(t,
				filepath.Join(fixture.installationState, "cluster-state", "cluster-initialized"),
				test.wantInstallationMarker+"\n",
			)
			requireCounterValue(t, mvCountFile, test.wantMVCount)
			requireCounterValue(t, syncCountFile, test.wantSyncCount)

			secondOutput, secondExported, secondErr := fixture.run(t)
			if secondErr != nil {
				t.Fatalf("repair did not resume after injected failure: %v\n%s", secondErr, secondOutput)
			}
			fixture.requireRepaired(t, secondExported)
		})
	}
}

func TestLegacyRepairRejectsNearMatchesWithoutModification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PostgreSQL container entrypoint requires a POSIX shell")
	}
	t.Parallel()

	binding := testInstallationID + ":41:42"
	tests := []struct {
		name   string
		mutate func(*testing.T, *legacyRepairFixture)
	}{
		{
			name: "PGDATA trailing data",
			mutate: func(t *testing.T, fixture *legacyRepairFixture) {
				writeTestFile(t,
					filepath.Join(fixture.postgresData, ".icloud-api-database-credentials"),
					testCredentialContents(binding)+"unknown-tail",
				)
			},
		},
		{
			name: "credential mismatch",
			mutate: func(t *testing.T, fixture *legacyRepairFixture) {
				contents := strings.Replace(
					testCredentialContents(binding), testDatabaseUser, "icloud_aaaaaaaaaaaaaaaa", 1,
				)
				writeTestFile(t,
					filepath.Join(fixture.postgresData, ".icloud-api-database-credentials"),
					contents,
				)
			},
		},
		{
			name: "wrong PGDATA identity",
			mutate: func(t *testing.T, fixture *legacyRepairFixture) {
				writeTestFile(t,
					filepath.Join(fixture.postgresData, ".icloud-api-database-credentials"),
					testCredentialContents(testInstallationID+":99:99"),
				)
			},
		},
		{
			name: "duplicate app marker",
			mutate: func(t *testing.T, fixture *legacyRepairFixture) {
				writeTestFile(t,
					filepath.Join(fixture.installationState, "app-state", "key-initialized"),
					testInstallationID+"\n",
				)
			},
		},
		{
			name: "stale bootstrap marker",
			mutate: func(t *testing.T, fixture *legacyRepairFixture) {
				writeTestFile(t,
					filepath.Join(fixture.databaseConfig, "cluster-state", "allow-cluster-bootstrap"),
					testInstallationID+"\n",
				)
			},
		},
		{
			name: "PostgreSQL PID file present",
			mutate: func(t *testing.T, fixture *legacyRepairFixture) {
				writeTestFile(t, filepath.Join(fixture.postgresData, "postmaster.pid"), "1\n")
			},
		},
		{
			name: "symlinked config credentials",
			mutate: func(t *testing.T, fixture *legacyRepairFixture) {
				credentials := filepath.Join(fixture.databaseConfig, "credentials")
				backing := filepath.Join(fixture.databaseConfig, "credentials.backing")
				if err := os.Rename(credentials, backing); err != nil {
					t.Fatalf("move credentials behind symlink: %v", err)
				}
				if err := os.Symlink(filepath.Base(backing), credentials); err != nil {
					t.Fatalf("create credentials symlink: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newLegacyRepairFixture(t, binding, binding)
			test.mutate(t, fixture)
			if err := os.Chmod(fixture.installationState, 0o755); err != nil {
				t.Fatalf("set installation state directory mode: %v", err)
			}
			writeTestFile(t, filepath.Join(fixture.installationState, "postgres-lifecycle.lock"), "")
			roots := []string{
				fixture.databaseConfig,
				fixture.installationState,
				fixture.postgresData,
			}
			before := snapshotFileTrees(t, roots...)

			output, exported, err := fixture.run(t)
			if err == nil {
				t.Fatalf("near-match repair state was accepted:\n%s", output)
			}
			if len(exported) != 0 {
				t.Fatalf("official PostgreSQL entrypoint ran after rejected repair state")
			}
			after := snapshotFileTrees(t, roots...)
			requireFileTreesEqual(t, before, after)
		})
	}
}

func TestExistingInitializedClusterStartsWithoutRepair(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PostgreSQL container entrypoint requires a POSIX shell")
	}
	t.Parallel()

	tempDir := t.TempDir()
	fakeBin := filepath.Join(tempDir, "bin")
	postgresData := filepath.Join(tempDir, "postgres-data")
	databaseConfig := filepath.Join(tempDir, "database-config")
	installationState := filepath.Join(tempDir, "installation-state")
	for _, directory := range []string{
		fakeBin,
		postgresData,
		filepath.Join(databaseConfig, "cluster-state"),
		filepath.Join(installationState, "cluster-state"),
		filepath.Join(installationState, "app-state"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create test directory %s: %v", directory, err)
		}
	}
	writeEntrypointFakes(t, fakeBin)

	credentials := testCredentialContents(testInstallationID)
	trackedFiles := map[string]string{
		filepath.Join(databaseConfig, "credentials"):                             credentials,
		filepath.Join(postgresData, ".icloud-api-database-credentials"):          credentials,
		filepath.Join(postgresData, "PG_VERSION"):                                "17\n",
		filepath.Join(databaseConfig, "cluster-state", "cluster-initialized"):    testInstallationID + "\n",
		filepath.Join(installationState, "cluster-state", "cluster-initialized"): testInstallationID + "\n",
		filepath.Join(installationState, "app-state", "key-initialized"):         testInstallationID + "\n",
	}
	for path, contents := range trackedFiles {
		writeTestFile(t, path, contents)
	}

	output, exported, err := runPostgresEntrypoint(
		t, tempDir, fakeBin, postgresData, databaseConfig, installationState,
	)
	if err != nil {
		t.Fatalf("existing PostgreSQL startup failed: %v\n%s", err, output)
	}
	if strings.Contains(output, "已修复旧版") {
		t.Fatalf("healthy existing cluster unexpectedly entered legacy repair:\n%s", output)
	}
	if string(exported) != testInstallationID+"\n" {
		t.Fatalf("exported installation ID = %q, want existing installation ID", exported)
	}
	for path, before := range trackedFiles {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read existing state %s: %v", path, err)
		}
		if string(after) != before {
			t.Fatalf("existing state changed during normal startup: %s", path)
		}
	}
}

func TestRejectsCredentialTrailingData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PostgreSQL container entrypoint requires a POSIX shell")
	}
	t.Parallel()

	tempDir := t.TempDir()
	fakeBin := filepath.Join(tempDir, "bin")
	postgresData := filepath.Join(tempDir, "postgres-data")
	databaseConfig := filepath.Join(tempDir, "database-config")
	installationState := filepath.Join(tempDir, "installation-state")
	for _, directory := range []string{fakeBin, postgresData, databaseConfig} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create test directory %s: %v", directory, err)
		}
	}
	writeEntrypointFakes(t, fakeBin)

	credentialsPath := filepath.Join(databaseConfig, "credentials")
	credentialsWithTail := testCredentialContents(testInstallationID) + "unknown-tail"
	writeTestFile(t, credentialsPath, credentialsWithTail)
	output, exported, err := runPostgresEntrypoint(
		t, tempDir, fakeBin, postgresData, databaseConfig, installationState,
	)
	if err == nil {
		t.Fatalf("credentials with non-newline-terminated trailing data were accepted")
	}
	if len(exported) != 0 {
		t.Fatalf("official PostgreSQL entrypoint ran after malformed credentials")
	}
	if !strings.Contains(output, "凭据文件包含尾部数据或格式错误") {
		t.Fatalf("unexpected trailing-data rejection output:\n%s", output)
	}
	after, readErr := os.ReadFile(credentialsPath)
	if readErr != nil {
		t.Fatalf("read rejected credentials: %v", readErr)
	}
	if string(after) != credentialsWithTail {
		t.Fatalf("rejected credentials were modified")
	}
}

type legacyRepairFixture struct {
	tempDir           string
	fakeBin           string
	postgresData      string
	databaseConfig    string
	installationState string
}

func newLegacyRepairFixture(
	t *testing.T,
	configMarkerValue string,
	installationMarkerValue string,
) *legacyRepairFixture {
	t.Helper()
	fixture := &legacyRepairFixture{tempDir: t.TempDir()}
	fixture.fakeBin = filepath.Join(fixture.tempDir, "bin")
	fixture.postgresData = filepath.Join(fixture.tempDir, "postgres-data")
	fixture.databaseConfig = filepath.Join(fixture.tempDir, "database-config")
	fixture.installationState = filepath.Join(fixture.tempDir, "installation-state")
	for _, directory := range []string{
		fixture.fakeBin,
		fixture.postgresData,
		filepath.Join(fixture.databaseConfig, "cluster-state"),
		filepath.Join(fixture.installationState, "cluster-state"),
		filepath.Join(fixture.installationState, "app-state"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create test directory %s: %v", directory, err)
		}
	}
	writeEntrypointFakes(t, fixture.fakeBin)

	binding := testInstallationID + ":41:42"
	writeTestFile(t, filepath.Join(fixture.databaseConfig, "credentials"), testCredentialContents(testInstallationID))
	writeTestFile(t, filepath.Join(fixture.postgresData, ".icloud-api-database-credentials"), testCredentialContents(binding))
	writeTestFile(t, filepath.Join(fixture.postgresData, "PG_VERSION"), "17\n")
	writeTestFile(t, filepath.Join(fixture.databaseConfig, "cluster-state", "cluster-initialized"), configMarkerValue+"\n")
	writeTestFile(t, filepath.Join(fixture.installationState, "cluster-state", "cluster-initialized"), installationMarkerValue+"\n")
	writeTestFile(t, filepath.Join(fixture.installationState, "app-state", "allow-key-bootstrap"), testInstallationID+"\n")
	return fixture
}

func (fixture *legacyRepairFixture) run(t *testing.T, extraEnvironment ...string) (string, []byte, error) {
	t.Helper()
	return runPostgresEntrypoint(
		t,
		fixture.tempDir,
		fixture.fakeBin,
		fixture.postgresData,
		fixture.databaseConfig,
		fixture.installationState,
		extraEnvironment...,
	)
}

func (fixture *legacyRepairFixture) requireRepaired(t *testing.T, exported []byte) {
	t.Helper()
	validCredentials := testCredentialContents(testInstallationID)
	for _, credentialsPath := range []string{
		filepath.Join(fixture.databaseConfig, "credentials"),
		filepath.Join(fixture.postgresData, ".icloud-api-database-credentials"),
	} {
		contents, err := os.ReadFile(credentialsPath)
		if err != nil {
			t.Fatalf("read repaired credentials %s: %v", credentialsPath, err)
		}
		if string(contents) != validCredentials {
			t.Fatalf("repaired credentials %s do not match the valid config copy", credentialsPath)
		}
	}
	for _, marker := range []string{
		filepath.Join(fixture.databaseConfig, "cluster-state", "cluster-initialized"),
		filepath.Join(fixture.installationState, "cluster-state", "cluster-initialized"),
	} {
		contents, err := os.ReadFile(marker)
		if err != nil {
			t.Fatalf("read repaired cluster marker %s: %v", marker, err)
		}
		if string(contents) != testInstallationID+"\n" {
			t.Fatalf("repaired cluster marker %s = %q, want installation ID", marker, contents)
		}
	}
	if string(exported) != testInstallationID+"\n" {
		t.Fatalf("exported installation ID = %q, want repaired installation ID", exported)
	}
}

const fakeStatCommand = `#!/bin/sh
set -eu
if [ "$1" = "-Lc" ] && [ "$2" = "%d:%i" ]; then
	[ "$3" = "$PGDATA" ] || exit 2
	printf '%s\n' '41:42'
	exit 0
fi
if [ "$1" = "-c" ] && [ "$2" = "%h" ]; then
	printf '%s\n' '1'
	exit 0
fi
if [ "$1" = "-c" ] && [ "$2" = "%u:%g:%a" ]; then
	case "$3" in
		*/database-config/credentials) printf '%s\n' '0:10001:640' ;;
		*/postgres-data/.icloud-api-database-credentials) printf '%s\n' '70:70:600' ;;
		*/cluster-state/cluster-initialized) printf '%s\n' '70:70:600' ;;
		*/app-state/*) printf '%s\n' '10001:10001:600' ;;
		*) exit 2 ;;
	esac
	exit 0
fi
exit 2
`

const fakeSyncCommand = `#!/bin/sh
set -eu
if [ -n "${SYNC_COUNT_FILE:-}" ]; then
	fake_sync_count=0
	if [ -f "$SYNC_COUNT_FILE" ]; then
		fake_sync_count="$(cat "$SYNC_COUNT_FILE")"
	fi
	fake_sync_count="$((fake_sync_count + 1))"
	printf '%s\n' "$fake_sync_count" > "$SYNC_COUNT_FILE"
	if [ -n "${FAIL_SYNC_AT:-}" ] && [ "$fake_sync_count" -eq "$FAIL_SYNC_AT" ]; then
		exit 91
	fi
fi
exit 0
`

const fakeMVCommand = `#!/bin/sh
set -eu
if [ -n "${MV_COUNT_FILE:-}" ]; then
	fake_mv_count=0
	if [ -f "$MV_COUNT_FILE" ]; then
		fake_mv_count="$(cat "$MV_COUNT_FILE")"
	fi
	fake_mv_count="$((fake_mv_count + 1))"
	printf '%s\n' "$fake_mv_count" > "$MV_COUNT_FILE"
	if [ -n "${FAIL_MV_AT:-}" ] && [ "$fake_mv_count" -eq "$FAIL_MV_AT" ]; then
		exit 92
	fi
fi
exec "$REAL_MV" "$@"
`

func writeEntrypointFakes(t *testing.T, fakeBin string) {
	t.Helper()
	writeExecutable(t, filepath.Join(fakeBin, "chown"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeBin, "flock"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeBin, "mv"), fakeMVCommand)
	writeExecutable(t, filepath.Join(fakeBin, "sync"), fakeSyncCommand)
	writeExecutable(t, filepath.Join(fakeBin, "stat"), fakeStatCommand)
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write test file %s: %v", path, err)
	}
}

func testCredentialContents(installationID string) string {
	return strings.Join([]string{
		testDatabaseUser,
		testDatabaseName,
		testDatabasePassword,
		installationID,
		"",
	}, "\n")
}

func requireFileContents(t *testing.T, path, expected string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(contents) != expected {
		t.Fatalf("file %s has unexpected contents", path)
	}
}

func requireCounterValue(t *testing.T, path string, expected int) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read command counter %s: %v", path, err)
	}
	if value := strings.TrimSpace(string(contents)); value != strconv.Itoa(expected) {
		t.Fatalf("command counter %s = %q, want %d", path, value, expected)
	}
}

type fileTreeSnapshotEntry struct {
	mode       os.FileMode
	contents   string
	linkTarget string
}

func snapshotFileTrees(t *testing.T, roots ...string) map[string]fileTreeSnapshotEntry {
	t.Helper()
	snapshot := make(map[string]fileTreeSnapshotEntry)
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, _ os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			entry := fileTreeSnapshotEntry{mode: info.Mode()}
			switch {
			case info.Mode().IsRegular():
				contents, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				entry.contents = string(contents)
			case info.Mode()&os.ModeSymlink != 0:
				linkTarget, err := os.Readlink(path)
				if err != nil {
					return err
				}
				entry.linkTarget = linkTarget
			}
			snapshot[path] = entry
			return nil
		})
		if err != nil {
			t.Fatalf("snapshot file tree %s: %v", root, err)
		}
	}
	return snapshot
}

func requireFileTreesEqual(
	t *testing.T,
	expected map[string]fileTreeSnapshotEntry,
	actual map[string]fileTreeSnapshotEntry,
) {
	t.Helper()
	for path, expectedEntry := range expected {
		actualEntry, found := actual[path]
		if !found {
			t.Fatalf("rejected repair removed persistent path: %s", path)
		}
		if actualEntry.mode != expectedEntry.mode {
			t.Fatalf(
				"rejected repair changed mode or type for %s: got %s, want %s",
				path,
				actualEntry.mode,
				expectedEntry.mode,
			)
		}
		if actualEntry.contents != expectedEntry.contents {
			t.Fatalf("rejected repair changed persistent file contents: %s", path)
		}
		if actualEntry.linkTarget != expectedEntry.linkTarget {
			t.Fatalf("rejected repair changed symlink target: %s", path)
		}
	}
	for path := range actual {
		if _, found := expected[path]; !found {
			t.Fatalf("rejected repair created persistent path: %s", path)
		}
	}
}

func entrypointTestShell() string {
	if dash, err := exec.LookPath("dash"); err == nil {
		return dash
	}
	return "/bin/sh"
}

func runPostgresEntrypoint(
	t *testing.T,
	tempDir string,
	fakeBin string,
	postgresData string,
	databaseConfig string,
	installationState string,
	extraEnvironment ...string,
) (string, []byte, error) {
	t.Helper()
	exportedInstallationID := filepath.Join(tempDir, "exported-installation-id")
	if err := os.Remove(exportedInstallationID); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove stale exported installation ID: %v", err)
	}
	officialEntrypoint := filepath.Join(tempDir, "postgres-official-entrypoint")
	writeExecutable(t, officialEntrypoint, `#!/bin/sh
set -eu
printf '%s\n' "$ICLOUD_API_INSTALLATION_ID" > "$CAPTURE_INSTALLATION_ID"
`)

	scriptPath, err := filepath.Abs("postgres-entrypoint.sh")
	if err != nil {
		t.Fatalf("resolve postgres entrypoint path: %v", err)
	}
	realMV, err := exec.LookPath("mv")
	if err != nil {
		t.Fatalf("find real mv command: %v", err)
	}
	excludedEnvironment := []string{
		"CAPTURE_INSTALLATION_ID",
		"FAIL_MV_AT",
		"FAIL_SYNC_AT",
		"ICLOUD_API_DATABASE_CONFIG_DIR",
		"ICLOUD_API_INSTALLATION_STATE_DIR",
		"MV_COUNT_FILE",
		"PATH",
		"PGDATA",
		"POSTGRES_OFFICIAL_ENTRYPOINT",
		"REAL_MV",
		"SYNC_COUNT_FILE",
	}
	for _, variable := range extraEnvironment {
		name, _, found := strings.Cut(variable, "=")
		if !found {
			t.Fatalf("extra environment variable has no value: %q", variable)
		}
		excludedEnvironment = append(excludedEnvironment, name)
	}
	command := exec.Command(entrypointTestShell(), scriptPath, "postgres")
	command.Env = append(filteredEnvironment(excludedEnvironment...),
		"CAPTURE_INSTALLATION_ID="+exportedInstallationID,
		"ICLOUD_API_DATABASE_CONFIG_DIR="+databaseConfig,
		"ICLOUD_API_INSTALLATION_STATE_DIR="+installationState,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PGDATA="+postgresData,
		"POSTGRES_OFFICIAL_ENTRYPOINT="+officialEntrypoint,
		"REAL_MV="+realMV,
	)
	command.Env = append(command.Env, extraEnvironment...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	runErr := command.Run()
	exported, readErr := os.ReadFile(exportedInstallationID)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read exported installation ID: %v", readErr)
	}
	return output.String(), exported, runErr
}

func filteredEnvironment(names ...string) []string {
	excluded := make(map[string]struct{}, len(names))
	for _, name := range names {
		excluded[name] = struct{}{}
	}

	filtered := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, found := excluded[name]; !found {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

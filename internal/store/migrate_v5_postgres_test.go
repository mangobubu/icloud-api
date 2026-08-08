package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostgresV6SeenTablesMigrationStructure(t *testing.T) {
	t.Parallel()

	wanted := []string{
		`CREATE TABLE consumed_messages (
			alias_id BIGINT NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
			uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
			uid BIGINT NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
			consumed_at BIGINT NOT NULL,
			PRIMARY KEY(alias_id, uid_validity, uid)
		)`,
		`CREATE TABLE imap_seen_tasks (
			account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
			uid BIGINT NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
			created_at BIGINT NOT NULL,
			PRIMARY KEY(account_id, uid_validity, uid)
		)`,
		`CREATE INDEX imap_seen_tasks_account_created_idx
			ON imap_seen_tasks(account_id, created_at, uid_validity, uid)`,
	}
	paths := []struct {
		name       string
		statements []string
	}{
		{name: "fresh v6", statements: postgresSchemaV6},
		{name: "v5 to v6", statements: postgresMigrateV5ToV6},
	}
	for _, path := range paths {
		path := path
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			for _, statement := range wanted {
				if !containsNormalizedSQL(path.statements, statement) {
					t.Errorf("%s PostgreSQL migration is missing %q", path.name, normalizeSQL(statement))
				}
			}
		})
	}

	const convergenceIndex = `CREATE INDEX IF NOT EXISTS imap_seen_tasks_account_created_idx
		ON imap_seen_tasks(account_id, created_at, uid_validity, uid)`
	if !containsNormalizedSQL(postgresSchemaConvergence, convergenceIndex) {
		t.Errorf("PostgreSQL v6 convergence is missing %q", normalizeSQL(convergenceIndex))
	}
}

func TestPostgresRestoreSupportsV4V5AndStrictlyValidatesV6(t *testing.T) {
	t.Parallel()

	scriptPath := filepath.Join("..", "..", "docker", "postgres-entrypoint.sh")
	contents, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read PostgreSQL restore entrypoint: %v", err)
	}
	script := string(contents)

	wanted := []string{
		`case "$auto_alias_table_count" in`,
		`die "恢复归档中的自动别名 schema 表不完整"`,
		`case "$seen_table_count" in`,
		`die "恢复归档中的消费与 IMAP Seen schema 表不完整"`,
		`optional_required_tables="alias_creation_schedules pending_alias_api_keys"`,
		`optional_required_tables="$optional_required_tables consumed_messages imap_seen_tasks"`,
		`$optional_required_tables; do`,
		`restored_schema_version NOT IN (4, 5, 6)`,
		`restored_schema_version = 4`,
		`restored_schema_version = 5`,
		`restored_schema_version = 6`,
		`ARRAY['account_id']::TEXT[]`,
		`ARRAY['alias_id']::TEXT[]`,
		`ARRAY['alias_id', 'uid_validity', 'uid']::TEXT[]`,
		`ARRAY['account_id', 'uid_validity', 'uid']::TEXT[]`,
		`constraint_state.confrelid = pg_catalog.to_regclass('public.' || required.referenced_table)`,
		`constraint_state.confdeltype = 'c'`,
		`access_method.amname = 'btree'`,
		`NOT index_metadata.indisunique`,
		`index_metadata.indpred IS NULL`,
		`index_metadata.indexprs IS NULL`,
		`ARRAY['enabled', 'next_run_at', 'account_id']::TEXT[]`,
		`ARRAY['account_id', 'created_at', 'uid_validity', 'uid']::TEXT[]`,
	}
	for _, fragment := range wanted {
		if !strings.Contains(script, fragment) {
			t.Errorf("PostgreSQL restore contract is missing %q", fragment)
		}
	}
	if strings.Contains(script, "data_migrations alias_creation_schedules") ||
		strings.Contains(script, "data_migrations consumed_messages") {
		t.Fatal("PostgreSQL restore preflight still requires extension tables for every archive")
	}
}

func TestPostgresMigrationExecutesV0V3V4V5V6Paths(t *testing.T) {
	t.Parallel()

	freshAdmin := postgresSchemaV6[0]
	syncState := postgresMigrateV3ToV4[len(postgresMigrateV3ToV4)-2]
	autoSchedule := postgresMigrateV4ToV5[0]
	consumedMessages := postgresMigrateV5ToV6[0]
	compatValidation := postgresV5CompatibilityMigration[0]
	compatAutoSchedule := postgresV5CompatibilityMigration[1]
	compatConsumedMessages := postgresV5CompatibilityMigration[3]
	seenIndexConvergence := postgresSchemaConvergence[len(postgresSchemaConvergence)-1]

	paths := []struct {
		version           int
		wanted            []string
		unwanted          []string
		wantVersionUpdate bool
	}{
		{0, []string{freshAdmin, syncState, autoSchedule, consumedMessages}, nil, true},
		{3, []string{syncState, autoSchedule, consumedMessages}, []string{freshAdmin}, true},
		{4, []string{autoSchedule, consumedMessages}, []string{freshAdmin, syncState}, true},
		{5, []string{compatValidation, compatAutoSchedule, compatConsumedMessages}, []string{freshAdmin, syncState, autoSchedule, consumedMessages}, true},
		{6, nil, []string{freshAdmin, syncState, autoSchedule, consumedMessages, compatValidation, compatAutoSchedule, compatConsumedMessages}, false},
	}
	for _, path := range paths {
		path := path
		t.Run(fmt.Sprintf("v%d", path.version), func(t *testing.T) {
			capture := &postgresMigrationCaptureDriver{version: path.version}
			driverName := fmt.Sprintf("icloud-api-postgres-v6-path-%p", capture)
			sql.Register(driverName, capture)
			raw, err := sql.Open(driverName, "")
			if err != nil {
				t.Fatalf("open PostgreSQL migration capture: %v", err)
			}
			raw.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = raw.Close() })

			db := newStore(raw, dialectPostgres)
			if err := db.migratePostgres(context.Background()); err != nil {
				t.Fatalf("migrate PostgreSQL schema v%d: %v", path.version, err)
			}
			for _, statement := range path.wanted {
				if !containsNormalizedSQL(capture.statements, statement) {
					t.Errorf("PostgreSQL v%d path did not execute %q", path.version, normalizeSQL(statement))
				}
			}
			for _, statement := range path.unwanted {
				if containsNormalizedSQL(capture.statements, statement) {
					t.Errorf("PostgreSQL v%d path unexpectedly executed %q", path.version, normalizeSQL(statement))
				}
			}
			if !containsNormalizedSQL(capture.statements, seenIndexConvergence) {
				t.Errorf("PostgreSQL v%d path did not converge the seen queue index", path.version)
			}

			updatedVersion := false
			for _, statement := range capture.statements {
				if strings.HasPrefix(normalizeSQL(statement), "update schema_migrations set version =") {
					updatedVersion = true
					break
				}
			}
			if updatedVersion != path.wantVersionUpdate {
				t.Errorf("PostgreSQL v%d version update = %t, want %t", path.version, updatedVersion, path.wantVersionUpdate)
			}
			if path.version == 3 &&
				(postgresMigrationStatementIndex(capture.statements, syncState) >=
					postgresMigrationStatementIndex(capture.statements, autoSchedule) ||
					postgresMigrationStatementIndex(capture.statements, autoSchedule) >=
						postgresMigrationStatementIndex(capture.statements, consumedMessages)) {
				t.Error("PostgreSQL v3 path did not apply v3-to-v4, v4-to-v5, and v5-to-v6 in order")
			}
		})
	}
}

type postgresMigrationCaptureDriver struct {
	version    int
	statements []string
}

func (d *postgresMigrationCaptureDriver) Open(string) (driver.Conn, error) {
	return &postgresMigrationCaptureConn{capture: d}, nil
}

type postgresMigrationCaptureConn struct {
	capture *postgresMigrationCaptureDriver
}

func (c *postgresMigrationCaptureConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepare is not implemented")
}

func (c *postgresMigrationCaptureConn) Close() error { return nil }

func (c *postgresMigrationCaptureConn) Begin() (driver.Tx, error) {
	return &postgresMigrationCaptureTx{}, nil
}

func (c *postgresMigrationCaptureConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return &postgresMigrationCaptureTx{}, nil
}

func (c *postgresMigrationCaptureConn) ExecContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Result, error) {
	c.capture.statements = append(c.capture.statements, query)
	return driver.RowsAffected(1), nil
}

func (c *postgresMigrationCaptureConn) QueryContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	if !strings.Contains(normalizeSQL(query), "select version from schema_migrations") {
		return nil, fmt.Errorf("unexpected PostgreSQL migration query: %s", query)
	}
	return &postgresMigrationVersionRows{version: int64(c.capture.version)}, nil
}

type postgresMigrationCaptureTx struct{}

func (*postgresMigrationCaptureTx) Commit() error   { return nil }
func (*postgresMigrationCaptureTx) Rollback() error { return nil }

type postgresMigrationVersionRows struct {
	version int64
	read    bool
}

func (*postgresMigrationVersionRows) Columns() []string { return []string{"version"} }
func (*postgresMigrationVersionRows) Close() error      { return nil }

func (r *postgresMigrationVersionRows) Next(values []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	values[0] = r.version
	return nil
}

func postgresMigrationStatementIndex(statements []string, wanted string) int {
	wanted = normalizeSQL(wanted)
	for index, statement := range statements {
		if normalizeSQL(statement) == wanted {
			return index
		}
	}
	return -1
}

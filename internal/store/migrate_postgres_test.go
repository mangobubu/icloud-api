package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errStopPostgresMigration = errors.New("stop postgres migration after begin")

func TestPostgresAdminSessionAdminIDIndexMigrationStructure(t *testing.T) {
	t.Parallel()

	const freshIndex = "create index admin_sessions_admin_id_idx on admin_sessions(admin_id)"
	if !containsNormalizedSQL(postgresSchemaV4, freshIndex) {
		t.Fatalf("fresh PostgreSQL schema is missing %q", freshIndex)
	}

	const convergenceIndex = "create index if not exists admin_sessions_admin_id_idx on admin_sessions(admin_id)"
	if !containsNormalizedSQL(postgresSchemaConvergence, convergenceIndex) {
		t.Fatalf("PostgreSQL v4 convergence is missing %q", convergenceIndex)
	}
}

func TestPostgresQueryIndexesMigrationStructure(t *testing.T) {
	t.Parallel()

	indexes := []struct {
		name string
		sql  string
	}{
		{"accounts", "accounts_enabled_email_idx on accounts(email, id) where enabled"},
		{"alias account listing", "aliases_account_address_idx on aliases(account_id, address, id)"},
		{"aliases", "aliases_enabled_account_address_idx on aliases(account_id, address, id) where enabled"},
	}
	paths := []struct {
		name        string
		statements  []string
		ifNotExists bool
	}{
		{"fresh v4", postgresSchemaV4, false},
		{"v3 to v4", postgresMigrateV3ToV4, true},
		{"v4 convergence", postgresSchemaConvergence, true},
	}

	for _, path := range paths {
		path := path
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			prefix := "create index "
			if path.ifNotExists {
				prefix += "if not exists "
			}
			for _, index := range indexes {
				if !containsNormalizedSQL(path.statements, prefix+index.sql) {
					t.Errorf("%s PostgreSQL schema is missing %s index", path.name, index.name)
				}
			}
		})
	}
}

func TestPostgresMigrationUsesReadCommittedIsolation(t *testing.T) {
	t.Parallel()

	capture := &beginOptionsDriver{options: make(chan driver.TxOptions, 1)}
	driverName := fmt.Sprintf("icloud-api-postgres-migration-isolation-test-%p", capture)
	sql.Register(driverName, capture)
	raw, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open migration isolation fixture: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	db := newStore(raw, dialectPostgres)
	err = db.migratePostgres(context.Background())
	if !errors.Is(err, errStopPostgresMigration) {
		t.Fatalf("postgres migration error = %v, want test stop error", err)
	}

	options := <-capture.options
	if isolation := sql.IsolationLevel(options.Isolation); isolation != sql.LevelReadCommitted {
		t.Fatalf("postgres migration isolation = %v, want %v", isolation, sql.LevelReadCommitted)
	}
	if options.ReadOnly {
		t.Fatal("postgres migration unexpectedly began a read-only transaction")
	}
}

type beginOptionsDriver struct {
	options chan driver.TxOptions
}

func (d *beginOptionsDriver) Open(string) (driver.Conn, error) {
	return &beginOptionsConn{options: d.options}, nil
}

type beginOptionsConn struct {
	options chan driver.TxOptions
}

func (c *beginOptionsConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not implemented")
}

func (c *beginOptionsConn) Close() error { return nil }

func (c *beginOptionsConn) Begin() (driver.Tx, error) {
	return nil, errors.New("legacy begin is not implemented")
}

func (c *beginOptionsConn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.options <- options
	return nil, errStopPostgresMigration
}

func containsNormalizedSQL(statements []string, wanted string) bool {
	wanted = normalizeSQL(wanted)
	for _, statement := range statements {
		if normalizeSQL(statement) == wanted {
			return true
		}
	}
	return false
}

func normalizeSQL(statement string) string {
	return strings.Join(strings.Fields(strings.ToLower(statement)), " ")
}

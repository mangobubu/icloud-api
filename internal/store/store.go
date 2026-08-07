package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

var (
	// ErrNotFound is returned when a requested row does not exist.
	ErrNotFound               = sql.ErrNoRows
	ErrAliasLimit             = errors.New("enabled alias limit reached")
	ErrAliasOwnershipConflict = errors.New("alias address belongs to another account")
	ErrAccountIdentityLocked  = errors.New("account identity is locked by aliases")
	ErrCredentialsChanged     = errors.New("administrator credentials changed")
	memoryID                  atomic.Uint64
)

// Store owns the application's SQLite persistence layer.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens a SQLite database, configures it, and applies all migrations.
func Open(path string) (*Store, error) {
	return OpenContext(context.Background(), path)
}

// OpenContext is Open with a caller-provided context for setup and migration.
func OpenContext(ctx context.Context, path string) (*Store, error) {
	dsn, err := sqliteDSN(path)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)

	s := New(db)
	if err := s.configure(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// New wraps an existing SQLite database. Call Migrate before using it.
// The caller remains responsible for ensuring every pooled connection enables
// SQLite foreign keys (Open does this through its DSN).
func New(db *sql.DB) *Store {
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// DB exposes the underlying handle for health checks and transaction-aware
// integration code. Business queries should use Store methods.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure sqlite (%s): %w", statement, err)
		}
	}
	return nil
}

func sqliteDSN(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("sqlite path is empty")
	}

	var dsn string
	if path == ":memory:" {
		dsn = fmt.Sprintf("file:icloud_api_%d?mode=memory&cache=shared", memoryID.Add(1))
	} else if strings.HasPrefix(path, "file:") {
		dsn = path
	} else {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve sqlite path: %w", err)
		}
		normalized := filepath.ToSlash(absolute)
		if !strings.HasPrefix(normalized, "/") {
			normalized = "/" + normalized
		}
		dsn = (&url.URL{Scheme: "file", Path: normalized}).String()
	}

	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + strings.Join([]string{
		"_pragma=foreign_keys(1)",
		"_pragma=busy_timeout(5000)",
		"_pragma=journal_mode(WAL)",
	}, "&"), nil
}

func timestamp(t time.Time) int64 { return t.UTC().UnixNano() }

func timeFromTimestamp(value int64) time.Time {
	return time.Unix(0, value).UTC()
}

func nullableTimestamp(value *time.Time) any {
	if value == nil {
		return nil
	}
	return timestamp(*value)
}

func timePtr(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	t := timeFromTimestamp(value.Int64)
	return &t
}

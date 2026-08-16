package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"icloud-api/internal/domain"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

var (
	// ErrNotFound is returned when a requested row does not exist.
	ErrNotFound                 = sql.ErrNoRows
	ErrInvalidPostgresURL       = errors.New("PostgreSQL URL must use postgres:// or postgresql://")
	ErrAliasLimit               = errors.New("enabled alias limit reached")
	ErrAliasConfirmationPending = errors.New("alias is awaiting Apple confirmation")
	ErrAliasCredentialMode      = errors.New("alias credential mode does not support this operation")
	ErrAliasOwnershipConflict   = errors.New("alias address belongs to another account")
	ErrAliasIdentityConflict    = errors.New("alias address conflicts with account identity")
	ErrAliasSuffixMismatch      = errors.New("alias address does not match custom mailbox suffix")
	ErrAccountIdentityLocked    = errors.New("account identity is locked by aliases")
	ErrAccountDisabled          = errors.New("primary account is disabled")
	ErrCustomMailboxRequired    = errors.New("custom mailbox is required")
	ErrICloudMailboxRequired    = errors.New("iCloud mailbox is required")
	ErrCredentialsChanged       = errors.New("administrator credentials changed")
	memoryID                    atomic.Uint64
)

type dialect uint8

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

const addressNamespaceAdvisoryLock = int64(0x49434c4f55444144)

// Store owns the application's persistence layer.
type Store struct {
	db                              *sql.DB
	dialect                         dialect
	now                             func() time.Time
	credentialFactory               func(aliasID, version int64) (domain.AliasCredentialMaterial, error)
	credentialReuseFactory          func(aliasID, version int64, pendingCiphertext string) (domain.AliasCredentialMaterial, error)
	credentialRevealFactory         func(aliasID int64, credentialCiphertext string) (string, error)
	credentialAPIKeyRotationFactory func(
		aliasID, version int64,
		credentialCiphertext, apiKey string,
	) (domain.AliasCredentialMaterial, error)
	mailArchiveDir          string
	mailArchiveLimit        int64
	mailArchiveAccountLocks sync.Map
}

// Open opens a PostgreSQL connection URL or a legacy SQLite database and
// applies all migrations.
func Open(dataSource string) (*Store, error) {
	return OpenContext(context.Background(), dataSource)
}

// OpenContext is Open with a caller-provided context for setup and migration.
func OpenContext(ctx context.Context, dataSource string) (*Store, error) {
	dataSource = strings.TrimSpace(dataSource)
	if hasPostgresScheme(dataSource) {
		postgresURL, err := normalizePostgresURL(dataSource)
		if err != nil {
			return nil, err
		}
		return openPostgres(ctx, postgresURL)
	}
	return openSQLite(ctx, dataSource)
}

func openPostgres(ctx context.Context, dataSource string) (*Store, error) {
	db, err := sql.Open("pgx", dataSource)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	s := newStore(db, dialectPostgres)
	if err := s.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func openSQLite(ctx context.Context, path string) (*Store, error) {
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

	s := newStore(db, dialectSQLite)
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
	return newStore(db, dialectSQLite)
}

func newStore(db *sql.DB, databaseDialect dialect) *Store {
	return &Store{
		db:      db,
		dialect: databaseDialect,
		now:     func() time.Time { return time.Now().UTC() },
	}
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
	if s.dialect != dialectSQLite {
		return nil
	}
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

// ValidatePostgresURL verifies that dataSource uses the URL form supported by
// OpenContext. It does not connect to the database.
func ValidatePostgresURL(dataSource string) error {
	_, err := normalizePostgresURL(dataSource)
	return err
}

func hasPostgresScheme(dataSource string) bool {
	dataSource = strings.TrimSpace(dataSource)
	separator := strings.IndexByte(dataSource, ':')
	if separator <= 0 {
		return false
	}
	scheme := dataSource[:separator]
	return strings.EqualFold(scheme, "postgres") || strings.EqualFold(scheme, "postgresql")
}

func normalizePostgresURL(dataSource string) (string, error) {
	dataSource = strings.TrimSpace(dataSource)
	separator := strings.IndexByte(dataSource, ':')
	if separator <= 0 {
		return "", ErrInvalidPostgresURL
	}

	scheme := strings.ToLower(dataSource[:separator])
	if scheme != "postgres" && scheme != "postgresql" {
		return "", ErrInvalidPostgresURL
	}
	if len(dataSource) < separator+3 || dataSource[separator+1:separator+3] != "//" {
		return "", ErrInvalidPostgresURL
	}

	parsed, err := url.Parse(dataSource)
	if err != nil || parsed.Opaque != "" || !strings.EqualFold(parsed.Scheme, scheme) {
		return "", ErrInvalidPostgresURL
	}
	return scheme + dataSource[separator:], nil
}

func (s *Store) execContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.rebind(query), args...)
}

func (s *Store) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.rebind(query), args...)
}

func (s *Store) queryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.rebind(query), args...)
}

func (s *Store) txExecContext(ctx context.Context, tx *sql.Tx, query string, args ...any) (sql.Result, error) {
	return tx.ExecContext(ctx, s.rebind(query), args...)
}

func (s *Store) txQueryContext(ctx context.Context, tx *sql.Tx, query string, args ...any) (*sql.Rows, error) {
	return tx.QueryContext(ctx, s.rebind(query), args...)
}

func (s *Store) txQueryRowContext(ctx context.Context, tx *sql.Tx, query string, args ...any) *sql.Row {
	return tx.QueryRowContext(ctx, s.rebind(query), args...)
}

func (s *Store) beginAddressNamespaceTx(ctx context.Context) (*sql.Tx, error) {
	options := &sql.TxOptions{}
	if s.dialect == dialectPostgres {
		// The conflict query must receive a fresh snapshot after waiting for a
		// preceding namespace owner to commit.
		options.Isolation = sql.LevelReadCommitted
	}
	tx, err := s.db.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	if err := s.lockAddressNamespaceTx(ctx, tx); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("lock address namespace: %w", err)
	}
	return tx, nil
}

// lockAddressNamespaceTx serializes the cross-table address namespace shared
// by accounts.email and custom-generated aliases.address. Callers must acquire
// this lock before any account row lock so account updates and custom alias
// generation cannot deadlock while enforcing the shared identity invariant.
func (s *Store) lockAddressNamespaceTx(ctx context.Context, tx *sql.Tx) error {
	if s.dialect == dialectPostgres {
		_, err := s.txExecContext(ctx, tx,
			`SELECT pg_advisory_xact_lock(?)`, addressNamespaceAdvisoryLock,
		)
		return err
	}
	// SQLite has one database writer at a time. A no-op UPDATE starts the write
	// transaction without changing an account row, providing the same ordering
	// before callers inspect either side of the address namespace.
	_, err := s.txExecContext(ctx, tx, `UPDATE accounts SET updated_at = updated_at WHERE 0`)
	return err
}

// lockAccountForUpdate serializes account-scoped writes across application
// processes for the lifetime of the transaction.
func (s *Store) lockAccountForUpdate(ctx context.Context, tx *sql.Tx, accountID int64) error {
	_, err := s.lockAccountVersionForUpdate(ctx, tx, accountID)
	return err
}

func (s *Store) lockAccountVersionForUpdate(ctx context.Context, tx *sql.Tx, accountID int64) (int64, error) {
	if s.dialect == dialectPostgres {
		var version int64
		err := s.txQueryRowContext(ctx, tx,
			`SELECT updated_at FROM accounts WHERE id = ? FOR UPDATE`, accountID,
		).Scan(&version)
		if err == sql.ErrNoRows {
			return 0, ErrNotFound
		}
		return version, err
	}

	// SQLite has no row-level SELECT FOR UPDATE. Taking its write lock before
	// reading the capacity or cursor gives the equivalent transaction ordering.
	result, err := s.txExecContext(ctx, tx,
		`UPDATE accounts SET updated_at = updated_at WHERE id = ?`, accountID,
	)
	if err != nil {
		return 0, err
	}
	if err := requireAffected(result, "account"); err != nil {
		return 0, err
	}
	var version int64
	if err := s.txQueryRowContext(ctx, tx,
		`SELECT updated_at FROM accounts WHERE id = ?`, accountID,
	).Scan(&version); err != nil {
		if err == sql.ErrNoRows {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return version, nil
}

func (s *Store) nextAccountVersion(current int64) (int64, error) {
	candidate := timestamp(s.now())
	if candidate > current {
		return candidate, nil
	}
	if current == int64(^uint64(0)>>1) {
		return 0, errors.New("account version is exhausted")
	}
	return current + 1, nil
}

func (s *Store) bumpAccountVersionTx(ctx context.Context, tx *sql.Tx, accountID, current int64) (int64, error) {
	next, err := s.nextAccountVersion(current)
	if err != nil {
		return 0, err
	}
	result, err := s.txExecContext(ctx, tx, `
		UPDATE accounts SET updated_at = ?
		WHERE id = ? AND updated_at = ?`, next, accountID, current)
	if err != nil {
		return 0, err
	}
	if err := requireAffected(result, "account"); err != nil {
		return 0, err
	}
	return next, nil
}

func (s *Store) rebind(query string) string {
	if s.dialect != dialectPostgres || !strings.Contains(query, "?") {
		return query
	}
	return rebindPostgres(query)
}

// rebindPostgres replaces placeholders while leaving quoted text, identifiers,
// dollar-quoted bodies, and comments untouched.
func rebindPostgres(query string) string {
	var result strings.Builder
	result.Grow(len(query) + 8)
	placeholder := 1

	for index := 0; index < len(query); {
		switch query[index] {
		case '\'':
			start := index
			index++
			for index < len(query) {
				if query[index] == '\\' && index+1 < len(query) {
					index += 2
					continue
				}
				if query[index] == '\'' {
					index++
					if index < len(query) && query[index] == '\'' {
						index++
						continue
					}
					break
				}
				index++
			}
			result.WriteString(query[start:index])
		case '"':
			start := index
			index++
			for index < len(query) {
				if query[index] == '"' {
					index++
					if index < len(query) && query[index] == '"' {
						index++
						continue
					}
					break
				}
				index++
			}
			result.WriteString(query[start:index])
		case '-':
			if index+1 < len(query) && query[index+1] == '-' {
				end := strings.IndexByte(query[index+2:], '\n')
				if end < 0 {
					result.WriteString(query[index:])
					return result.String()
				}
				end += index + 3
				result.WriteString(query[index:end])
				index = end
				continue
			}
			result.WriteByte(query[index])
			index++
		case '/':
			if index+1 < len(query) && query[index+1] == '*' {
				end := strings.Index(query[index+2:], "*/")
				if end < 0 {
					result.WriteString(query[index:])
					return result.String()
				}
				end += index + 4
				result.WriteString(query[index:end])
				index = end
				continue
			}
			result.WriteByte(query[index])
			index++
		case '$':
			delimiterEnd := postgresDollarDelimiterEnd(query, index)
			if delimiterEnd < 0 {
				result.WriteByte(query[index])
				index++
				continue
			}
			delimiter := query[index:delimiterEnd]
			closingOffset := strings.Index(query[delimiterEnd:], delimiter)
			if closingOffset < 0 {
				result.WriteString(query[index:])
				return result.String()
			}
			end := delimiterEnd + closingOffset + len(delimiter)
			result.WriteString(query[index:end])
			index = end
		case '?':
			fmt.Fprintf(&result, "$%d", placeholder)
			placeholder++
			index++
		default:
			result.WriteByte(query[index])
			index++
		}
	}
	return result.String()
}

func postgresDollarDelimiterEnd(query string, start int) int {
	if start+1 >= len(query) {
		return -1
	}
	if query[start+1] == '$' {
		return start + 2
	}
	first := query[start+1]
	if first != '_' && (first < 'A' || first > 'Z') && (first < 'a' || first > 'z') {
		return -1
	}
	for index := start + 2; index < len(query); index++ {
		character := query[index]
		if character == '$' {
			return index + 1
		}
		if character != '_' && (character < 'A' || character > 'Z') &&
			(character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return -1
		}
	}
	return -1
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

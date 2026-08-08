package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	legacySQLiteMigrationName = "legacy_sqlite_import_v1"
)

type legacyCopySpec struct {
	table          string
	selectSQL      string
	insertSQL      string
	columnCount    int
	booleanColumns map[int]string
	resetSequence  bool
}

// LegacySQLiteCipherValidator is implemented by secure.Cipher. Imported
// ciphertext is authenticated before a fresh PostgreSQL database is bound to
// the configured master key.
type LegacySQLiteCipherValidator interface {
	Decrypt(string) (string, error)
	DecryptAppleSession(string) (string, error)
}

// ImportLegacySQLite is retained for source compatibility. Legacy ciphertext
// must be authenticated, so callers must use ImportLegacySQLiteWithValidator.
func (s *Store) ImportLegacySQLite(ctx context.Context, legacyPath string) error {
	if strings.TrimSpace(legacyPath) == "" {
		return nil
	}
	return errors.New("legacy SQLite import requires a ciphertext validator")
}

// ImportLegacySQLiteWithValidator is ImportLegacySQLite with authentication of
// every encrypted IMAP credential and Apple session before it is persisted.
func (s *Store) ImportLegacySQLiteWithValidator(
	ctx context.Context,
	legacyPath string,
	validator LegacySQLiteCipherValidator,
) error {
	legacyPath = strings.TrimSpace(legacyPath)
	if legacyPath == "" {
		return nil
	}
	if s.dialect != dialectPostgres {
		return errors.New("legacy SQLite import requires a PostgreSQL store")
	}
	if validator == nil {
		return errors.New("legacy SQLite import requires a ciphertext validator")
	}

	tx, err := s.beginMasterKeyLifecycleTx(ctx)
	if err != nil {
		return fmt.Errorf("begin legacy SQLite import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := s.importLegacySQLiteWithValidatorTx(ctx, tx, legacyPath, validator, false); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy SQLite import: %w", err)
	}
	return nil
}

func (s *Store) importLegacySQLiteWithValidatorTx(
	ctx context.Context,
	tx *sql.Tx,
	legacyPath string,
	validator LegacySQLiteCipherValidator,
	allowBound bool,
) (bool, error) {
	legacyPath = strings.TrimSpace(legacyPath)
	if legacyPath == "" {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS data_migrations (
			name TEXT PRIMARY KEY,
			applied_at BIGINT NOT NULL
		)`); err != nil {
		return false, fmt.Errorf("create data migration registry: %w", err)
	}

	var alreadyApplied bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM data_migrations WHERE name = $1)`,
		legacySQLiteMigrationName,
	).Scan(&alreadyApplied); err != nil {
		return false, fmt.Errorf("check legacy SQLite import marker: %w", err)
	}
	if alreadyApplied {
		return false, nil
	}

	dsn, sourcePath, err := legacySQLiteReadOnlyDSN(legacyPath)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(sourcePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect legacy SQLite database: %w", err)
	}
	if info.IsDir() {
		return false, fmt.Errorf("legacy SQLite path is a directory: %s", sourcePath)
	}
	if !allowBound {
		var fingerprintExists bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM app_metadata WHERE name = $1)
		`, masterKeyFingerprintName).Scan(&fingerprintExists); err != nil {
			return false, fmt.Errorf("check master key binding before legacy SQLite import: %w", err)
		}
		if fingerprintExists {
			return false, errors.New("pending legacy SQLite import must be combined with master key initialization")
		}
	}

	var targetHasData bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM admins
			UNION ALL SELECT 1 FROM admin_sessions
			UNION ALL SELECT 1 FROM accounts
			UNION ALL SELECT 1 FROM aliases
			UNION ALL SELECT 1 FROM latest_messages
			UNION ALL SELECT 1 FROM audit_logs
			UNION ALL SELECT 1 FROM apple_web_sessions
			LIMIT 1
		)`).Scan(&targetHasData); err != nil {
		return false, fmt.Errorf("check PostgreSQL import target: %w", err)
	}
	if targetHasData {
		return false, errors.New("legacy SQLite import requires empty PostgreSQL business tables")
	}

	legacyDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return false, fmt.Errorf("open legacy SQLite database: %w", err)
	}
	defer legacyDB.Close()
	legacyDB.SetMaxOpenConns(1)
	legacyDB.SetMaxIdleConns(1)
	if err := legacyDB.PingContext(ctx); err != nil {
		return false, fmt.Errorf("read legacy SQLite database: %w", err)
	}

	legacyTx, err := legacyDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, fmt.Errorf("begin legacy SQLite snapshot: %w", err)
	}
	defer func() { _ = legacyTx.Rollback() }()

	for _, table := range []string{
		"admins", "admin_sessions", "accounts", "aliases", "latest_messages", "audit_logs",
	} {
		exists, err := legacySQLiteTableExists(ctx, legacyTx, table)
		if err != nil {
			return false, err
		}
		if !exists {
			return false, fmt.Errorf("legacy SQLite database is missing table %s", table)
		}
	}

	adminHasPasswordVersion, err := legacySQLiteColumnExists(ctx, legacyTx, "admins", "password_version")
	if err != nil {
		return false, err
	}
	sessionHasPasswordVersion, err := legacySQLiteColumnExists(ctx, legacyTx, "admin_sessions", "password_version")
	if err != nil {
		return false, err
	}
	hasAppleSessions, err := legacySQLiteTableExists(ctx, legacyTx, "apple_web_sessions")
	if err != nil {
		return false, err
	}

	specs := legacySQLiteCopySpecs(adminHasPasswordVersion, sessionHasPasswordVersion, hasAppleSessions)
	for _, spec := range specs {
		if err := prepareLegacyBooleanColumns(ctx, tx, &spec); err != nil {
			return false, err
		}
		if err := copyLegacySQLiteTable(ctx, legacyTx, tx, spec, validator); err != nil {
			return false, err
		}
	}
	for _, spec := range specs {
		if spec.resetSequence {
			legacySequence, err := legacySQLiteSequence(ctx, legacyTx, spec.table)
			if err != nil {
				return false, err
			}
			if err := resetPostgreSQLSequence(ctx, tx, spec.table, legacySequence); err != nil {
				return false, err
			}
		}
	}

	if err := legacyTx.Commit(); err != nil {
		return false, fmt.Errorf("finish legacy SQLite snapshot: %w", err)
	}
	if err := markLegacySQLiteMigration(ctx, tx, s); err != nil {
		return false, err
	}
	return true, nil
}

func legacySQLiteCopySpecs(adminHasVersion, sessionHasVersion, hasAppleSessions bool) []legacyCopySpec {
	adminVersion := "1"
	if adminHasVersion {
		adminVersion = "password_version"
	}
	sessionVersion := "1"
	if sessionHasVersion {
		sessionVersion = "password_version"
	}

	specs := []legacyCopySpec{
		{
			table: "admins", columnCount: 5, resetSequence: true,
			selectSQL: `SELECT id, username, password_hash, ` + adminVersion + `, created_at FROM admins ORDER BY id`,
			insertSQL: `INSERT INTO admins(id, username, password_hash, password_version, created_at)
				VALUES($1, $2, $3, $4, $5)`,
		},
		{
			table: "admin_sessions", columnCount: 6,
			selectSQL: `SELECT token_hash, admin_id, ` + sessionVersion + `, csrf, expires_at, created_at
				FROM admin_sessions ORDER BY rowid`,
			insertSQL: `INSERT INTO admin_sessions(token_hash, admin_id, password_version, csrf, expires_at, created_at)
				VALUES($1, $2, $3, $4, $5, $6)`,
		},
		{
			table: "accounts", columnCount: 13, resetSequence: true,
			selectSQL: `SELECT id, name, email, imap_host, imap_port, imap_username,
				password_ciphertext, enabled, last_sync_status, last_sync_error,
				last_synced_at, created_at, updated_at FROM accounts ORDER BY id`,
			insertSQL: `INSERT INTO accounts(
				id, name, email, imap_host, imap_port, imap_username,
				password_ciphertext, enabled, last_sync_status, last_sync_error,
				last_synced_at, created_at, updated_at
			) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			booleanColumns: map[int]string{7: "enabled"},
		},
		{
			table: "aliases", columnCount: 13, resetSequence: true,
			selectSQL: `SELECT
				id, account_id, address, label, api_key_hash, api_key_prefix, enabled,
				CASE WHEN id IN (
					SELECT alias_id FROM latest_messages WHERE uid_validity = 0 OR uid = 0
				) THEN 'pending' ELSE last_sync_status END,
				CASE WHEN id IN (
					SELECT alias_id FROM latest_messages WHERE uid_validity = 0 OR uid = 0
				) THEN '' ELSE last_sync_error END,
				CASE WHEN id IN (
					SELECT alias_id FROM latest_messages WHERE uid_validity = 0 OR uid = 0
				) THEN NULL ELSE last_synced_at END,
				last_accessed_at, created_at, updated_at
				FROM aliases ORDER BY id`,
			insertSQL: `INSERT INTO aliases(
				id, account_id, address, label, api_key_hash, api_key_prefix, enabled,
				last_sync_status, last_sync_error, last_synced_at, last_accessed_at,
				created_at, updated_at
			) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			booleanColumns: map[int]string{6: "enabled"},
		},
		{
			table: "latest_messages", columnCount: 15,
			selectSQL: `SELECT
				alias_id, uid_validity, uid, message_id, internal_date, header_date,
				from_json, to_json, cc_json, subject, text_body, html_body,
				attachments_json, body_truncated, synced_at
				FROM latest_messages
				WHERE uid_validity > 0 AND uid > 0
				ORDER BY alias_id`,
			insertSQL: `INSERT INTO latest_messages(
				alias_id, uid_validity, uid, message_id, internal_date, header_date,
				from_json, to_json, cc_json, subject, text_body, html_body,
				attachments_json, body_truncated, synced_at
			) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			booleanColumns: map[int]string{13: "body_truncated"},
		},
		{
			table: "audit_logs", columnCount: 11, resetSequence: true,
			selectSQL: `SELECT
				id, admin_id, username, action, resource_type, resource_id,
				result, ip, request_id, detail, created_at
				FROM audit_logs ORDER BY id`,
			insertSQL: `INSERT INTO audit_logs(
				id, admin_id, username, action, resource_type, resource_id,
				result, ip, request_id, detail, created_at
			) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		},
	}
	if hasAppleSessions {
		specs = append(specs, legacyCopySpec{
			table: "apple_web_sessions", columnCount: 8,
			selectSQL: `SELECT
				account_id, session_ciphertext, apple_id, region, authenticated,
				last_validated_at, created_at, updated_at
				FROM apple_web_sessions ORDER BY account_id`,
			insertSQL: `INSERT INTO apple_web_sessions(
				account_id, session_ciphertext, apple_id, region, authenticated,
				last_validated_at, created_at, updated_at
			) VALUES($1, $2, $3, $4, $5, $6, $7, $8)`,
			booleanColumns: map[int]string{4: "authenticated"},
		})
	}
	return specs
}

func copyLegacySQLiteTable(
	ctx context.Context,
	source *sql.Tx,
	target *sql.Tx,
	spec legacyCopySpec,
	validator LegacySQLiteCipherValidator,
) error {
	rows, err := source.QueryContext(ctx, spec.selectSQL)
	if err != nil {
		return fmt.Errorf("read legacy SQLite table %s: %w", spec.table, err)
	}
	defer rows.Close()

	for rows.Next() {
		values := make([]any, spec.columnCount)
		destinations := make([]any, spec.columnCount)
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return fmt.Errorf("scan legacy SQLite table %s: %w", spec.table, err)
		}
		for index := range spec.booleanColumns {
			converted, err := legacySQLiteBoolean(values[index])
			if err != nil {
				return fmt.Errorf("convert legacy SQLite %s boolean: %w", spec.table, err)
			}
			values[index] = converted
		}
		if err := prepareLegacySQLiteValues(spec.table, values, validator); err != nil {
			return err
		}
		if _, err := target.ExecContext(ctx, spec.insertSQL, values...); err != nil {
			return fmt.Errorf("import legacy SQLite table %s: %w", spec.table, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy SQLite table %s: %w", spec.table, err)
	}
	return nil
}

func prepareLegacySQLiteValues(
	table string,
	values []any,
	validator LegacySQLiteCipherValidator,
) error {
	// Ciphertexts must be authenticated byte-for-byte before ordinary SQLite
	// TEXT is normalized for PostgreSQL's UTF-8 and NUL restrictions.
	if err := validateLegacySQLiteCiphertext(table, values, validator); err != nil {
		return err
	}
	for index, value := range values {
		if (table == "accounts" && index == 6) ||
			(table == "apple_web_sessions" && index == 1) {
			continue
		}
		if text, ok := value.(string); ok {
			text = strings.ToValidUTF8(text, "\uFFFD")
			values[index] = strings.ReplaceAll(text, "\x00", "\uFFFD")
		}
	}
	return nil
}

func validateLegacySQLiteCiphertext(
	table string,
	values []any,
	validator LegacySQLiteCipherValidator,
) error {
	if validator == nil {
		return nil
	}

	var (
		ciphertext any
		validate   func(string) (string, error)
	)
	switch table {
	case "accounts":
		if len(values) <= 6 {
			return errors.New("legacy SQLite account row is missing password ciphertext")
		}
		ciphertext = values[6]
		validate = validator.Decrypt
	case "apple_web_sessions":
		if len(values) <= 1 {
			return errors.New("legacy SQLite Apple session row is missing ciphertext")
		}
		ciphertext = values[1]
		validate = validator.DecryptAppleSession
	default:
		return nil
	}

	encoded, ok := ciphertext.(string)
	if !ok {
		return fmt.Errorf("legacy SQLite %s ciphertext has type %T", table, ciphertext)
	}
	if _, err := validate(encoded); err != nil {
		return fmt.Errorf("validate legacy SQLite %s ciphertext: %w", table, err)
	}
	return nil
}

func prepareLegacyBooleanColumns(ctx context.Context, tx *sql.Tx, spec *legacyCopySpec) error {
	for index, column := range spec.booleanColumns {
		var dataType string
		if err := tx.QueryRowContext(ctx, `
			SELECT data_type
			FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`,
			spec.table, column,
		).Scan(&dataType); err != nil {
			return fmt.Errorf("inspect PostgreSQL column %s.%s: %w", spec.table, column, err)
		}
		if dataType != "boolean" {
			delete(spec.booleanColumns, index)
		}
	}
	return nil
}

func legacySQLiteBoolean(value any) (bool, error) {
	switch value := value.(type) {
	case int64:
		return value != 0, nil
	case bool:
		return value, nil
	case []byte:
		return string(value) != "0", nil
	case string:
		return value != "0", nil
	default:
		return false, fmt.Errorf("unexpected value %T", value)
	}
}

func legacySQLiteTableExists(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ? COLLATE NOCASE
		)`, table,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("inspect legacy SQLite table %s: %w", table, err)
	}
	return exists, nil
}

func legacySQLiteColumnExists(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, fmt.Errorf("inspect legacy SQLite columns for %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var position, notNull, primaryKey int64
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&position, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, fmt.Errorf("scan legacy SQLite columns for %s: %w", table, err)
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate legacy SQLite columns for %s: %w", table, err)
	}
	return false, nil
}

func legacySQLiteSequence(ctx context.Context, tx *sql.Tx, table string) (int64, error) {
	exists, err := legacySQLiteTableExists(ctx, tx, "sqlite_sequence")
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT seq FROM sqlite_sequence WHERE name = ?`, table,
	).Scan(&sequence); errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("read legacy SQLite sequence for %s: %w", table, err)
	}
	return sequence, nil
}

func resetPostgreSQLSequence(ctx context.Context, tx *sql.Tx, table string, legacySequence int64) error {
	query := fmt.Sprintf(`
		WITH sequence_state AS (
			SELECT GREATEST(COALESCE(MAX(id), 0), $1::BIGINT) AS value FROM %s
		)
		SELECT setval(
			pg_get_serial_sequence('%s', 'id'),
			CASE WHEN value > 0 THEN value ELSE 1 END,
			value > 0
		) FROM sequence_state`, table, table)
	var sequenceValue int64
	if err := tx.QueryRowContext(ctx, query, legacySequence).Scan(&sequenceValue); err != nil {
		return fmt.Errorf("reset PostgreSQL sequence for %s: %w", table, err)
	}
	return nil
}

func markLegacySQLiteMigration(ctx context.Context, tx *sql.Tx, s *Store) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO data_migrations(name, applied_at)
		VALUES($1, $2)
		ON CONFLICT (name) DO NOTHING`,
		legacySQLiteMigrationName, timestamp(s.now()),
	); err != nil {
		return fmt.Errorf("mark legacy SQLite import complete: %w", err)
	}
	return nil
}

func legacySQLiteReadOnlyDSN(path string) (dsn, resolvedPath string, err error) {
	resolvedPath = path
	if len(path) >= len("file:") && strings.EqualFold(path[:len("file:")], "file:") {
		u, parseErr := url.Parse(path)
		if parseErr != nil {
			return "", "", fmt.Errorf("parse legacy SQLite path: %w", parseErr)
		}
		if u.Host != "" && u.Host != "localhost" {
			return "", "", fmt.Errorf("legacy SQLite URL host is unsupported: %s", u.Host)
		}
		decodedPath := u.Path
		if decodedPath == "" && u.Opaque != "" {
			decodedPath, err = url.PathUnescape(u.Opaque)
			if err != nil {
				return "", "", fmt.Errorf("decode legacy SQLite path: %w", err)
			}
		}
		resolvedPath = filepath.FromSlash(decodedPath)
		if len(resolvedPath) >= 3 && resolvedPath[0] == filepath.Separator && resolvedPath[2] == ':' {
			resolvedPath = resolvedPath[1:]
		}
	}
	if resolvedPath == "" {
		return "", "", errors.New("legacy SQLite path is empty")
	}
	resolvedPath, err = filepath.Abs(resolvedPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve legacy SQLite path: %w", err)
	}
	normalized := filepath.ToSlash(resolvedPath)
	if !strings.HasPrefix(normalized, "/") {
		normalized = "/" + normalized
	}
	u := &url.URL{Scheme: "file", Path: normalized}
	query := u.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = query.Encode()
	return u.String(), resolvedPath, nil
}

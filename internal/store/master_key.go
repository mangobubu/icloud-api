package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
)

const (
	masterKeySize                  = 32
	masterKeyFingerprintName       = "master_key_fingerprint_v1"
	masterKeyFingerprintDomain     = "icloud-api/master-key-fingerprint/v1"
	masterKeyLifecycleAdvisoryLock = int64(0x49434c4f55444d47)
)

// ErrMasterKeyMismatch indicates that the supplied key is not the key that was
// previously bound to this PostgreSQL database.
var ErrMasterKeyMismatch = errors.New("master key does not match database fingerprint")

// MasterKeyCipherValidator authenticates encrypted fields before an existing
// PostgreSQL database without a fingerprint is bound to a master key.
type MasterKeyCipherValidator interface {
	Decrypt(string) (string, error)
	DecryptAppleSession(string) (string, error)
}

// VerifyStoredMasterKey checks an existing database binding without creating
// one. The bool is false only when this database has not yet stored a master
// key fingerprint.
func (s *Store) VerifyStoredMasterKey(ctx context.Context, masterKey []byte) (bool, error) {
	fingerprint, err := s.validatedMasterKeyFingerprint(masterKey)
	if err != nil {
		return false, err
	}

	tx, err := s.beginMasterKeyLifecycleTx(ctx)
	if err != nil {
		return false, fmt.Errorf("begin stored master key verification: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	bound, err := verifyStoredMasterKeyTx(ctx, tx, fingerprint)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit stored master key verification: %w", err)
	}
	return bound, nil
}

// InitializeMasterKeyWithLegacySQLite performs startup key verification,
// optional legacy import, ciphertext authentication, and first fingerprint
// binding in one PostgreSQL transaction. The transaction-level advisory lock
// serializes the complete lifecycle across application processes.
func (s *Store) InitializeMasterKeyWithLegacySQLite(
	ctx context.Context,
	masterKey []byte,
	legacyPath string,
	validator MasterKeyCipherValidator,
) error {
	fingerprint, err := s.validatedMasterKeyFingerprint(masterKey)
	if err != nil {
		return err
	}

	tx, err := s.beginMasterKeyLifecycleTx(ctx)
	if err != nil {
		return fmt.Errorf("begin master key initialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	bound, err := verifyStoredMasterKeyTx(ctx, tx, fingerprint)
	if err != nil {
		return err
	}
	imported, err := s.importLegacySQLiteWithValidatorTx(ctx, tx, legacyPath, validator, true)
	if err != nil {
		return err
	}
	if !bound || imported {
		if err := validateStoredCiphertexts(ctx, tx, validator); err != nil {
			return err
		}
	}
	if !bound {
		if err := bindMasterKeyFingerprintTx(ctx, tx, fingerprint, timestamp(s.now())); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit master key initialization: %w", err)
	}
	return nil
}

// VerifyMasterKey atomically binds a fresh PostgreSQL database to masterKey,
// then verifies that every subsequent startup uses the same key. Only a
// domain-separated fingerprint is persisted; the master key never enters the
// database.
func (s *Store) VerifyMasterKey(ctx context.Context, masterKey []byte) error {
	return s.VerifyMasterKeyWithValidator(ctx, masterKey, nil)
}

// VerifyMasterKeyWithValidator also authenticates every encrypted field before
// creating the first fingerprint in a database that already contains data.
func (s *Store) VerifyMasterKeyWithValidator(
	ctx context.Context,
	masterKey []byte,
	validator MasterKeyCipherValidator,
) error {
	fingerprint, err := s.validatedMasterKeyFingerprint(masterKey)
	if err != nil {
		return err
	}
	tx, err := s.beginMasterKeyLifecycleTx(ctx)
	if err != nil {
		return fmt.Errorf("begin master key verification: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	bound, err := verifyStoredMasterKeyTx(ctx, tx, fingerprint)
	if err != nil {
		return err
	}
	if bound {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit master key verification: %w", err)
		}
		return nil
	}

	if err := validateStoredCiphertexts(ctx, tx, validator); err != nil {
		return err
	}
	if err := bindMasterKeyFingerprintTx(ctx, tx, fingerprint, timestamp(s.now())); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit master key verification: %w", err)
	}
	return nil
}

func (s *Store) beginMasterKeyLifecycleTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	if _, err := s.txExecContext(ctx, tx,
		`SELECT pg_advisory_xact_lock(?)`, masterKeyLifecycleAdvisoryLock,
	); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("lock master key lifecycle: %w", err)
	}
	return tx, nil
}

func verifyStoredMasterKeyTx(
	ctx context.Context,
	tx *sql.Tx,
	fingerprint [sha256.Size]byte,
) (bool, error) {
	var stored []byte
	if err := tx.QueryRowContext(ctx, `
		SELECT value FROM app_metadata WHERE name = $1
	`, masterKeyFingerprintName).Scan(&stored); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("read stored master key fingerprint: %w", err)
	}
	if err := compareMasterKeyFingerprint(stored, fingerprint); err != nil {
		return false, err
	}
	return true, nil
}

func bindMasterKeyFingerprintTx(
	ctx context.Context,
	tx *sql.Tx,
	fingerprint [sha256.Size]byte,
	updatedAt int64,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO app_metadata(name, value, updated_at)
		VALUES($1, $2, $3)
		ON CONFLICT(name) DO NOTHING
	`, masterKeyFingerprintName, fingerprint[:], updatedAt); err != nil {
		return fmt.Errorf("initialize master key fingerprint: %w", err)
	}

	var stored []byte
	if err := tx.QueryRowContext(ctx, `
		SELECT value FROM app_metadata WHERE name = $1
	`, masterKeyFingerprintName).Scan(&stored); err != nil {
		return fmt.Errorf("read master key fingerprint: %w", err)
	}
	return compareMasterKeyFingerprint(stored, fingerprint)
}

func (s *Store) validatedMasterKeyFingerprint(masterKey []byte) ([sha256.Size]byte, error) {
	var empty [sha256.Size]byte
	if s == nil || s.db == nil {
		return empty, errors.New("store is unavailable")
	}
	if s.dialect != dialectPostgres {
		return empty, errors.New("master key fingerprint verification requires PostgreSQL")
	}
	if len(masterKey) != masterKeySize {
		return empty, fmt.Errorf("master key length is %d bytes; want %d", len(masterKey), masterKeySize)
	}
	return fingerprintMasterKey(masterKey), nil
}

func validateStoredCiphertexts(ctx context.Context, tx *sql.Tx, validator MasterKeyCipherValidator) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT 'imap', id, password_ciphertext FROM accounts
		UNION ALL
		SELECT 'apple', account_id, session_ciphertext FROM apple_web_sessions
		ORDER BY 1, 2
	`)
	if err != nil {
		return fmt.Errorf("read encrypted data before binding master key: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var kind string
		var accountID int64
		var ciphertext string
		if err := rows.Scan(&kind, &accountID, &ciphertext); err != nil {
			return fmt.Errorf("scan encrypted data before binding master key: %w", err)
		}
		if validator == nil {
			return errors.New("database contains encrypted data; first master key binding requires ciphertext validation")
		}

		var decryptErr error
		switch kind {
		case "imap":
			_, decryptErr = validator.Decrypt(ciphertext)
		case "apple":
			_, decryptErr = validator.DecryptAppleSession(ciphertext)
		default:
			return fmt.Errorf("unknown encrypted data kind %q", kind)
		}
		if decryptErr != nil {
			return fmt.Errorf("validate stored %s ciphertext for account %d before binding master key: %w", kind, accountID, decryptErr)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate encrypted data before binding master key: %w", err)
	}
	return nil
}

func compareMasterKeyFingerprint(stored []byte, expected [sha256.Size]byte) error {
	if len(stored) != sha256.Size {
		return fmt.Errorf("stored master key fingerprint has invalid length %d", len(stored))
	}
	if subtle.ConstantTimeCompare(stored, expected[:]) != 1 {
		return ErrMasterKeyMismatch
	}
	return nil
}

func fingerprintMasterKey(masterKey []byte) [sha256.Size]byte {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(masterKeyFingerprintDomain))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write(masterKey)

	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hasher.Sum(nil))
	return fingerprint
}

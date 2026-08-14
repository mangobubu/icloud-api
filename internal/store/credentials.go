package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"icloud-api/internal/domain"
)

var errPendingAliasAPIKeyMismatch = errors.New("pending API key does not match the established alias credential")

// ConfigureAliasCredentialFactory installs the startup-only issuer used by
// every alias creation and rotation path. It must be called before workers and
// HTTP handlers begin serving.
func (s *Store) ConfigureAliasCredentialFactory(
	factory func(aliasID, version int64) (domain.AliasCredentialMaterial, error),
) {
	s.credentialFactory = factory
}

// ConfigureAliasCredentialReuseFactory installs the startup-only issuer used
// to upgrade an unclaimed legacy key without replacing its API key.
func (s *Store) ConfigureAliasCredentialReuseFactory(
	factory func(aliasID, version int64, pendingCiphertext string) (domain.AliasCredentialMaterial, error),
) {
	s.credentialReuseFactory = factory
}

// ConfigureAliasCredentialRevealFactory installs the trusted in-process
// reader used only to return newly imported API keys in the sync response.
func (s *Store) ConfigureAliasCredentialRevealFactory(
	factory func(aliasID int64, credentialCiphertext string) (string, error),
) {
	s.credentialRevealFactory = factory
}

// ConfigureAliasAPIKeyRotationFactory installs the trusted issuer that
// rewrites only the API key inside an existing v2 credential bundle.
func (s *Store) ConfigureAliasAPIKeyRotationFactory(
	factory func(aliasID, version int64, credentialCiphertext, apiKey string) (domain.AliasCredentialMaterial, error),
) {
	s.credentialAPIKeyRotationFactory = factory
}

func validateAliasCredentialMaterial(material domain.AliasCredentialMaterial) error {
	if strings.TrimSpace(material.Ciphertext) == "" || len(material.APIKeyHash) != 32 ||
		len(material.IMAPPasswordHash) != 32 || strings.TrimSpace(material.OAuthClientID) == "" ||
		len(material.RefreshTokenHash) != 32 || material.Version < 1 {
		return errors.New("alias credential material is incomplete")
	}
	if !validAPIKeyPrefix(material.APIKeyPrefix) {
		return errors.New("alias credential material API key prefix is invalid")
	}
	if strings.ContainsAny(material.OAuthClientID, " \t\r\n") {
		return errors.New("alias OAuth client ID contains whitespace")
	}
	return nil
}

func validateAliasCredentialMaterialVersion(material domain.AliasCredentialMaterial, requestedVersion int64) error {
	if err := validateAliasCredentialMaterial(material); err != nil {
		return err
	}
	if material.Version != requestedVersion {
		return fmt.Errorf(
			"alias credential material version %d does not match requested version %d",
			material.Version, requestedVersion,
		)
	}
	return nil
}

func validAPIKeyPrefix(prefix string) bool {
	prefix = strings.TrimSpace(prefix)
	if len(prefix) != 12 || !strings.HasPrefix(prefix, "icm_") {
		return false
	}
	for _, character := range prefix[4:] {
		if (character < 'A' || character > 'Z') &&
			(character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func randomMailboxUIDValidity() (uint32, error) {
	var buffer [4]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return 0, err
	}
	value := binary.BigEndian.Uint32(buffer[:])
	if value == 0 {
		value = 1
	}
	return value, nil
}

func (s *Store) installGeneratedAliasCredentialsTx(
	ctx context.Context,
	tx *sql.Tx,
	aliasID, version int64,
	initializeMailbox bool,
) (domain.AliasCredentialMaterial, error) {
	if s.credentialFactory == nil {
		return domain.AliasCredentialMaterial{}, nil
	}
	material, err := s.credentialFactory(aliasID, version)
	if err != nil {
		return domain.AliasCredentialMaterial{}, fmt.Errorf("issue alias credentials: %w", err)
	}
	if err := validateAliasCredentialMaterialVersion(material, version); err != nil {
		return domain.AliasCredentialMaterial{}, fmt.Errorf("issue alias credentials: %w", err)
	}
	uidValidity := uint32(0)
	if initializeMailbox {
		uidValidity, err = randomMailboxUIDValidity()
		if err != nil {
			return domain.AliasCredentialMaterial{}, fmt.Errorf("generate alias mailbox UIDVALIDITY: %w", err)
		}
	}
	query := `
		UPDATE aliases SET
			credential_ciphertext = ?, api_key_hash = ?, api_key_prefix = ?,
			imap_password_hash = ?, oauth_client_id = ?, refresh_token_hash = ?,
			credential_version = ?, credential_mode = ?, updated_at = ?`
	args := []any{
		material.Ciphertext, material.APIKeyHash, material.APIKeyPrefix, material.IMAPPasswordHash,
		material.OAuthClientID, material.RefreshTokenHash, material.Version,
		domain.AliasCredentialModeV2, timestamp(s.now()),
	}
	if initializeMailbox {
		// Preserve mailbox_uid_next when an upgraded alias already owns the
		// metadata-only snapshot migrated as local UID 1.
		query += `, mailbox_uid_validity = ?`
		args = append(args, int64(uidValidity))
	}
	query += ` WHERE id = ?`
	args = append(args, aliasID)
	result, err := s.txExecContext(ctx, tx, query, args...)
	if err != nil {
		return domain.AliasCredentialMaterial{}, fmt.Errorf("store alias credentials: %w", err)
	}
	if err := requireAffected(result, "alias"); err != nil {
		return domain.AliasCredentialMaterial{}, err
	}
	return material, nil
}

// EnsureAliasCredentials initializes only aliases explicitly classified as v2
// or aliases that still have a pending one-time key. Legacy aliases without a
// pending row retain their original API key and old contract indefinitely.
// A reused pending key remains queued until the administrator acknowledges it
// through the legacy pending-key API, preserving the one-time retrieval flow.
// If a historical legacy alias was rotated after its pending row was written,
// the established key remains authoritative and the now-stale pending row is
// discarded without changing the alias or blocking the rest of startup.
func (s *Store) EnsureAliasCredentials(ctx context.Context) error {
	if s.credentialFactory == nil {
		return errors.New("alias credential factory is not configured")
	}
	for {
		var aliasID, accountID int64
		err := s.queryRowContext(ctx, `
			SELECT al.id, al.account_id FROM aliases al
			LEFT JOIN pending_alias_api_keys p ON p.alias_id = al.id
			WHERE (al.credential_mode = ? AND (al.credential_version = 0 OR al.credential_ciphertext = ''))
			   OR (p.alias_id IS NOT NULL AND (al.credential_version = 0 OR al.credential_ciphertext = ''))
			ORDER BY al.id LIMIT 1`,
			domain.AliasCredentialModeV2,
		).Scan(&aliasID, &accountID)
		if err == sql.ErrNoRows {
			break
		}
		if err != nil {
			return fmt.Errorf("find alias awaiting credential migration: %w", err)
		}
		if err := s.ensureAliasCredentialsTx(ctx, aliasID, accountID); err != nil {
			return fmt.Errorf("migrate alias %d credentials: %w", aliasID, err)
		}
	}
	return nil
}

func (s *Store) ensureAliasCredentialsTx(ctx context.Context, aliasID, accountID int64) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin alias credential migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Alias creation, rotation, and deletion all take this account row lock. A
	// startup initializer must join the same order so it cannot publish material
	// generated from a pending key that a concurrent administrator just rotated.
	if _, err := s.lockAccountVersionForUpdate(ctx, tx, accountID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("lock alias account before credential migration: %w", err)
	}

	lockSuffix := ""
	if s.dialect == dialectPostgres {
		lockSuffix = " FOR UPDATE"
	}
	var currentVersion int64
	var currentAPIKeyHash []byte
	var ciphertext, mode string
	err = s.txQueryRowContext(ctx, tx, `
		SELECT credential_version, credential_ciphertext, credential_mode, api_key_hash
		FROM aliases WHERE id = ? AND account_id = ?`+lockSuffix,
		aliasID, accountID,
	).Scan(&currentVersion, &ciphertext, &mode, &currentAPIKeyHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if currentVersion != 0 && strings.TrimSpace(ciphertext) != "" {
		return tx.Commit()
	}

	var pendingCiphertext string
	pendingErr := s.txQueryRowContext(ctx, tx,
		`SELECT api_key_ciphertext FROM pending_alias_api_keys WHERE alias_id = ?`+lockSuffix,
		aliasID,
	).Scan(&pendingCiphertext)
	switch {
	case pendingErr == nil:
		// The store does not own the master cipher. The startup factory may opt
		// into key reuse through the optional interface below.
		if s.credentialReuseFactory == nil {
			return errors.New("alias credential reuse factory is not configured")
		}
		err = s.installAliasCredentialMaterialTx(ctx, tx, aliasID, 1, true,
			s.credentialReuseFactory, pendingCiphertext, currentAPIKeyHash)
		canonicalLegacy := mode == domain.AliasCredentialModeLegacy &&
			currentVersion == 0 && strings.TrimSpace(ciphertext) == ""
		if errors.Is(err, errPendingAliasAPIKeyMismatch) && canonicalLegacy {
			// Some historical deployments could rotate a legacy API key while
			// leaving its older one-time pending row behind. The live legacy hash
			// is already in use and must win; publishing a bundle for the stale
			// pending key would silently replace it. Delete only the exact row we
			// inspected while holding the account lock, then continue startup.
			deleteResult, deleteErr := s.txExecContext(ctx, tx, `
				DELETE FROM pending_alias_api_keys
				WHERE alias_id = ? AND api_key_ciphertext = ?`,
				aliasID, pendingCiphertext,
			)
			if deleteErr != nil {
				return fmt.Errorf("delete stale pending alias key during credential migration: %w", deleteErr)
			}
			if deleteErr := requireAffected(deleteResult, "stale pending alias key"); deleteErr != nil {
				return fmt.Errorf("delete stale pending alias key during credential migration: %w", deleteErr)
			}
			err = nil
		}
	case errors.Is(pendingErr, sql.ErrNoRows) && mode == domain.AliasCredentialModeV2:
		_, err = s.installGeneratedAliasCredentialsTx(ctx, tx, aliasID, 1, true)
	case errors.Is(pendingErr, sql.ErrNoRows):
		// The pending row may have been acknowledged before this transaction took
		// its locks. A legacy alias without it must remain untouched.
		return tx.Commit()
	default:
		return pendingErr
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) installAliasCredentialMaterialTx(
	ctx context.Context,
	tx *sql.Tx,
	aliasID, version int64,
	initializeMailbox bool,
	issuer func(aliasID, version int64, pendingCiphertext string) (domain.AliasCredentialMaterial, error),
	pendingCiphertext string,
	expectedAPIKeyHash []byte,
) error {
	material, err := issuer(aliasID, version, pendingCiphertext)
	if err != nil {
		return err
	}
	if err := validateAliasCredentialMaterialVersion(material, version); err != nil {
		return err
	}
	if len(expectedAPIKeyHash) != 32 {
		return errors.New("established alias API key hash is invalid")
	}
	if subtle.ConstantTimeCompare(material.APIKeyHash, expectedAPIKeyHash) != 1 {
		return errPendingAliasAPIKeyMismatch
	}
	uidValidity := uint32(0)
	if initializeMailbox {
		uidValidity, err = randomMailboxUIDValidity()
		if err != nil {
			return err
		}
	}
	query := `UPDATE aliases SET credential_ciphertext = ?, api_key_hash = ?, api_key_prefix = ?,
		imap_password_hash = ?, oauth_client_id = ?, refresh_token_hash = ?,
		credential_version = ?, credential_mode = ?, updated_at = ?`
	args := []any{material.Ciphertext, material.APIKeyHash, material.APIKeyPrefix, material.IMAPPasswordHash,
		material.OAuthClientID, material.RefreshTokenHash, material.Version,
		domain.AliasCredentialModeV2, timestamp(s.now())}
	if initializeMailbox {
		query += `, mailbox_uid_validity = ?`
		args = append(args, int64(uidValidity))
	}
	query += ` WHERE id = ?`
	args = append(args, aliasID)
	result, err := s.txExecContext(ctx, tx, query, args...)
	if err != nil {
		return err
	}
	return requireAffected(result, "alias")
}

func (s *Store) RotateAliasCredentials(ctx context.Context, id int64) (domain.Alias, error) {
	if s.credentialFactory == nil {
		return domain.Alias{}, errors.New("alias credential factory is not configured")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return domain.Alias{}, fmt.Errorf("begin alias credential rotation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var accountID int64
	if err := s.txQueryRowContext(ctx, tx, `
		SELECT account_id
		FROM aliases WHERE id = ?`, id,
	).Scan(&accountID); err != nil {
		if err == sql.ErrNoRows {
			return domain.Alias{}, ErrNotFound
		}
		return domain.Alias{}, fmt.Errorf("read alias before credential rotation: %w", err)
	}
	if _, err := s.lockAccountVersionForUpdate(ctx, tx, accountID); err != nil {
		return domain.Alias{}, fmt.Errorf("lock alias account before credential rotation: %w", err)
	}
	// Read the version only after taking the account lock. All alias mutations
	// for one account use this lock order, so concurrent rotations cannot both
	// replace the bundle with the same credential version.
	var version int64
	var credentialMode string
	var enabled bool
	var lastSyncError string
	if err := s.txQueryRowContext(ctx, tx, `
		SELECT credential_version, credential_mode, enabled, last_sync_error
		FROM aliases WHERE id = ? AND account_id = ?`, id, accountID,
	).Scan(&version, &credentialMode, &enabled, &lastSyncError); err != nil {
		if err == sql.ErrNoRows {
			return domain.Alias{}, ErrNotFound
		}
		return domain.Alias{}, fmt.Errorf("read locked alias before credential rotation: %w", err)
	}
	if !enabled && lastSyncError == domain.AppleAliasConfirmationPending {
		return domain.Alias{}, fmt.Errorf("rotate alias credentials: %w", ErrAliasConfirmationPending)
	}
	if credentialMode != domain.AliasCredentialModeV2 {
		return domain.Alias{}, fmt.Errorf("rotate alias credentials for mode %q: %w", credentialMode, ErrAliasCredentialMode)
	}
	if version < 1 {
		version = 1
	} else {
		version++
	}
	if _, err := s.installGeneratedAliasCredentialsTx(ctx, tx, id, version, false); err != nil {
		return domain.Alias{}, err
	}
	if _, err := s.txExecContext(ctx, tx,
		`DELETE FROM pending_alias_api_keys WHERE alias_id = ?`, id,
	); err != nil {
		return domain.Alias{}, fmt.Errorf("delete stale pending alias key after credential rotation: %w", err)
	}
	rotated, err := s.getAliasByIDTx(ctx, tx, id)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("read alias after credential rotation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Alias{}, fmt.Errorf("commit alias credential rotation: %w", err)
	}
	return rotated, nil
}

// RotateAliasAPIKey preserves the original hash-only store contract. Legacy
// aliases can be rotated without their raw key because they have no encrypted
// v2 bundle. Call RotateAliasAPIKeyWithRawKey for a v2 alias.
func (s *Store) RotateAliasAPIKey(
	ctx context.Context,
	id int64,
	apiKeyHash []byte,
	apiKeyPrefix string,
) (domain.Alias, error) {
	return s.rotateAliasAPIKey(ctx, id, apiKeyHash, apiKeyPrefix, "")
}

// RotateAliasAPIKeyWithRawKey changes only the API key. For v2 aliases, the
// encrypted bundle is rewritten while IMAP/OAuth authenticators and the
// credential version remain unchanged.
func (s *Store) RotateAliasAPIKeyWithRawKey(
	ctx context.Context,
	id int64,
	apiKeyHash []byte,
	apiKeyPrefix, rawAPIKey string,
) (domain.Alias, error) {
	return s.rotateAliasAPIKey(ctx, id, apiKeyHash, apiKeyPrefix, rawAPIKey)
}

func (s *Store) rotateAliasAPIKey(
	ctx context.Context,
	id int64,
	apiKeyHash []byte,
	apiKeyPrefix, rawAPIKey string,
) (domain.Alias, error) {
	if len(apiKeyHash) == 0 {
		return domain.Alias{}, errors.New("rotate alias API key: hash is empty")
	}
	var accountID int64
	if err := s.queryRowContext(ctx,
		`SELECT account_id FROM aliases WHERE id = ?`, id,
	).Scan(&accountID); err != nil {
		if err == sql.ErrNoRows {
			return domain.Alias{}, ErrNotFound
		}
		return domain.Alias{}, fmt.Errorf("read alias account before API key rotation: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return domain.Alias{}, fmt.Errorf("begin alias API key rotation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.lockAccountVersionForUpdate(ctx, tx, accountID); err != nil {
		return domain.Alias{}, fmt.Errorf("lock alias account before API key rotation: %w", err)
	}

	var current domain.Alias
	if err := s.txQueryRowContext(ctx, tx, `
		SELECT enabled, last_sync_error, credential_mode, credential_ciphertext,
		       imap_password_hash, oauth_client_id, refresh_token_hash, credential_version
		FROM aliases WHERE id = ? AND account_id = ?`, id, accountID,
	).Scan(
		&current.Enabled, &current.LastSyncError, &current.CredentialMode,
		&current.CredentialCiphertext, &current.IMAPPasswordHash,
		&current.OAuthClientID, &current.RefreshTokenHash, &current.CredentialVersion,
	); err != nil {
		if err == sql.ErrNoRows {
			return domain.Alias{}, ErrNotFound
		}
		return domain.Alias{}, fmt.Errorf("read alias before API key rotation: %w", err)
	}
	if !current.Enabled && current.LastSyncError == domain.AppleAliasConfirmationPending {
		return domain.Alias{}, fmt.Errorf("rotate alias API key: %w", ErrAliasConfirmationPending)
	}

	sanitizedPrefix := strings.TrimSpace(sanitizePostgresText(apiKeyPrefix))
	switch current.CredentialMode {
	case domain.AliasCredentialModeLegacy:
		result, updateErr := s.txExecContext(ctx, tx, `
			UPDATE aliases
			SET api_key_hash = ?, api_key_prefix = ?, updated_at = ?
			WHERE id = ? AND account_id = ?`,
			apiKeyHash, sanitizedPrefix, timestamp(s.now()), id, accountID,
		)
		if updateErr != nil {
			return domain.Alias{}, fmt.Errorf("rotate legacy alias API key: %w", updateErr)
		}
		if err := requireAffected(result, "alias"); err != nil {
			return domain.Alias{}, err
		}

	case domain.AliasCredentialModeV2:
		if strings.TrimSpace(rawAPIKey) == "" {
			return domain.Alias{}, errors.New("rotate v2 alias API key: raw key is required")
		}
		if s.credentialAPIKeyRotationFactory == nil {
			return domain.Alias{}, errors.New("rotate v2 alias API key: rotation factory is not configured")
		}
		if current.CredentialVersion < 1 || strings.TrimSpace(current.CredentialCiphertext) == "" {
			return domain.Alias{}, errors.New("rotate v2 alias API key: credential bundle is incomplete")
		}
		material, issueErr := s.credentialAPIKeyRotationFactory(
			id, current.CredentialVersion, current.CredentialCiphertext, rawAPIKey,
		)
		if issueErr != nil {
			return domain.Alias{}, fmt.Errorf("rotate v2 alias API key: %w", issueErr)
		}
		if err := validateAliasCredentialMaterialVersion(material, current.CredentialVersion); err != nil {
			return domain.Alias{}, fmt.Errorf("rotate v2 alias API key: %w", err)
		}
		if material.Version != current.CredentialVersion ||
			subtle.ConstantTimeCompare(material.APIKeyHash, apiKeyHash) != 1 ||
			material.APIKeyPrefix != sanitizedPrefix ||
			subtle.ConstantTimeCompare(material.IMAPPasswordHash, current.IMAPPasswordHash) != 1 ||
			material.OAuthClientID != current.OAuthClientID ||
			subtle.ConstantTimeCompare(material.RefreshTokenHash, current.RefreshTokenHash) != 1 {
			return domain.Alias{}, errors.New("rotate v2 alias API key: issuer changed non-API credentials")
		}
		result, updateErr := s.txExecContext(ctx, tx, `
			UPDATE aliases
			SET credential_ciphertext = ?, api_key_hash = ?, api_key_prefix = ?, updated_at = ?
			WHERE id = ? AND account_id = ?`,
			material.Ciphertext, material.APIKeyHash, sanitizedPrefix,
			timestamp(s.now()), id, accountID,
		)
		if updateErr != nil {
			return domain.Alias{}, fmt.Errorf("store rotated v2 alias API key: %w", updateErr)
		}
		if err := requireAffected(result, "alias"); err != nil {
			return domain.Alias{}, err
		}

	default:
		return domain.Alias{}, fmt.Errorf("rotate alias API key: unsupported credential mode %q", current.CredentialMode)
	}

	if _, err := s.txExecContext(ctx, tx,
		`DELETE FROM pending_alias_api_keys WHERE alias_id = ?`, id,
	); err != nil {
		return domain.Alias{}, fmt.Errorf("delete stale pending alias key after rotation: %w", err)
	}
	rotated, err := s.getAliasByIDTx(ctx, tx, id)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("read alias after API key rotation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Alias{}, fmt.Errorf("commit alias API key rotation: %w", err)
	}
	return rotated, nil
}

func (s *Store) GetAliasByAPIKeyHash(ctx context.Context, hash []byte) (domain.Alias, error) {
	return scanAlias(s.queryRowContext(ctx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.api_key_hash = ?`, hash,
	))
}

func (s *Store) GetAliasByIMAPPasswordHash(ctx context.Context, hash []byte) (domain.Alias, error) {
	return scanAlias(s.queryRowContext(ctx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.imap_password_hash = ?`, hash,
	))
}

func (s *Store) GetAliasByOAuthClientID(ctx context.Context, clientID string) (domain.Alias, error) {
	return scanAlias(s.queryRowContext(ctx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.oauth_client_id = ?`, strings.TrimSpace(clientID),
	))
}

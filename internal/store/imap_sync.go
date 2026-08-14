package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

// GetIMAPSyncState returns the last atomically committed mailbox cursor for an
// account.
func (s *Store) GetIMAPSyncState(ctx context.Context, accountID int64) (domain.IMAPSyncState, error) {
	if accountID < 1 {
		return domain.IMAPSyncState{}, fmt.Errorf("get IMAP sync state: account ID must be positive")
	}

	var state domain.IMAPSyncState
	var uidValidity, lastUID int64
	var updatedAt int64
	err := s.queryRowContext(ctx, `
		SELECT account_id, uid_validity, last_uid, updated_at
		FROM imap_sync_states
		WHERE account_id = ?`, accountID,
	).Scan(&state.AccountID, &uidValidity, &lastUID, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.IMAPSyncState{}, ErrNotFound
		}
		return domain.IMAPSyncState{}, fmt.Errorf("get IMAP sync state: %w", err)
	}
	if uidValidity < 1 || uidValidity > int64(^uint32(0)) || lastUID < 0 || lastUID > int64(^uint32(0)) {
		return domain.IMAPSyncState{}, fmt.Errorf(
			"get IMAP sync state: invalid mailbox position UIDVALIDITY=%d UID=%d",
			uidValidity, lastUID,
		)
	}
	state.UIDValidity = uint32(uidValidity)
	state.LastUID = uint32(lastUID)
	state.UpdatedAt = timeFromTimestamp(updatedAt)
	return state, nil
}

// ListMailboxSnapshotPositions returns the persisted v1 compatibility
// snapshots for every enabled alias. V2 aliases use the archive for IMAPS and
// OTP history, but their API keys and direct links still work with v1 routes.
func (s *Store) ListMailboxSnapshotPositions(
	ctx context.Context,
	accountID int64,
) (map[int64]domain.MailboxSnapshotPosition, error) {
	if accountID < 1 {
		return nil, fmt.Errorf("list mailbox snapshot positions: account ID must be positive")
	}
	rows, err := s.queryContext(ctx, `
		SELECT lm.alias_id, lm.uid_validity, lm.uid
		FROM latest_messages lm
		JOIN aliases a ON a.id = lm.alias_id
		WHERE a.account_id = ? AND a.enabled = TRUE
		ORDER BY lm.alias_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list mailbox snapshot positions: %w", err)
	}
	defer rows.Close()

	positions := make(map[int64]domain.MailboxSnapshotPosition)
	for rows.Next() {
		var position domain.MailboxSnapshotPosition
		var uidValidity, uid int64
		if err := rows.Scan(&position.AliasID, &uidValidity, &uid); err != nil {
			return nil, fmt.Errorf("scan mailbox snapshot position: %w", err)
		}
		if position.AliasID < 1 || uidValidity < 1 || uidValidity > int64(^uint32(0)) ||
			uid < 1 || uid > int64(^uint32(0)) {
			return nil, fmt.Errorf(
				"list mailbox snapshot positions: invalid position alias=%d UIDVALIDITY=%d UID=%d",
				position.AliasID, uidValidity, uid,
			)
		}
		position.UIDValidity = uint32(uidValidity)
		position.UID = uint32(uid)
		positions[position.AliasID] = position
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mailbox snapshot positions: %w", err)
	}
	return positions, nil
}

// ApplyMailboxSync publishes archived messages, advances the account cursor,
// and updates enabled aliases to pending or healthy in one transaction.
func (s *Store) ApplyMailboxSync(
	ctx context.Context,
	accountID int64,
	expectedAccountVersion time.Time,
	enabledAliases []domain.Alias,
	result domain.MailboxSyncResult,
	syncedAt time.Time,
) error {
	defer s.cleanupArchiveInputs(result.ArchivedMessages)
	if expectedAccountVersion.IsZero() {
		return fmt.Errorf("apply mailbox sync: expected account version is required")
	}
	if err := validateMailboxSync(accountID, enabledAliases, result); err != nil {
		return err
	}
	unlockArchive := s.lockMailArchiveAccount(accountID)
	defer unlockArchive()
	stagedArchive, err := s.stageArchiveMessages(result.ArchivedMessages)
	if err != nil {
		return fmt.Errorf("stage mailbox archive: %w", err)
	}
	archiveCommitted := false
	defer func() {
		if !archiveCommitted {
			s.cleanupPublishedArchive(stagedArchive)
		}
	}()
	if syncedAt.IsZero() {
		syncedAt = s.now()
	}
	syncedAt = syncedAt.UTC()
	observedAt := result.State.UpdatedAt
	if !observedAt.IsZero() {
		observedAt = observedAt.UTC()
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin mailbox sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, accountID)
	if err != nil {
		return fmt.Errorf("lock account for mailbox sync: %w", err)
	}
	if accountVersion != timestamp(expectedAccountVersion) {
		return nil
	}
	aliasesMatch, err := s.enabledAliasSetMatchesTx(ctx, tx, accountID, enabledAliases)
	if err != nil {
		return fmt.Errorf("recheck enabled aliases before mailbox publish: %w", err)
	}
	if !aliasesMatch {
		// Account and alias mutations invalidate the cursor while holding the
		// same account lock. A result collected for an older alias set must not
		// recreate that cursor or mark aliases it never observed as healthy.
		return nil
	}

	var currentUIDValidity, currentLastUID, currentUpdatedAt int64
	currentStateExists := false
	err = s.txQueryRowContext(ctx, tx, `
		SELECT uid_validity, last_uid, updated_at
		FROM imap_sync_states
		WHERE account_id = ?`, accountID,
	).Scan(&currentUIDValidity, &currentLastUID, &currentUpdatedAt)
	switch {
	case err == nil:
		currentStateExists = true
		currentObservedAt := timeFromTimestamp(currentUpdatedAt)
		sameGeneration := currentUIDValidity == int64(result.State.UIDValidity)
		if sameGeneration && currentLastUID > int64(result.State.LastUID) {
			return nil
		}
		if !sameGeneration && !result.Reset {
			return nil
		}
		if sameGeneration && (observedAt.IsZero() || observedAt.Before(currentObservedAt)) {
			// Wall clocks can move backwards. UID is the monotonic position within
			// one generation, so keep the later observation timestamp without
			// suppressing cursor progress or a same-position health refresh.
			observedAt = currentObservedAt
		}
		if !sameGeneration && observedAt.IsZero() {
			observedAt = syncedAt
		}
	case err == sql.ErrNoRows:
		// Only an authoritative no-backfill baseline may establish a missing cursor.
		// This also prevents an in-flight incremental result from recreating a
		// cursor that an account or alias mutation deliberately invalidated.
		if !result.Reset {
			return nil
		}
		if observedAt.IsZero() {
			observedAt = syncedAt
		}
	case err != nil:
		return fmt.Errorf("read IMAP sync state before publish: %w", err)
	}

	if err := s.persistArchivedMessagesTx(ctx, tx, result.ArchivedMessages, stagedArchive, syncedAt); err != nil {
		return fmt.Errorf("persist mailbox archive: %w", err)
	}
	// Keep the v1 one-row snapshot in sync while the archive API uses the new
	// normalized tables. This is intentionally a dual write: legacy aliases
	// retain their old routes and consumption state, while v2 aliases can read
	// the archive without depending on this compatibility table.
	resetAllLegacySnapshots := result.Reset && currentStateExists &&
		currentUIDValidity != int64(result.State.UIDValidity)
	if err := s.persistLegacyLatestMessagesTx(
		ctx, tx, accountID, result, syncedAt, resetAllLegacySnapshots,
	); err != nil {
		return fmt.Errorf("persist legacy mailbox snapshot: %w", err)
	}

	if _, err := s.txExecContext(ctx, tx, `
		INSERT INTO imap_sync_states(account_id, uid_validity, last_uid, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			uid_validity = excluded.uid_validity,
			last_uid = excluded.last_uid,
			updated_at = excluded.updated_at`,
		accountID, int64(result.State.UIDValidity), int64(result.State.LastUID), timestamp(observedAt),
	); err != nil {
		return fmt.Errorf("upsert IMAP sync state: %w", err)
	}

	syncStatus := domain.SyncStatusOK
	if result.HasMore {
		syncStatus = domain.SyncStatusPending
	}
	if _, err := s.txExecContext(ctx, tx, `
		UPDATE aliases
		SET last_sync_status = ?, last_sync_error = '', last_synced_at = ?, updated_at = ?
		WHERE account_id = ? AND enabled = TRUE`,
		syncStatus, timestamp(syncedAt), timestamp(syncedAt), accountID,
	); err != nil {
		return fmt.Errorf("mark mailbox aliases synced: %w", err)
	}
	nextAccountVersion, err := s.nextAccountVersion(accountVersion)
	if err != nil {
		return fmt.Errorf("advance account version after mailbox sync: %w", err)
	}
	accountResult, err := s.txExecContext(ctx, tx, `
		UPDATE accounts
		SET last_sync_status = ?, last_sync_error = '', last_synced_at = ?, updated_at = ?
		WHERE id = ? AND updated_at = ?`,
		syncStatus, timestamp(syncedAt), nextAccountVersion, accountID, accountVersion,
	)
	if err != nil {
		return fmt.Errorf("mark mailbox account synced: %w", err)
	}
	if err := requireAffected(accountResult, "account"); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mailbox sync: %w", err)
	}
	archiveCommitted = true
	if err := s.EnforceMailArchiveLimit(ctx); err != nil {
		return fmt.Errorf("enforce mailbox archive limit: %w", err)
	}
	return nil
}

func (s *Store) persistLegacyLatestMessagesTx(
	ctx context.Context,
	tx *sql.Tx,
	accountID int64,
	result domain.MailboxSyncResult,
	syncedAt time.Time,
	resetAllSnapshots bool,
) error {
	if result.Reset {
		query := `DELETE FROM latest_messages WHERE alias_id IN (SELECT id FROM aliases WHERE account_id = ?)`
		args := []any{accountID}
		if !resetAllSnapshots && result.State.UIDValidity != 0 {
			query += ` AND uid_validity <> ?`
			args = append(args, int64(result.State.UIDValidity))
		}
		if _, err := s.txExecContext(ctx, tx, query, args...); err != nil {
			return err
		}
	}
	for aliasID, update := range result.LegacySnapshotUpdates {
		if update.SnapshotState != domain.SnapshotEmpty {
			continue
		}
		if _, err := s.txExecContext(ctx, tx, `
			DELETE FROM latest_messages
			WHERE alias_id = ?
			  AND EXISTS (
				SELECT 1 FROM aliases
				WHERE id = ? AND account_id = ? AND enabled = TRUE
			  )`,
			aliasID, aliasID, accountID,
		); err != nil {
			return err
		}
	}
	for _, archived := range result.ArchivedMessages {
		// The v1 contract only exposed messages discovered as unread. Keep an
		// already-read message in the normalized v2 archive, but do not let the
		// compatibility projection resurrect or replace the legacy latest row.
		if archived.UpstreamSeen {
			continue
		}
		if len(archived.AliasIDs) == 0 {
			continue
		}
		messageSyncedAt := archived.SyncedAt
		if messageSyncedAt.IsZero() {
			messageSyncedAt = syncedAt
		}
		for _, aliasID := range archived.AliasIDs {
			if aliasID < 1 {
				return fmt.Errorf("invalid legacy snapshot alias ID %d", aliasID)
			}
			fromJSON, err := marshalJSONList(archived.From)
			if err != nil {
				return err
			}
			toJSON, err := marshalJSONList(archived.To)
			if err != nil {
				return err
			}
			ccJSON, err := marshalJSONList(archived.CC)
			if err != nil {
				return err
			}
			attachmentsJSON, err := marshalJSONList(archived.Attachments)
			if err != nil {
				return err
			}
			if _, err := s.txExecContext(ctx, tx, `
				INSERT INTO latest_messages(
					alias_id, uid_validity, uid, message_id, internal_date, header_date,
					from_json, to_json, cc_json, subject, text_body, html_body,
					attachments_json, body_truncated, synced_at
				) SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
				WHERE EXISTS (SELECT 1 FROM aliases WHERE id = ? AND account_id = ?)
				ON CONFLICT(alias_id) DO UPDATE SET
					uid_validity = excluded.uid_validity, uid = excluded.uid,
					message_id = excluded.message_id, internal_date = excluded.internal_date,
					header_date = excluded.header_date, from_json = excluded.from_json,
					to_json = excluded.to_json, cc_json = excluded.cc_json,
					subject = excluded.subject, text_body = excluded.text_body,
					html_body = excluded.html_body, attachments_json = excluded.attachments_json,
					body_truncated = excluded.body_truncated, synced_at = excluded.synced_at
				WHERE excluded.uid_validity != latest_messages.uid_validity
				   OR (excluded.uid_validity = latest_messages.uid_validity AND excluded.uid >= latest_messages.uid)`,
				aliasID, int64(archived.UIDValidity), int64(archived.UID),
				sanitizePostgresText(archived.MessageID), timestamp(archived.InternalDate),
				nullableTimestamp(archived.HeaderDate), fromJSON, toJSON, ccJSON,
				sanitizePostgresText(archived.Subject), sanitizePostgresText(archived.TextBody),
				sanitizePostgresText(archived.HTMLBody), attachmentsJSON, archived.BodyTruncated,
				timestamp(messageSyncedAt), aliasID, accountID); err != nil {
				return err
			}
		}
	}
	return nil
}

// RecordMailboxSyncFailure marks one account and all of its currently enabled
// aliases failed without issuing one UPDATE per alias.
func (s *Store) RecordMailboxSyncFailure(
	ctx context.Context,
	accountID int64,
	expectedAccountVersion time.Time,
	message string,
	at time.Time,
) error {
	if accountID < 1 {
		return fmt.Errorf("record mailbox sync failure: account ID must be positive")
	}
	if expectedAccountVersion.IsZero() {
		return fmt.Errorf("record mailbox sync failure: expected account version is required")
	}
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC()
	message = strings.TrimSpace(sanitizePostgresText(message))

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin mailbox sync failure: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Keep the same account-then-alias lock order as successful publication.
	// Besides preventing a PostgreSQL deadlock, the lock makes the freshness
	// check and both status writes one serialized account-scoped decision.
	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, accountID)
	if err != nil {
		return fmt.Errorf("lock account for mailbox sync failure: %w", err)
	}
	if accountVersion != timestamp(expectedAccountVersion) {
		return nil
	}

	atTimestamp := timestamp(at)
	if _, err := s.txExecContext(ctx, tx, `
		UPDATE aliases
		SET last_sync_status = ?, last_sync_error = ?, last_synced_at = ?, updated_at = ?
		WHERE account_id = ? AND enabled = TRUE`,
		domain.SyncStatusError, message, atTimestamp, atTimestamp, accountID,
	); err != nil {
		return fmt.Errorf("mark mailbox aliases failed: %w", err)
	}
	nextAccountVersion, err := s.nextAccountVersion(accountVersion)
	if err != nil {
		return fmt.Errorf("advance account version after mailbox sync failure: %w", err)
	}
	accountResult, err := s.txExecContext(ctx, tx, `
		UPDATE accounts
		SET last_sync_status = ?, last_sync_error = ?, last_synced_at = ?, updated_at = ?
		WHERE id = ? AND updated_at = ?`,
		domain.SyncStatusError, message, atTimestamp, nextAccountVersion, accountID, accountVersion,
	)
	if err != nil {
		return fmt.Errorf("mark mailbox account failed: %w", err)
	}
	if err := requireAffected(accountResult, "account"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mailbox sync failure: %w", err)
	}
	return nil
}

func (s *Store) enabledAliasSetMatchesTx(
	ctx context.Context,
	tx *sql.Tx,
	accountID int64,
	enabledAliases []domain.Alias,
) (bool, error) {
	expectedIDs := make([]int64, len(enabledAliases))
	for index, alias := range enabledAliases {
		expectedIDs[index] = alias.ID
	}
	sort.Slice(expectedIDs, func(left, right int) bool { return expectedIDs[left] < expectedIDs[right] })

	rows, err := s.txQueryContext(ctx, tx, `
		SELECT id
		FROM aliases
		WHERE account_id = ? AND enabled = TRUE
		ORDER BY id`, accountID)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	index := 0
	for rows.Next() {
		var aliasID int64
		if err := rows.Scan(&aliasID); err != nil {
			return false, err
		}
		if index >= len(expectedIDs) || expectedIDs[index] != aliasID {
			return false, nil
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return index == len(expectedIDs), nil
}

func validateMailboxSync(
	accountID int64,
	enabledAliases []domain.Alias,
	result domain.MailboxSyncResult,
) error {
	if accountID < 1 {
		return fmt.Errorf("apply mailbox sync: account ID must be positive")
	}
	if result.State.AccountID != accountID {
		return fmt.Errorf(
			"apply mailbox sync: state account ID %d does not match account %d",
			result.State.AccountID, accountID,
		)
	}
	if result.State.UIDValidity == 0 {
		return fmt.Errorf("apply mailbox sync: state UIDVALIDITY must be positive")
	}
	enabled := make(map[int64]domain.Alias, len(enabledAliases))
	for _, alias := range enabledAliases {
		if alias.ID < 1 {
			return fmt.Errorf("apply mailbox sync: enabled alias ID must be positive")
		}
		if alias.AccountID != accountID {
			return fmt.Errorf(
				"apply mailbox sync: alias %d belongs to account %d, not account %d",
				alias.ID, alias.AccountID, accountID,
			)
		}
		if !alias.Enabled {
			return fmt.Errorf("apply mailbox sync: alias %d is not enabled", alias.ID)
		}
		if _, duplicate := enabled[alias.ID]; duplicate {
			return fmt.Errorf("apply mailbox sync: duplicate enabled alias %d", alias.ID)
		}
		enabled[alias.ID] = alias
	}
	for aliasID, update := range result.LegacySnapshotUpdates {
		_, ok := enabled[aliasID]
		if !ok {
			return fmt.Errorf("apply mailbox sync: legacy snapshot alias %d is not enabled", aliasID)
		}
		if update.AliasID != aliasID {
			return fmt.Errorf(
				"apply mailbox sync: legacy snapshot alias ID %d does not match map key %d",
				update.AliasID, aliasID,
			)
		}
		if update.SnapshotState != domain.SnapshotEmpty ||
			update.UIDValidity != result.State.UIDValidity || update.UID != 0 {
			return fmt.Errorf("apply mailbox sync: legacy snapshot alias %d has invalid empty state", aliasID)
		}
	}
	upstreamUIDs := make(map[uint32]struct{}, len(result.ArchivedMessages))
	for index, message := range result.ArchivedMessages {
		if message.AccountID != accountID || message.UIDValidity != result.State.UIDValidity ||
			message.UID == 0 || message.UID > result.State.LastUID || message.InternalDate.IsZero() {
			return fmt.Errorf("apply mailbox sync: archived message %d has invalid identity", index)
		}
		if _, duplicate := upstreamUIDs[message.UID]; duplicate {
			return fmt.Errorf("apply mailbox sync: archived message UID %d is duplicated", message.UID)
		}
		upstreamUIDs[message.UID] = struct{}{}
		if len(message.AliasIDs) == 0 {
			return fmt.Errorf("apply mailbox sync: archived message %d has no alias recipients", index)
		}
		messageAliases := make(map[int64]struct{}, len(message.AliasIDs))
		for _, aliasID := range message.AliasIDs {
			if _, ok := enabled[aliasID]; !ok {
				return fmt.Errorf("apply mailbox sync: archived message alias %d is not enabled", aliasID)
			}
			if _, duplicate := messageAliases[aliasID]; duplicate {
				return fmt.Errorf("apply mailbox sync: archived message alias %d is duplicated", aliasID)
			}
			messageAliases[aliasID] = struct{}{}
		}
	}
	return nil
}

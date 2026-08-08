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

// ConsumeLatestMessage atomically claims one exact alias snapshot and queues
// its IMAP \Seen update. False means the snapshot changed, became unavailable,
// or was already consumed. The database account-row lock follows the same
// ordering as mailbox sync, so a delayed API response cannot enqueue work for
// a snapshot replaced by sync.
func (s *Store) ConsumeLatestMessage(
	ctx context.Context,
	aliasID int64,
	expectedAPIKeyHash []byte,
	expectedLastSyncedAt time.Time,
	expectedMessageSyncedAt time.Time,
	uidValidity, uid uint32,
	consumedAt time.Time,
) (bool, error) {
	if aliasID < 1 {
		return false, fmt.Errorf("consume latest message: alias ID must be positive")
	}
	if len(expectedAPIKeyHash) == 0 {
		return false, fmt.Errorf("consume latest message: expected API key hash is required")
	}
	if expectedLastSyncedAt.IsZero() {
		return false, fmt.Errorf("consume latest message: expected last synced time is required")
	}
	if expectedMessageSyncedAt.IsZero() {
		return false, fmt.Errorf("consume latest message: expected message synced time is required")
	}
	if uidValidity == 0 {
		return false, fmt.Errorf("consume latest message: UIDVALIDITY must be positive")
	}
	if uid == 0 {
		return false, fmt.Errorf("consume latest message: UID must be positive")
	}
	if consumedAt.IsZero() {
		consumedAt = s.now()
	}
	consumedAt = consumedAt.UTC()

	// Resolve immutable ownership before opening the write transaction. This
	// lets SQLite take its write lock as the transaction's first statement.
	var accountID int64
	if err := s.queryRowContext(ctx,
		`SELECT account_id FROM aliases WHERE id = ?`, aliasID,
	).Scan(&accountID); err != nil {
		if err == sql.ErrNoRows {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("read alias account before message consumption: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin latest message consumption: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.lockAccountForUpdate(ctx, tx, accountID); err != nil {
		return false, fmt.Errorf("lock account for latest message consumption: %w", err)
	}
	result, err := s.txExecContext(ctx, tx, `
		INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at)
		SELECT lm.alias_id, lm.uid_validity, lm.uid, ?
		FROM latest_messages lm
		JOIN aliases al ON al.id = lm.alias_id
		JOIN accounts account ON account.id = al.account_id
		WHERE lm.alias_id = ? AND al.account_id = ?
		  AND al.enabled = TRUE AND account.enabled = TRUE
		  AND al.api_key_hash = ?
		  AND al.last_sync_status = ? AND al.last_synced_at = ?
		  AND lm.synced_at = ? AND lm.uid_validity = ? AND lm.uid = ?
		ON CONFLICT(alias_id, uid_validity, uid) DO NOTHING`,
		timestamp(consumedAt), aliasID, accountID, expectedAPIKeyHash,
		domain.SyncStatusOK, timestamp(expectedLastSyncedAt), timestamp(expectedMessageSyncedAt),
		int64(uidValidity), int64(uid),
	)
	if err != nil {
		return false, fmt.Errorf("claim latest message consumption: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read latest message consumption result: %w", err)
	}
	if changed == 0 {
		return false, nil
	}

	if _, err := s.txExecContext(ctx, tx, `
		INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(account_id, uid_validity, uid) DO NOTHING`,
		accountID, int64(uidValidity), int64(uid), timestamp(consumedAt),
	); err != nil {
		return false, fmt.Errorf("enqueue IMAP seen task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit latest message consumption: %w", err)
	}
	return true, nil
}

// ListSeenTaskAccountIDs lists accounts with pending tasks in oldest-first
// order. The result is intentionally unbounded: limiting an oldest-first
// account prefix would permanently starve later accounts if every account in
// that prefix remained disabled or failed repeatedly.
func (s *Store) ListSeenTaskAccountIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.queryContext(ctx, `
		SELECT account_id
		FROM imap_seen_tasks
		GROUP BY account_id
		ORDER BY MIN(created_at), account_id`)
	if err != nil {
		return nil, fmt.Errorf("list seen task accounts: %w", err)
	}
	defer rows.Close()

	var accountIDs []int64
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			return nil, fmt.Errorf("scan seen task account: %w", err)
		}
		if accountID < 1 {
			return nil, fmt.Errorf("scan seen task account: invalid account ID %d", accountID)
		}
		accountIDs = append(accountIDs, accountID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate seen task accounts: %w", err)
	}
	return accountIDs, nil
}

// ListSeenTasks returns a bounded task batch for exactly one account.
func (s *Store) ListSeenTasks(ctx context.Context, accountID int64, limit int) ([]domain.SeenTask, error) {
	if accountID < 1 {
		return nil, fmt.Errorf("list seen tasks: account ID must be positive")
	}
	if limit < 1 {
		return nil, fmt.Errorf("list seen tasks: limit must be positive")
	}
	rows, err := s.queryContext(ctx, `
		SELECT account_id, uid_validity, uid, created_at
		FROM imap_seen_tasks
		WHERE account_id = ?
		ORDER BY created_at, uid_validity, uid
		LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("list seen tasks: %w", err)
	}
	return scanSeenTasks(rows, "account")
}

func scanSeenTasks(rows *sql.Rows, operation string) ([]domain.SeenTask, error) {
	defer rows.Close()
	var tasks []domain.SeenTask
	for rows.Next() {
		var task domain.SeenTask
		var uidValidity, uid, createdAt int64
		if err := rows.Scan(&task.AccountID, &uidValidity, &uid, &createdAt); err != nil {
			return nil, fmt.Errorf("scan %s seen task: %w", operation, err)
		}
		if task.AccountID < 1 || uidValidity < 1 || uidValidity > int64(^uint32(0)) ||
			uid < 1 || uid > int64(^uint32(0)) {
			return nil, fmt.Errorf(
				"scan %s seen task: invalid position account=%d UIDVALIDITY=%d UID=%d",
				operation, task.AccountID, uidValidity, uid,
			)
		}
		task.UIDValidity = uint32(uidValidity)
		task.UID = uint32(uid)
		task.CreatedAt = timeFromTimestamp(createdAt)
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s seen tasks: %w", operation, err)
	}
	return tasks, nil
}

// DeleteSeenTasks completes or discards exact tasks. Including every identity
// component prevents an older worker from deleting work for another account or
// mailbox generation.
func (s *Store) DeleteSeenTasks(
	ctx context.Context,
	accountID int64,
	uidValidity uint32,
	uids []uint32,
) error {
	if accountID < 1 {
		return fmt.Errorf("delete seen tasks: account ID must be positive")
	}
	if uidValidity == 0 {
		return fmt.Errorf("delete seen tasks: UIDVALIDITY must be positive")
	}
	if len(uids) == 0 {
		return nil
	}

	unique := make([]uint32, 0, len(uids))
	seen := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		if uid == 0 {
			return fmt.Errorf("delete seen tasks: UID must be positive")
		}
		if _, exists := seen[uid]; exists {
			continue
		}
		seen[uid] = struct{}{}
		unique = append(unique, uid)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i] < unique[j] })

	placeholders := make([]string, len(unique))
	args := make([]any, 0, len(unique)+2)
	args = append(args, accountID, int64(uidValidity))
	for index, uid := range unique {
		placeholders[index] = "?"
		args = append(args, int64(uid))
	}
	if _, err := s.execContext(ctx, `
		DELETE FROM imap_seen_tasks
		WHERE account_id = ? AND uid_validity = ?
		  AND uid IN (`+strings.Join(placeholders, ", ")+`)`, args...); err != nil {
		return fmt.Errorf("delete seen tasks: %w", err)
	}
	return nil
}

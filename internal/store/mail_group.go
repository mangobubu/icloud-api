package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
	"icloud-api/internal/domain"
)

const maxMailGroupNameRunes = 100

// CreateMailGroup adds one global administrator-defined mailbox group.
func (s *Store) CreateMailGroup(ctx context.Context, name string) (domain.MailGroup, error) {
	name, err := normalizeMailGroupName(name)
	if err != nil {
		return domain.MailGroup{}, err
	}
	now := s.now().UTC()
	_, err = s.execContext(ctx, `
		INSERT INTO mail_groups(name, name_key, created_at, updated_at)
		VALUES(?, ?, ?, ?)`, name, mailGroupNameKey(name), timestamp(now), timestamp(now))
	if err != nil {
		if isUniqueConstraintError(err) {
			return domain.MailGroup{}, ErrMailGroupNameExists
		}
		return domain.MailGroup{}, fmt.Errorf("create mail group: %w", err)
	}
	// Read by the unique name so this remains dialect-neutral; PostgreSQL
	// drivers intentionally do not expose LastInsertId.
	return s.GetMailGroupByName(ctx, name)
}

func normalizeMailGroupName(name string) (string, error) {
	name = strings.TrimSpace(sanitizePostgresText(name))
	if name == "" {
		return "", ErrMailGroupNameRequired
	}
	if utf8.RuneCountInString(name) > maxMailGroupNameRunes {
		return "", fmt.Errorf("%w: maximum %d characters", ErrMailGroupNameTooLong, maxMailGroupNameRunes)
	}
	return name, nil
}

// mailGroupNameKey gives every database dialect the same Unicode-aware
// uniqueness semantics. Keep the display name untouched and use this value
// only for comparisons and constraints.
func mailGroupNameKey(name string) string {
	name = strings.TrimSpace(sanitizePostgresText(name))
	return strings.ToLower(norm.NFKC.String(name))
}

// GetMailGroup returns one group and its current alias count.
func (s *Store) GetMailGroup(ctx context.Context, id int64) (domain.MailGroup, error) {
	if id < 1 {
		return domain.MailGroup{}, ErrNotFound
	}
	return scanMailGroup(s.queryRowContext(ctx, `
		SELECT g.id, g.name, COUNT(a.id), g.created_at, g.updated_at
		FROM mail_groups g
		LEFT JOIN aliases a ON a.group_id = g.id
		WHERE g.id = ?
		GROUP BY g.id, g.name, g.created_at, g.updated_at`, id))
}

// GetMailGroupByName is used after inserts on drivers that do not expose an
// auto-generated LastInsertId.
func (s *Store) GetMailGroupByName(ctx context.Context, name string) (domain.MailGroup, error) {
	return scanMailGroup(s.queryRowContext(ctx, `
		SELECT g.id, g.name, COUNT(a.id), g.created_at, g.updated_at
		FROM mail_groups g
		LEFT JOIN aliases a ON a.group_id = g.id
		WHERE g.name_key = ?
		GROUP BY g.id, g.name, g.created_at, g.updated_at`, mailGroupNameKey(name)))
}

// ListMailGroups returns groups ordered by name for stable selector rendering.
func (s *Store) ListMailGroups(ctx context.Context) ([]domain.MailGroup, error) {
	rows, err := s.queryContext(ctx, `
		SELECT g.id, g.name, COUNT(a.id), g.created_at, g.updated_at
		FROM mail_groups g
		LEFT JOIN aliases a ON a.group_id = g.id
		GROUP BY g.id, g.name, g.created_at, g.updated_at
		ORDER BY LOWER(g.name), g.id`)
	if err != nil {
		return nil, fmt.Errorf("list mail groups: %w", err)
	}
	defer rows.Close()
	groups := make([]domain.MailGroup, 0)
	for rows.Next() {
		group, scanErr := scanMailGroup(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mail groups: %w", err)
	}
	return groups, nil
}

// UpdateMailGroup renames an existing group without changing its membership.
func (s *Store) UpdateMailGroup(ctx context.Context, id int64, name string) (domain.MailGroup, error) {
	if id < 1 {
		return domain.MailGroup{}, ErrNotFound
	}
	name, err := normalizeMailGroupName(name)
	if err != nil {
		return domain.MailGroup{}, err
	}
	result, err := s.execContext(ctx, `
		UPDATE mail_groups SET name = ?, name_key = ?, updated_at = ? WHERE id = ?`,
		name, mailGroupNameKey(name), timestamp(s.now().UTC()), id)
	if err != nil {
		if isUniqueConstraintError(err) {
			return domain.MailGroup{}, ErrMailGroupNameExists
		}
		return domain.MailGroup{}, fmt.Errorf("update mail group: %w", err)
	}
	if err := requireAffected(result, "mail group"); err != nil {
		return domain.MailGroup{}, err
	}
	return s.GetMailGroup(ctx, id)
}

// DeleteMailGroup removes a group and leaves its aliases ungrouped.
func (s *Store) DeleteMailGroup(ctx context.Context, id int64) error {
	if id < 1 {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin mail group deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.dialect == dialectSQLite {
		result, lockErr := s.txExecContext(ctx, tx,
			`UPDATE mail_groups SET name_key = name_key WHERE id = ?`, id)
		if lockErr != nil {
			return fmt.Errorf("lock mail group for deletion: %w", lockErr)
		}
		affected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("read mail group deletion lock result: %w", rowsErr)
		}
		if affected == 0 {
			return ErrNotFound
		}
	} else {
		var exists int
		if lockErr := s.txQueryRowContext(ctx, tx,
			`SELECT 1 FROM mail_groups WHERE id = ? FOR UPDATE`, id,
		).Scan(&exists); lockErr != nil {
			if errors.Is(lockErr, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock mail group for deletion: %w", lockErr)
		}
	}
	if err := s.lockMailGroupAliasesForDelete(ctx, tx, id); err != nil {
		return err
	}
	if _, err := s.txExecContext(ctx, tx,
		`UPDATE aliases SET group_id = NULL, updated_at = ? WHERE group_id = ?`,
		timestamp(s.now().UTC()), id); err != nil {
		return fmt.Errorf("ungroup aliases before group deletion: %w", err)
	}
	result, err := s.txExecContext(ctx, tx, `DELETE FROM mail_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete mail group: %w", err)
	}
	if err := requireAffected(result, "mail group"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mail group deletion: %w", err)
	}
	return nil
}

// SetAliasGroup moves one alias to a group. A nil group ID explicitly clears
// the membership and is the representation used by the ungroup action.
func (s *Store) SetAliasGroup(ctx context.Context, aliasID int64, groupID *int64) (domain.Alias, error) {
	if aliasID < 1 {
		return domain.Alias{}, ErrNotFound
	}
	if groupID != nil && *groupID < 1 {
		return domain.Alias{}, fmt.Errorf("set alias group: group ID must be positive")
	}
	var value any
	if groupID != nil {
		value = *groupID
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return domain.Alias{}, fmt.Errorf("begin alias group move: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if groupID != nil {
		if err := s.lockMailGroupForMove(ctx, tx, *groupID); err != nil {
			return domain.Alias{}, err
		}
	}
	result, err := s.txExecContext(ctx, tx, `
		UPDATE aliases SET group_id = ?, updated_at = ? WHERE id = ?`,
		value, timestamp(s.now().UTC()), aliasID)
	if err != nil {
		return domain.Alias{}, fmt.Errorf("set alias group: %w", err)
	}
	if err := requireAffected(result, "alias"); err != nil {
		return domain.Alias{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Alias{}, fmt.Errorf("commit alias group move: %w", err)
	}
	return s.GetAlias(ctx, aliasID)
}

// SetAliasesGroup moves a deduplicated set of aliases atomically.
func (s *Store) SetAliasesGroup(ctx context.Context, aliasIDs []int64, groupID *int64) error {
	if len(aliasIDs) == 0 {
		return errors.New("set aliases group: at least one alias is required")
	}
	if groupID != nil && *groupID < 1 {
		return fmt.Errorf("set aliases group: group ID must be positive")
	}
	seen := make(map[int64]struct{}, len(aliasIDs))
	ids := make([]int64, 0, len(aliasIDs))
	for _, id := range aliasIDs {
		if id < 1 {
			return fmt.Errorf("set aliases group: alias ID must be positive")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	placeholders := make([]string, len(ids))
	idArgs := make([]any, 0, len(ids))
	for index, id := range ids {
		placeholders[index] = "?"
		idArgs = append(idArgs, id)
	}
	var value any
	if groupID != nil {
		value = *groupID
	}
	args := append([]any{value, timestamp(s.now().UTC())}, idArgs...)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin aliases group move: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if groupID != nil {
		if err := s.lockMailGroupForMove(ctx, tx, *groupID); err != nil {
			return err
		}
	}
	if err := s.lockAliasesForBatchGroupMove(ctx, tx, placeholders, idArgs, len(ids)); err != nil {
		return err
	}
	query := `UPDATE aliases SET group_id = ?, updated_at = ? WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	result, err := s.txExecContext(ctx, tx, query, args...)
	if err != nil {
		return fmt.Errorf("move aliases to group: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read aliases group move result: %w", err)
	}
	if affected != int64(len(ids)) {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit aliases group move: %w", err)
	}
	return nil
}

func (s *Store) lockMailGroupAliasesForDelete(ctx context.Context, tx *sql.Tx, groupID int64) error {
	if s.dialect != dialectPostgres {
		return nil
	}
	rows, err := s.txQueryContext(ctx, tx,
		`SELECT id FROM aliases WHERE group_id = ? ORDER BY id FOR UPDATE`, groupID)
	if err != nil {
		return fmt.Errorf("lock mail group aliases for deletion: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var aliasID int64
		if err := rows.Scan(&aliasID); err != nil {
			return fmt.Errorf("scan locked mail group alias: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate locked mail group aliases: %w", err)
	}
	return nil
}

func (s *Store) lockAliasesForBatchGroupMove(
	ctx context.Context,
	tx *sql.Tx,
	placeholders []string,
	idArgs []any,
	want int,
) error {
	if s.dialect != dialectPostgres {
		return nil
	}
	query := `SELECT id FROM aliases WHERE id IN (` + strings.Join(placeholders, ",") + `) ORDER BY id FOR UPDATE`
	rows, err := s.txQueryContext(ctx, tx, query, idArgs...)
	if err != nil {
		return fmt.Errorf("lock aliases for group move: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var aliasID int64
		if err := rows.Scan(&aliasID); err != nil {
			return fmt.Errorf("scan locked alias for group move: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate locked aliases for group move: %w", err)
	}
	if count != want {
		return ErrNotFound
	}
	return nil
}

// lockMailGroupForMove keeps the target alive until the alias update commits.
// PostgreSQL can lock the referenced key directly. SQLite deferred
// transactions need a harmless write first so a concurrent delete cannot slip
// between the existence check and the alias update.
func (s *Store) lockMailGroupForMove(ctx context.Context, tx *sql.Tx, groupID int64) error {
	if s.dialect == dialectSQLite {
		result, err := s.txExecContext(ctx, tx,
			`UPDATE mail_groups SET name_key = name_key WHERE id = ?`, groupID)
		if err != nil {
			return fmt.Errorf("lock mail group for move: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read mail group lock result: %w", err)
		}
		if affected == 0 {
			return ErrMailGroupNotFound
		}
		return nil
	}

	var exists int
	if err := s.txQueryRowContext(ctx, tx,
		`SELECT 1 FROM mail_groups WHERE id = ? FOR KEY SHARE`, groupID,
	).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMailGroupNotFound
		}
		return fmt.Errorf("lock mail group for move: %w", err)
	}
	return nil
}

func scanMailGroup(scanner rowScanner) (domain.MailGroup, error) {
	var group domain.MailGroup
	var createdAt, updatedAt int64
	if err := scanner.Scan(&group.ID, &group.Name, &group.AliasCount, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.MailGroup{}, ErrNotFound
		}
		return domain.MailGroup{}, fmt.Errorf("scan mail group: %w", err)
	}
	group.CreatedAt = timeFromTimestamp(createdAt)
	group.UpdatedAt = timeFromTimestamp(updatedAt)
	return group, nil
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "duplicate key") ||
		strings.Contains(message, "unique violation")
}

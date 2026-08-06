package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"icloud-api/internal/domain"
)

const latestMessageColumns = `
	alias_id, uid_validity, uid, message_id, internal_date, header_date,
	from_json, to_json, cc_json, subject, text_body, html_body,
	attachments_json, body_truncated, synced_at`

// UpsertLatestMessage inserts or refreshes an alias's single message row. The
// UIDVALIDITY/UID guard makes delayed work from the same mailbox generation
// unable to replace a newer UID. A different UIDVALIDITY always represents a
// new generation (its numeric value is opaque). Reprocessing the same UID is
// allowed so a parser improvement can refresh its content.
func (s *Store) UpsertLatestMessage(ctx context.Context, message domain.LatestMessage) (bool, error) {
	if err := validateLatestMessagePosition(message); err != nil {
		return false, err
	}
	fromJSON, err := marshalJSONList(message.From)
	if err != nil {
		return false, fmt.Errorf("encode message from addresses: %w", err)
	}
	toJSON, err := marshalJSONList(message.To)
	if err != nil {
		return false, fmt.Errorf("encode message to addresses: %w", err)
	}
	ccJSON, err := marshalJSONList(message.CC)
	if err != nil {
		return false, fmt.Errorf("encode message cc addresses: %w", err)
	}
	attachmentsJSON, err := marshalJSONList(message.Attachments)
	if err != nil {
		return false, fmt.Errorf("encode message attachments: %w", err)
	}
	if message.SyncedAt.IsZero() {
		message.SyncedAt = s.now()
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin latest message upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO latest_messages(
			alias_id, uid_validity, uid, message_id, internal_date, header_date,
			from_json, to_json, cc_json, subject, text_body, html_body,
			attachments_json, body_truncated, synced_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(alias_id) DO UPDATE SET
			uid_validity = excluded.uid_validity,
			uid = excluded.uid,
			message_id = excluded.message_id,
			internal_date = excluded.internal_date,
			header_date = excluded.header_date,
			from_json = excluded.from_json,
			to_json = excluded.to_json,
			cc_json = excluded.cc_json,
			subject = excluded.subject,
			text_body = excluded.text_body,
			html_body = excluded.html_body,
			attachments_json = excluded.attachments_json,
			body_truncated = excluded.body_truncated,
			synced_at = excluded.synced_at
		WHERE excluded.uid_validity != latest_messages.uid_validity
		   OR (excluded.uid_validity = latest_messages.uid_validity
		       AND excluded.uid >= latest_messages.uid)`,
		message.AliasID, int64(message.UIDValidity), int64(message.UID), message.MessageID,
		timestamp(message.InternalDate), nullableTimestamp(message.HeaderDate),
		fromJSON, toJSON, ccJSON, message.Subject, message.TextBody, message.HTMLBody,
		attachmentsJSON, message.BodyTruncated, timestamp(message.SyncedAt),
	)
	if err != nil {
		return false, fmt.Errorf("upsert latest message: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read latest message upsert result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit latest message upsert: %w", err)
	}
	return changed > 0, nil
}

// UpsertLatest is kept as the concise form used by sync workers.
func (s *Store) UpsertLatest(ctx context.Context, message domain.LatestMessage) (bool, error) {
	return s.UpsertLatestMessage(ctx, message)
}

// ReplaceLatestMessage applies an authoritative IMAP snapshot. Unlike
// UpsertLatestMessage, it allows the current highest UID to move backwards
// after the newer message was deleted from the mailbox.
func (s *Store) ReplaceLatestMessage(ctx context.Context, message domain.LatestMessage) error {
	if err := validateLatestMessagePosition(message); err != nil {
		return err
	}
	fromJSON, err := marshalJSONList(message.From)
	if err != nil {
		return fmt.Errorf("encode message from addresses: %w", err)
	}
	toJSON, err := marshalJSONList(message.To)
	if err != nil {
		return fmt.Errorf("encode message to addresses: %w", err)
	}
	ccJSON, err := marshalJSONList(message.CC)
	if err != nil {
		return fmt.Errorf("encode message cc addresses: %w", err)
	}
	attachmentsJSON, err := marshalJSONList(message.Attachments)
	if err != nil {
		return fmt.Errorf("encode message attachments: %w", err)
	}
	if message.SyncedAt.IsZero() {
		message.SyncedAt = s.now()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO latest_messages(
			alias_id, uid_validity, uid, message_id, internal_date, header_date,
			from_json, to_json, cc_json, subject, text_body, html_body,
			attachments_json, body_truncated, synced_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(alias_id) DO UPDATE SET
			uid_validity = excluded.uid_validity,
			uid = excluded.uid,
			message_id = excluded.message_id,
			internal_date = excluded.internal_date,
			header_date = excluded.header_date,
			from_json = excluded.from_json,
			to_json = excluded.to_json,
			cc_json = excluded.cc_json,
			subject = excluded.subject,
			text_body = excluded.text_body,
			html_body = excluded.html_body,
			attachments_json = excluded.attachments_json,
			body_truncated = excluded.body_truncated,
			synced_at = excluded.synced_at`,
		message.AliasID, int64(message.UIDValidity), int64(message.UID), message.MessageID,
		timestamp(message.InternalDate), nullableTimestamp(message.HeaderDate),
		fromJSON, toJSON, ccJSON, message.Subject, message.TextBody, message.HTMLBody,
		attachmentsJSON, message.BodyTruncated, timestamp(message.SyncedAt),
	)
	if err != nil {
		return fmt.Errorf("replace latest message: %w", err)
	}
	return nil
}

func validateLatestMessagePosition(message domain.LatestMessage) error {
	if message.AliasID < 1 {
		return fmt.Errorf("latest message alias ID must be positive")
	}
	if message.UIDValidity == 0 {
		return fmt.Errorf("latest message UIDVALIDITY must be positive")
	}
	if message.UID == 0 {
		return fmt.Errorf("latest message UID must be positive")
	}
	return nil
}

func (s *Store) GetLatestMessage(ctx context.Context, aliasID int64) (domain.LatestMessage, error) {
	return scanLatestMessage(s.db.QueryRowContext(ctx,
		`SELECT `+latestMessageColumns+` FROM latest_messages WHERE alias_id = ?`, aliasID,
	))
}

func (s *Store) DeleteLatestMessage(ctx context.Context, aliasID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM latest_messages WHERE alias_id = ?`, aliasID); err != nil {
		return fmt.Errorf("delete latest message: %w", err)
	}
	return nil
}

// DeleteLatestMessageFromOtherUIDValidity clears a snapshot from an obsolete
// IMAP mailbox generation while retaining an uncertain current-generation row.
func (s *Store) DeleteLatestMessageFromOtherUIDValidity(ctx context.Context, aliasID int64, uidValidity uint32) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM latest_messages WHERE alias_id = ? AND uid_validity != ?`,
		aliasID, int64(uidValidity),
	); err != nil {
		return fmt.Errorf("delete obsolete latest message: %w", err)
	}
	return nil
}

func scanLatestMessage(scanner rowScanner) (domain.LatestMessage, error) {
	var message domain.LatestMessage
	var uidValidity, uid int64
	var internalDate, syncedAt int64
	var headerDate sql.NullInt64
	var fromJSON, toJSON, ccJSON, attachmentsJSON string
	var truncated int
	err := scanner.Scan(
		&message.AliasID, &uidValidity, &uid, &message.MessageID,
		&internalDate, &headerDate, &fromJSON, &toJSON, &ccJSON,
		&message.Subject, &message.TextBody, &message.HTMLBody,
		&attachmentsJSON, &truncated, &syncedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.LatestMessage{}, ErrNotFound
		}
		return domain.LatestMessage{}, fmt.Errorf("scan latest message: %w", err)
	}
	message.UIDValidity = uint32(uidValidity)
	message.UID = uint32(uid)
	message.InternalDate = timeFromTimestamp(internalDate)
	message.HeaderDate = timePtr(headerDate)
	message.BodyTruncated = truncated != 0
	message.SyncedAt = timeFromTimestamp(syncedAt)
	if err := unmarshalMessageJSON(&message, fromJSON, toJSON, ccJSON, attachmentsJSON); err != nil {
		return domain.LatestMessage{}, err
	}
	return message, nil
}

func marshalJSONList[T any](value []T) (string, error) {
	if value == nil {
		value = []T{}
	}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

func unmarshalMessageJSON(
	message *domain.LatestMessage,
	fromJSON, toJSON, ccJSON, attachmentsJSON string,
) error {
	for _, item := range []struct {
		name   string
		value  string
		target any
	}{
		{"from", fromJSON, &message.From},
		{"to", toJSON, &message.To},
		{"cc", ccJSON, &message.CC},
		{"attachments", attachmentsJSON, &message.Attachments},
	} {
		if err := json.Unmarshal([]byte(item.value), item.target); err != nil {
			return fmt.Errorf("decode message %s: %w", item.name, err)
		}
	}
	return nil
}

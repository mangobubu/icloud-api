package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"icloud-api/internal/domain"
)

const (
	archiveContentAvailable = domain.ArchiveContentAvailable
	archiveContentEvicted   = domain.ArchiveContentEvicted
	archiveContentMetadata  = domain.ArchiveContentMetadata
	archiveContentOversized = domain.ArchiveContentOversized
	archiveContentMissing   = domain.ArchiveContentMissing
	maxOTPHistory           = 100
)

type ArchiveStats struct {
	ContentBytes int64
	ContentLimit int64
	MessageCount int64
	EvictedCount int64
}

const archiveSourceResetPrefix = "source-reset-account-"

func (s *Store) lockMailArchiveAccount(accountID int64) func() {
	if s == nil || accountID < 1 {
		return func() {}
	}
	value, _ := s.mailArchiveAccountLocks.LoadOrStore(accountID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

type mailArchiveQuarantine struct {
	original string
	root     string
	archived string
}

func (s *Store) quarantineMailArchiveAccount(accountID int64) (mailArchiveQuarantine, error) {
	if s == nil || s.mailArchiveDir == "" || accountID < 1 {
		return mailArchiveQuarantine{}, nil
	}
	original := filepath.Join(s.mailArchiveDir, fmt.Sprintf("account-%d", accountID))
	info, err := os.Stat(original)
	if errors.Is(err, os.ErrNotExist) {
		return mailArchiveQuarantine{}, nil
	}
	if err != nil {
		return mailArchiveQuarantine{}, fmt.Errorf("inspect account mail archive: %w", err)
	}
	if !info.IsDir() {
		return mailArchiveQuarantine{}, errors.New("account mail archive path is not a directory")
	}
	root, err := os.MkdirTemp(
		s.MailArchiveTempDir(),
		fmt.Sprintf("%s%d-", archiveSourceResetPrefix, accountID),
	)
	if err != nil {
		return mailArchiveQuarantine{}, fmt.Errorf("create account archive quarantine: %w", err)
	}
	quarantine := mailArchiveQuarantine{
		original: original,
		root:     root,
		archived: filepath.Join(root, "archive"),
	}
	if err := os.Rename(original, quarantine.archived); err != nil {
		_ = os.Remove(root)
		return mailArchiveQuarantine{}, fmt.Errorf("quarantine account mail archive: %w", err)
	}
	return quarantine, nil
}

func (q mailArchiveQuarantine) restore() error {
	if q.archived == "" {
		return nil
	}
	if err := os.Rename(q.archived, q.original); err != nil {
		return fmt.Errorf("restore account mail archive: %w", err)
	}
	if err := os.Remove(q.root); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove account archive quarantine: %w", err)
	}
	return nil
}

func (q mailArchiveQuarantine) discard() {
	if q.root != "" {
		_ = os.RemoveAll(q.root)
	}
}

func (s *Store) ConfigureMailArchive(directory string, limit int64) error {
	directory = strings.TrimSpace(directory)
	if directory == "" || limit < 1 {
		return errors.New("mail archive directory and positive limit are required")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("resolve mail archive directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(abs, ".tmp"), 0o700); err != nil {
		return fmt.Errorf("create mail archive directory: %w", err)
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return fmt.Errorf("protect mail archive directory: %w", err)
	}
	s.mailArchiveDir = abs
	s.mailArchiveLimit = limit
	return nil
}

func (s *Store) MailArchiveTempDir() string {
	if s == nil || s.mailArchiveDir == "" {
		return ""
	}
	return filepath.Join(s.mailArchiveDir, ".tmp")
}

type stagedArchiveContent struct {
	path    string
	bytes   int64
	sha256  string
	created bool
}

func archiveMessageKey(message domain.ArchivedMessage) string {
	return fmt.Sprintf("%d/%d/%d", message.AccountID, message.UIDValidity, message.UID)
}

func (s *Store) stageArchiveMessages(messages []domain.ArchivedMessage) (_ map[string]stagedArchiveContent, returnedErr error) {
	staged := make(map[string]stagedArchiveContent)
	defer func() {
		if returnedErr != nil {
			s.cleanupPublishedArchive(staged)
		}
	}()
	if len(messages) == 0 {
		return staged, nil
	}
	if s.mailArchiveDir == "" {
		return nil, errors.New("mail archive is not configured")
	}
	for _, message := range messages {
		if message.ContentState == archiveContentOversized ||
			(len(message.RawMIME) == 0 && strings.TrimSpace(message.RawMIMEPath) == "") {
			continue
		}
		relative := filepath.Join(
			fmt.Sprintf("account-%d", message.AccountID),
			fmt.Sprintf("uidv-%d", message.UIDValidity),
			fmt.Sprintf("uid-%d.eml", message.UID),
		)
		finalPath := filepath.Join(s.mailArchiveDir, relative)
		if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
			return nil, fmt.Errorf("create archive message directory: %w", err)
		}
		var content stagedArchiveContent
		var err error
		if message.RawMIMEPath != "" {
			content, err = s.publishStagedArchiveFile(message, finalPath, relative)
		} else {
			content, err = s.publishArchiveBytes(message.RawMIME, finalPath, relative)
		}
		if err != nil {
			return nil, err
		}
		key := archiveMessageKey(message)
		content.created = content.created || staged[key].created
		staged[key] = content
	}
	return staged, nil
}

func (s *Store) publishArchiveBytes(raw []byte, finalPath, relative string) (stagedArchiveContent, error) {
	digest := sha256.Sum256(raw)
	digestText := hex.EncodeToString(digest[:])
	if existing, ok, err := matchingArchiveFile(finalPath, int64(len(raw)), digestText); err != nil {
		return stagedArchiveContent{}, err
	} else if ok {
		return stagedArchiveContent{path: filepath.ToSlash(relative), bytes: existing, sha256: digestText}, nil
	}
	temporary, err := os.CreateTemp(s.MailArchiveTempDir(), "message-*.tmp")
	if err != nil {
		return stagedArchiveContent{}, fmt.Errorf("create archive temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(raw)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return stagedArchiveContent{}, fmt.Errorf("write archive message: %w", err)
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return stagedArchiveContent{}, fmt.Errorf("publish archive message: %w", err)
	}
	removeTemporary = false
	return stagedArchiveContent{path: filepath.ToSlash(relative), bytes: int64(len(raw)), sha256: digestText, created: true}, nil
}

func (s *Store) publishStagedArchiveFile(message domain.ArchivedMessage, finalPath, relative string) (stagedArchiveContent, error) {
	source, err := filepath.Abs(strings.TrimSpace(message.RawMIMEPath))
	if err != nil {
		return stagedArchiveContent{}, fmt.Errorf("resolve staged archive message: %w", err)
	}
	temporaryRoot := filepath.Clean(s.MailArchiveTempDir())
	if source == temporaryRoot || !strings.HasPrefix(filepath.Clean(source), temporaryRoot+string(os.PathSeparator)) {
		return stagedArchiveContent{}, errors.New("staged archive message is outside the archive temporary directory")
	}
	info, err := os.Stat(source)
	if err != nil {
		return stagedArchiveContent{}, fmt.Errorf("read staged archive message: %w", err)
	}
	if !info.Mode().IsRegular() {
		return stagedArchiveContent{}, errors.New("staged archive message is not a regular file")
	}
	if message.RawSize > 0 && message.RawSize != info.Size() {
		return stagedArchiveContent{}, errors.New("staged archive message size changed")
	}
	digestText := strings.ToLower(strings.TrimSpace(message.RawSHA256))
	if !validSHA256Hex(digestText) {
		digestText, err = hashArchiveFile(source)
		if err != nil {
			return stagedArchiveContent{}, err
		}
	}
	if existing, ok, err := matchingArchiveFile(finalPath, info.Size(), digestText); err != nil {
		return stagedArchiveContent{}, err
	} else if ok {
		_ = os.Remove(source)
		return stagedArchiveContent{path: filepath.ToSlash(relative), bytes: existing, sha256: digestText}, nil
	}
	if err := os.Rename(source, finalPath); err != nil {
		return stagedArchiveContent{}, fmt.Errorf("publish staged archive message: %w", err)
	}
	return stagedArchiveContent{path: filepath.ToSlash(relative), bytes: info.Size(), sha256: digestText, created: true}, nil
}

func (s *Store) cleanupPublishedArchive(staged map[string]stagedArchiveContent) {
	for _, content := range staged {
		if !content.created {
			continue
		}
		if path, ok := s.safeArchivePath(content.path); ok {
			_ = os.Remove(path)
		}
	}
}

func (s *Store) cleanupArchiveInputs(messages []domain.ArchivedMessage) {
	temporaryRoot := filepath.Clean(s.MailArchiveTempDir())
	if temporaryRoot == "." || temporaryRoot == "" {
		return
	}
	for _, message := range messages {
		path, err := filepath.Abs(strings.TrimSpace(message.RawMIMEPath))
		if err != nil || path == temporaryRoot || !strings.HasPrefix(filepath.Clean(path), temporaryRoot+string(os.PathSeparator)) {
			continue
		}
		_ = os.Remove(path)
	}
}

func matchingArchiveFile(path string, size int64, digest string) (int64, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("inspect archived message: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return 0, false, errors.New("archived message identity conflicts with an existing file")
	}
	existingDigest, err := hashArchiveFile(path)
	if err != nil {
		return 0, false, err
	}
	if existingDigest != digest {
		return 0, false, errors.New("archived message checksum conflicts with an existing file")
	}
	return info.Size(), true, nil
}

func hashArchiveFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open archived message for checksum: %w", err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("checksum archived message: %w", err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (s *Store) persistArchivedMessagesTx(
	ctx context.Context,
	tx *sql.Tx,
	messages []domain.ArchivedMessage,
	staged map[string]stagedArchiveContent,
	syncedAt time.Time,
) error {
	for _, message := range messages {
		if message.AccountID < 1 || message.UIDValidity == 0 || message.UID == 0 || len(message.AliasIDs) == 0 {
			return errors.New("archive message identity is incomplete")
		}
		fromJSON, err := marshalJSONList(message.From)
		if err != nil {
			return fmt.Errorf("encode archived message senders: %w", err)
		}
		toJSON, err := marshalJSONList(message.To)
		if err != nil {
			return fmt.Errorf("encode archived message recipients: %w", err)
		}
		ccJSON, err := marshalJSONList(message.CC)
		if err != nil {
			return fmt.Errorf("encode archived message cc: %w", err)
		}
		contentState := archiveContentMetadata
		content := staged[archiveMessageKey(message)]
		if content.path != "" {
			contentState = archiveContentAvailable
		} else if message.ContentState == archiveContentOversized {
			contentState = archiveContentOversized
			content.bytes = message.RawSize
			content.sha256 = strings.ToLower(strings.TrimSpace(message.RawSHA256))
		} else if message.RawSize > 0 {
			content.bytes = message.RawSize
			content.sha256 = strings.ToLower(strings.TrimSpace(message.RawSHA256))
		}
		var archivedID int64
		err = s.txQueryRowContext(ctx, tx, `
			INSERT INTO archived_messages(
				account_id, uid_validity, upstream_uid, message_id, internal_date, header_date,
				from_json, to_json, cc_json, subject, content_path, content_bytes,
				content_sha256, content_state, body_truncated, synced_at, created_at
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(account_id, uid_validity, upstream_uid) DO UPDATE SET
				message_id = excluded.message_id,
				internal_date = excluded.internal_date,
				header_date = excluded.header_date,
				from_json = excluded.from_json,
				to_json = excluded.to_json,
				cc_json = excluded.cc_json,
				subject = excluded.subject,
				content_path = CASE WHEN excluded.content_state = 'available' THEN excluded.content_path ELSE archived_messages.content_path END,
				content_bytes = CASE WHEN excluded.content_state = 'available' OR archived_messages.content_bytes = 0 THEN excluded.content_bytes ELSE archived_messages.content_bytes END,
				content_sha256 = CASE WHEN excluded.content_state = 'available' OR archived_messages.content_sha256 = '' THEN excluded.content_sha256 ELSE archived_messages.content_sha256 END,
				content_state = CASE WHEN excluded.content_state = 'available' THEN excluded.content_state ELSE archived_messages.content_state END,
				body_truncated = excluded.body_truncated,
				synced_at = excluded.synced_at
			RETURNING id`,
			message.AccountID, int64(message.UIDValidity), int64(message.UID), sanitizePostgresText(message.MessageID),
			timestamp(message.InternalDate), nullableTimestamp(message.HeaderDate), fromJSON, toJSON, ccJSON,
			sanitizePostgresText(message.Subject), content.path, content.bytes, content.sha256, contentState,
			message.BodyTruncated, timestamp(syncedAt), timestamp(syncedAt),
		).Scan(&archivedID)
		if err != nil {
			return fmt.Errorf("upsert archived message: %w", err)
		}
		aliasIDs := append([]int64(nil), message.AliasIDs...)
		sort.Slice(aliasIDs, func(i, j int) bool { return aliasIDs[i] < aliasIDs[j] })
		for index, aliasID := range aliasIDs {
			if aliasID < 1 || index > 0 && aliasIDs[index-1] == aliasID {
				return errors.New("archive message contains invalid or duplicate alias ID")
			}
			var exists int
			err := s.txQueryRowContext(ctx, tx, `
				SELECT 1 FROM alias_messages WHERE alias_id = ? AND message_id = ?`, aliasID, archivedID,
			).Scan(&exists)
			if err == nil {
				continue
			}
			if err != sql.ErrNoRows {
				return fmt.Errorf("check archived alias message: %w", err)
			}
			var nextUID int64
			if err := s.txQueryRowContext(ctx, tx, `
				UPDATE aliases SET mailbox_uid_next = mailbox_uid_next + 1
				WHERE id = ? AND mailbox_uid_next < 4294967295
				RETURNING mailbox_uid_next - 1`, aliasID,
			).Scan(&nextUID); err != nil {
				return fmt.Errorf("allocate alias mailbox UID: %w", err)
			}
			if _, err := s.txExecContext(ctx, tx, `
				INSERT INTO alias_messages(alias_id, message_id, mailbox_uid, otp, created_at)
				VALUES(?, ?, ?, ?, ?)`,
				aliasID, archivedID, nextUID, strings.TrimSpace(message.OTP), timestamp(message.InternalDate),
			); err != nil {
				return fmt.Errorf("link archived message to alias: %w", err)
			}
			if strings.TrimSpace(message.OTP) != "" {
				if _, err := s.txExecContext(ctx, tx, `
					UPDATE alias_messages SET otp = ''
					WHERE alias_id = ? AND otp <> '' AND message_id NOT IN (
						SELECT kept.message_id FROM alias_messages kept
						JOIN archived_messages archived ON archived.id = kept.message_id
						WHERE kept.alias_id = ? AND kept.otp <> ''
						ORDER BY archived.internal_date DESC, archived.id DESC LIMIT ?
					)`, aliasID, aliasID, maxOTPHistory,
				); err != nil {
					return fmt.Errorf("prune alias OTP history: %w", err)
				}
			}
		}
	}
	return nil
}

func (s *Store) EnforceMailArchiveLimit(ctx context.Context) error {
	if s.mailArchiveDir == "" || s.mailArchiveLimit < 1 {
		return nil
	}
	for {
		var used int64
		if err := s.queryRowContext(ctx, `
			SELECT COALESCE(SUM(content_bytes), 0)
			FROM archived_messages WHERE content_state = 'available'`,
		).Scan(&used); err != nil {
			return fmt.Errorf("measure mail archive usage: %w", err)
		}
		if used <= s.mailArchiveLimit {
			return nil
		}
		var id int64
		var relativePath string
		if err := s.queryRowContext(ctx, `
			SELECT id, content_path FROM archived_messages
			WHERE content_state = 'available'
			ORDER BY internal_date, id LIMIT 1`,
		).Scan(&id, &relativePath); err != nil {
			return fmt.Errorf("select oldest archived content: %w", err)
		}
		path, ok := s.safeArchivePath(relativePath)
		if !ok {
			return fmt.Errorf("evict archived content %d: unsafe content path", id)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("evict archived content file %d: %w", id, err)
		}
		result, err := s.execContext(ctx, `
			UPDATE archived_messages
			SET content_path = '', content_state = 'evicted'
			WHERE id = ? AND content_state = 'available'`, id,
		)
		if err != nil {
			return fmt.Errorf("evict archived content metadata: %w", err)
		}
		changed, _ := result.RowsAffected()
		if changed == 0 {
			continue
		}
	}
}

// ReconcileMailArchive converts database rows whose published file vanished
// into metadata-only placeholders without discarding their original size or
// checksum. It is safe to run on every startup.
func (s *Store) ReconcileMailArchive(ctx context.Context) error {
	if err := s.reconcileMailArchiveQuarantines(ctx); err != nil {
		return err
	}
	rows, err := s.queryContext(ctx, `
		SELECT id, content_path, content_bytes
		FROM archived_messages WHERE content_state = 'available'
		ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list archived content for reconciliation: %w", err)
	}
	type candidate struct {
		id    int64
		path  string
		bytes int64
	}
	var missing []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.path, &item.bytes); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan archived content for reconciliation: %w", err)
		}
		path, ok := s.safeArchivePath(item.path)
		info, statErr := os.Stat(path)
		if !ok || statErr != nil || !info.Mode().IsRegular() || info.Size() != item.bytes {
			missing = append(missing, item)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close archived content reconciliation rows: %w", err)
	}
	for _, item := range missing {
		if _, err := s.execContext(ctx, `
			UPDATE archived_messages SET content_path = '', content_state = 'missing'
			WHERE id = ? AND content_state = 'available'`, item.id); err != nil {
			return fmt.Errorf("mark missing archived content: %w", err)
		}
	}
	return nil
}

func (s *Store) reconcileMailArchiveQuarantines(ctx context.Context) error {
	if s == nil || s.mailArchiveDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.MailArchiveTempDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list account archive quarantines: %w", err)
	}
	for _, entry := range entries {
		accountID, ok := archiveQuarantineAccountID(entry.Name())
		if !ok || !entry.IsDir() {
			continue
		}
		unlockArchive := s.lockMailArchiveAccount(accountID)
		root := filepath.Join(s.MailArchiveTempDir(), entry.Name())
		archived := filepath.Join(root, "archive")
		original := filepath.Join(s.mailArchiveDir, fmt.Sprintf("account-%d", accountID))

		var archivedRows int
		queryErr := s.queryRowContext(ctx,
			`SELECT COUNT(*) FROM archived_messages WHERE account_id = ?`, accountID,
		).Scan(&archivedRows)
		if queryErr != nil {
			unlockArchive()
			return fmt.Errorf("inspect quarantined account archive metadata: %w", queryErr)
		}
		originalInfo, statErr := os.Stat(original)
		switch {
		case statErr == nil:
			if !originalInfo.IsDir() {
				unlockArchive()
				return errors.New("account mail archive path is not a directory")
			}
			// A live directory means a committed source generation has already
			// published content. The quarantine is necessarily obsolete.
			if err := os.RemoveAll(root); err != nil {
				unlockArchive()
				return fmt.Errorf("discard obsolete account archive quarantine: %w", err)
			}
		case errors.Is(statErr, os.ErrNotExist) && archivedRows == 0:
			// The database-side reset committed before the process stopped.
			if err := os.RemoveAll(root); err != nil {
				unlockArchive()
				return fmt.Errorf("discard committed account archive quarantine: %w", err)
			}
		case errors.Is(statErr, os.ErrNotExist):
			// Metadata still references the previous source, so the SQL
			// transaction rolled back and its files must be restored.
			if err := os.Rename(archived, original); err != nil {
				unlockArchive()
				return fmt.Errorf("restore interrupted account archive quarantine: %w", err)
			}
			if err := os.Remove(root); err != nil && !errors.Is(err, os.ErrNotExist) {
				unlockArchive()
				return fmt.Errorf("remove restored account archive quarantine: %w", err)
			}
		default:
			unlockArchive()
			return fmt.Errorf("inspect account mail archive during reconciliation: %w", statErr)
		}
		unlockArchive()
	}
	return nil
}

func archiveQuarantineAccountID(name string) (int64, bool) {
	remainder := strings.TrimPrefix(name, archiveSourceResetPrefix)
	if remainder == name {
		return 0, false
	}
	idText, _, ok := strings.Cut(remainder, "-")
	if !ok {
		return 0, false
	}
	accountID, err := strconv.ParseInt(idText, 10, 64)
	return accountID, err == nil && accountID > 0
}

func (s *Store) safeArchivePath(relative string) (string, bool) {
	if s.mailArchiveDir == "" || relative == "" || filepath.IsAbs(relative) {
		return "", false
	}
	root := filepath.Clean(s.mailArchiveDir)
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(relative)))
	separator := string(os.PathSeparator)
	if path != root && !strings.HasPrefix(path, root+separator) {
		return "", false
	}
	return path, true
}

func (s *Store) ReadArchivedContent(message domain.ArchivedMailboxMessage) ([]byte, error) {
	file, err := s.OpenArchivedContent(message)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func (s *Store) OpenArchivedContent(message domain.ArchivedMailboxMessage) (*os.File, error) {
	path, ok := s.safeArchivePath(message.ContentPath)
	if !ok || message.ContentState != archiveContentAvailable {
		return nil, ErrNotFound
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.markArchivedContentMissing(message.ID)
			return nil, ErrNotFound
		}
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != message.ContentBytes {
		_ = file.Close()
		s.markArchivedContentMissing(message.ID)
		return nil, errors.New("archived message content size mismatch")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil || hex.EncodeToString(digest.Sum(nil)) != message.ContentSHA256 {
		_ = file.Close()
		s.markArchivedContentMissing(message.ID)
		return nil, errors.New("archived message content checksum mismatch")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (s *Store) markArchivedContentMissing(id int64) {
	if id < 1 {
		return
	}
	_, _ = s.execContext(context.Background(), `
		UPDATE archived_messages SET content_path = '', content_state = 'missing'
		WHERE id = ? AND content_state = 'available'`, id)
}

const archivedMailboxColumns = `
	m.id, am.alias_id, am.mailbox_uid, al.mailbox_uid_validity,
	m.message_id, m.internal_date, m.header_date, m.from_json, m.to_json, m.cc_json,
	m.subject, m.content_path, m.content_bytes, m.content_sha256, m.content_state,
	am.otp, m.body_truncated, m.created_at`

func (s *Store) ListArchivedMailboxMessages(ctx context.Context, aliasID int64) ([]domain.ArchivedMailboxMessage, error) {
	rows, err := s.queryContext(ctx, `
		SELECT `+archivedMailboxColumns+`
		FROM alias_messages am
		JOIN aliases al ON al.id = am.alias_id
		JOIN archived_messages m ON m.id = am.message_id
		WHERE am.alias_id = ? ORDER BY am.mailbox_uid`, aliasID,
	)
	if err != nil {
		return nil, fmt.Errorf("list archived mailbox messages: %w", err)
	}
	defer rows.Close()
	var result []domain.ArchivedMailboxMessage
	for rows.Next() {
		message, err := scanArchivedMailboxMessage(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, message)
	}
	return result, rows.Err()
}

func (s *Store) GetArchivedMailboxMessage(ctx context.Context, aliasID int64, mailboxUID uint32) (domain.ArchivedMailboxMessage, error) {
	return scanArchivedMailboxMessage(s.queryRowContext(ctx, `
		SELECT `+archivedMailboxColumns+`
		FROM alias_messages am
		JOIN aliases al ON al.id = am.alias_id
		JOIN archived_messages m ON m.id = am.message_id
		WHERE am.alias_id = ? AND am.mailbox_uid = ?`, aliasID, int64(mailboxUID),
	))
}

func scanArchivedMailboxMessage(scanner rowScanner) (domain.ArchivedMailboxMessage, error) {
	var message domain.ArchivedMailboxMessage
	var mailboxUID, uidValidity int64
	var internalDate, createdAt int64
	var headerDate sql.NullInt64
	var fromJSON, toJSON, ccJSON string
	if err := scanner.Scan(
		&message.ID, &message.AliasID, &mailboxUID, &uidValidity,
		&message.MessageID, &internalDate, &headerDate, &fromJSON, &toJSON, &ccJSON,
		&message.Subject, &message.ContentPath, &message.ContentBytes, &message.ContentSHA256,
		&message.ContentState, &message.OTP, &message.BodyTruncated, &createdAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return domain.ArchivedMailboxMessage{}, ErrNotFound
		}
		return domain.ArchivedMailboxMessage{}, fmt.Errorf("scan archived mailbox message: %w", err)
	}
	if mailboxUID < 1 || mailboxUID > int64(^uint32(0)) || uidValidity < 1 || uidValidity > int64(^uint32(0)) {
		return domain.ArchivedMailboxMessage{}, errors.New("archived mailbox UID state is invalid")
	}
	message.MailboxUID = uint32(mailboxUID)
	message.UIDValidity = uint32(uidValidity)
	message.InternalDate = timeFromTimestamp(internalDate)
	message.HeaderDate = timePtr(headerDate)
	message.ArchivedAt = timeFromTimestamp(createdAt)
	if err := json.Unmarshal([]byte(fromJSON), &message.From); err != nil {
		return domain.ArchivedMailboxMessage{}, err
	}
	if err := json.Unmarshal([]byte(toJSON), &message.To); err != nil {
		return domain.ArchivedMailboxMessage{}, err
	}
	if err := json.Unmarshal([]byte(ccJSON), &message.CC); err != nil {
		return domain.ArchivedMailboxMessage{}, err
	}
	return message, nil
}

func (s *Store) ListAliasOTPs(ctx context.Context, aliasID int64, limit int) ([]domain.OTPRecord, error) {
	if limit < 1 || limit > maxOTPHistory {
		limit = maxOTPHistory
	}
	rows, err := s.queryContext(ctx, `
		SELECT am.otp, m.internal_date
		FROM alias_messages am
		JOIN archived_messages m ON m.id = am.message_id
		WHERE am.alias_id = ? AND am.otp <> ''
		ORDER BY m.internal_date DESC, m.id DESC LIMIT ?`, aliasID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list alias OTP history: %w", err)
	}
	defer rows.Close()
	result := make([]domain.OTPRecord, 0, limit)
	for rows.Next() {
		var record domain.OTPRecord
		var timestampValue int64
		if err := rows.Scan(&record.OTP, &timestampValue); err != nil {
			return nil, fmt.Errorf("scan alias OTP history: %w", err)
		}
		record.Time = timeFromTimestamp(timestampValue)
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *Store) MailArchiveStats(ctx context.Context) (ArchiveStats, error) {
	stats := ArchiveStats{ContentLimit: s.mailArchiveLimit}
	if err := s.queryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN content_state = 'available' THEN content_bytes ELSE 0 END), 0), COUNT(*),
		       COALESCE(SUM(CASE WHEN content_state = 'evicted' THEN 1 ELSE 0 END), 0)
		FROM archived_messages`,
	).Scan(&stats.ContentBytes, &stats.MessageCount, &stats.EvictedCount); err != nil {
		return ArchiveStats{}, fmt.Errorf("read mail archive stats: %w", err)
	}
	return stats, nil
}

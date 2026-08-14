package publicimap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	stdmail "net/mail"
	"os"
	"sort"
	"strings"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-sasl"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

type Repository interface {
	GetMailboxBindingByAddress(context.Context, string) (domain.MailboxBinding, error)
	GetMailboxBindingByIMAPPasswordHash(context.Context, []byte) (domain.MailboxBinding, error)
	ListArchivedMailboxMessages(context.Context, int64) ([]domain.ArchivedMailboxMessage, error)
	OpenArchivedContent(domain.ArchivedMailboxMessage) (*os.File, error)
}

type Session struct {
	repo   Repository
	cipher *secure.Cipher
	now    func() time.Time

	binding           domain.MailboxBinding
	credentialVersion int64
	selected          bool
	messages          []domain.ArchivedMailboxMessage
}

var _ imapserver.SessionSASL = (*Session)(nil)
var _ imapserver.SessionMove = (*Session)(nil)

func NewSession(repo Repository, cipher *secure.Cipher) *Session {
	return &Session{repo: repo, cipher: cipher, now: time.Now}
}

func (s *Session) Close() error {
	s.binding = domain.MailboxBinding{}
	s.credentialVersion = 0
	s.messages = nil
	s.selected = false
	return nil
}

func (s *Session) Login(username, password string) error {
	binding, err := s.repo.GetMailboxBindingByIMAPPasswordHash(context.Background(), secure.HashToken(password))
	if err != nil || !sameMailboxIdentity(binding, username) {
		return imapserver.ErrAuthFailed
	}
	s.binding = binding
	s.credentialVersion = binding.Alias.CredentialVersion
	return nil
}

func (s *Session) AuthenticateMechanisms() []string {
	return []string{sasl.Plain, "XOAUTH2"}
}

func (s *Session) Authenticate(mechanism string) (sasl.Server, error) {
	switch strings.ToUpper(strings.TrimSpace(mechanism)) {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			if identity != "" && !strings.EqualFold(identity, username) {
				return imapserver.ErrAuthFailed
			}
			return s.Login(username, password)
		}), nil
	case "XOAUTH2":
		return &xoauth2Server{authenticate: s.loginAccessToken}, nil
	default:
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: "SASL mechanism not supported"}
	}
}

func (s *Session) loginAccessToken(username, token string) error {
	binding, err := s.repo.GetMailboxBindingByAddress(context.Background(), domain.NormalizeEmail(username))
	if err != nil || !sameMailboxIdentity(binding, username) || s.cipher == nil {
		return imapserver.ErrAuthFailed
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	if !s.cipher.VerifyAliasAccessToken(
		token,
		binding.Alias.ID,
		binding.Alias.CredentialVersion,
		binding.Alias.RefreshTokenHash,
		now,
	) {
		return imapserver.ErrAuthFailed
	}
	s.binding = binding
	s.credentialVersion = binding.Alias.CredentialVersion
	return nil
}

func sameMailboxIdentity(binding domain.MailboxBinding, username string) bool {
	return binding.Alias.ID > 0 &&
		binding.Alias.CredentialMode == domain.AliasCredentialModeV2 &&
		binding.Alias.CredentialVersion > 0 && binding.Alias.Enabled && binding.Account.Enabled &&
		domain.NormalizeEmail(username) == domain.NormalizeEmail(binding.Alias.Address)
}

type xoauth2Server struct {
	done         bool
	authenticate func(string, string) error
}

func (server *xoauth2Server) Next(response []byte) ([]byte, bool, error) {
	if server.done {
		return nil, false, sasl.ErrUnexpectedClientResponse
	}
	if response == nil {
		return []byte{}, false, nil
	}
	server.done = true
	fields := strings.Split(string(response), "\x01")
	var username, authorization string
	for _, field := range fields {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "user":
			username = strings.TrimSpace(value)
		case "auth":
			authorization = strings.TrimSpace(value)
		}
	}
	scheme, token, found := strings.Cut(authorization, " ")
	if username == "" || !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return nil, true, imapserver.ErrAuthFailed
	}
	if err := server.authenticate(username, token); err != nil {
		return nil, true, err
	}
	return nil, true, nil
}

func (s *Session) Select(mailbox string, _ *imap.SelectOptions) (*imap.SelectData, error) {
	if err := s.refreshBinding(); err != nil {
		return nil, err
	}
	if !isInbox(mailbox) {
		return nil, noSuchMailbox()
	}
	messages, err := s.repo.ListArchivedMailboxMessages(context.Background(), s.binding.Alias.ID)
	if err != nil {
		return nil, err
	}
	s.messages = messages
	s.selected = true
	return &imap.SelectData{
		Flags:          []imap.Flag{imap.FlagSeen},
		PermanentFlags: []imap.Flag{},
		NumMessages:    uint32(len(messages)),
		UIDNext:        imap.UID(s.binding.Alias.MailboxUIDNext),
		UIDValidity:    s.binding.Alias.MailboxUIDValidity,
	}, nil
}

func (s *Session) Unselect() error {
	s.selected = false
	s.messages = nil
	return nil
}

func (s *Session) List(writer *imapserver.ListWriter, reference string, patterns []string, options *imap.ListOptions) error {
	if err := s.refreshBinding(); err != nil {
		return err
	}
	if len(patterns) == 0 {
		return writer.WriteList(&imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Delim: '/'})
	}
	for _, pattern := range patterns {
		if !imapserver.MatchList("INBOX", '/', reference, pattern) {
			continue
		}
		data := &imap.ListData{Mailbox: "INBOX", Delim: '/', Attrs: []imap.MailboxAttr{imap.MailboxAttrNoInferiors}}
		if options != nil && options.ReturnStatus != nil {
			status, err := s.Status("INBOX", options.ReturnStatus)
			if err != nil {
				return err
			}
			data.Status = status
		}
		return writer.WriteList(data)
	}
	return nil
}

func (s *Session) Status(mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	if !isInbox(mailbox) {
		return nil, noSuchMailbox()
	}
	if err := s.refreshBinding(); err != nil {
		return nil, err
	}
	messages, err := s.repo.ListArchivedMailboxMessages(context.Background(), s.binding.Alias.ID)
	if err != nil {
		return nil, err
	}
	data := &imap.StatusData{Mailbox: "INBOX"}
	if options == nil {
		return data, nil
	}
	count := uint32(len(messages))
	zero := uint32(0)
	if options.NumMessages {
		data.NumMessages = &count
	}
	if options.NumRecent {
		data.NumRecent = &zero
	}
	if options.UIDNext {
		data.UIDNext = imap.UID(s.binding.Alias.MailboxUIDNext)
	}
	if options.UIDValidity {
		data.UIDValidity = s.binding.Alias.MailboxUIDValidity
	}
	if options.NumUnseen {
		data.NumUnseen = &count
	}
	if options.NumDeleted {
		data.NumDeleted = &zero
	}
	if options.Size {
		var total int64
		for _, message := range messages {
			total += s.messageSize(message)
		}
		data.Size = &total
	}
	return data, nil
}

func (s *Session) Fetch(writer *imapserver.FetchWriter, numberSet imap.NumSet, options *imap.FetchOptions) error {
	if !s.selected {
		return errors.New("INBOX is not selected")
	}
	if err := s.refreshBinding(); err != nil {
		return err
	}
	for index, message := range s.messages {
		sequence := uint32(index + 1)
		if !numberSetContains(numberSet, sequence, imap.UID(message.MailboxUID)) {
			continue
		}
		response := writer.CreateMessage(sequence)
		response.WriteUID(imap.UID(message.MailboxUID))
		if options.Flags {
			response.WriteFlags([]imap.Flag{})
		}
		if options.InternalDate {
			response.WriteInternalDate(message.InternalDate)
		}
		if options.RFC822Size {
			response.WriteRFC822Size(s.messageSize(message))
		}
		if options.Envelope {
			response.WriteEnvelope(messageEnvelope(message))
		}
		if options.BodyStructure != nil {
			reader, _, err := s.openMessage(message)
			if err != nil {
				return err
			}
			response.WriteBodyStructure(imapserver.ExtractBodyStructure(reader))
			_ = reader.Close()
		}
		for _, section := range options.BodySection {
			if err := s.writeBodySection(response, message, section); err != nil {
				return err
			}
		}
		for _, section := range options.BinarySection {
			reader, _, err := s.openMessage(message)
			if err != nil {
				return err
			}
			content := imapserver.ExtractBinarySection(reader, section)
			_ = reader.Close()
			literal := response.WriteBinarySection(section, int64(len(content)))
			if _, err := literal.Write(content); err != nil {
				_ = literal.Close()
				return err
			}
			if err := literal.Close(); err != nil {
				return err
			}
		}
		for _, section := range options.BinarySectionSize {
			reader, _, err := s.openMessage(message)
			if err != nil {
				return err
			}
			size := imapserver.ExtractBinarySectionSize(reader, section)
			_ = reader.Close()
			response.WriteBinarySectionSize(section, size)
		}
		if err := response.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) writeBodySection(response *imapserver.FetchResponseWriter, message domain.ArchivedMailboxMessage, section *imap.FetchItemBodySection) error {
	reader, size, err := s.openMessage(message)
	if err != nil {
		return err
	}
	defer reader.Close()
	wholeMessage := len(section.Part) == 0 && section.Specifier == imap.PartSpecifierNone &&
		len(section.HeaderFields) == 0 && len(section.HeaderFieldsNot) == 0
	if wholeMessage {
		offset, length := int64(0), size
		if section.Partial != nil {
			offset = section.Partial.Offset
			if offset > size {
				offset = size
			}
			length = min(section.Partial.Size, size-offset)
		}
		if _, err := reader.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		literal := response.WriteBodySection(section, length)
		_, writeErr := io.CopyN(literal, reader, length)
		closeErr := literal.Close()
		return errors.Join(writeErr, closeErr)
	}
	content := imapserver.ExtractBodySection(reader, section)
	literal := response.WriteBodySection(section, int64(len(content)))
	if _, err := literal.Write(content); err != nil {
		_ = literal.Close()
		return err
	}
	return literal.Close()
}

func (s *Session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, _ *imap.SearchOptions) (*imap.SearchData, error) {
	if !s.selected {
		return nil, errors.New("INBOX is not selected")
	}
	if err := s.refreshBinding(); err != nil {
		return nil, err
	}
	var sequenceSet imap.SeqSet
	var uidSet imap.UIDSet
	data := &imap.SearchData{}
	for index, message := range s.messages {
		sequence := uint32(index + 1)
		matched, err := s.messageMatches(message, sequence, criteria)
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
		}
		var number uint32
		if kind == imapserver.NumKindUID {
			uidSet.AddNum(imap.UID(message.MailboxUID))
			number = message.MailboxUID
		} else {
			sequenceSet.AddNum(sequence)
			number = sequence
		}
		if data.Min == 0 || number < data.Min {
			data.Min = number
		}
		if number > data.Max {
			data.Max = number
		}
		data.Count++
	}
	if kind == imapserver.NumKindUID {
		data.All = uidSet
	} else {
		data.All = sequenceSet
	}
	return data, nil
}

func (s *Session) messageMatches(message domain.ArchivedMailboxMessage, sequence uint32, criteria *imap.SearchCriteria) (bool, error) {
	if criteria == nil {
		return true, nil
	}
	for _, set := range criteria.SeqNum {
		if !set.Contains(sequence) {
			return false, nil
		}
	}
	for _, set := range criteria.UID {
		if !set.Contains(imap.UID(message.MailboxUID)) {
			return false, nil
		}
	}
	if !dateMatches(message.InternalDate, criteria.Since, criteria.Before) {
		return false, nil
	}
	if (!criteria.SentSince.IsZero() || !criteria.SentBefore.IsZero()) &&
		(message.HeaderDate == nil || !dateMatches(*message.HeaderDate, criteria.SentSince, criteria.SentBefore)) {
		return false, nil
	}
	for _, flag := range criteria.Flag {
		if flag != "" {
			return false, nil
		}
	}
	if criteria.Larger != 0 && s.messageSize(message) <= criteria.Larger {
		return false, nil
	}
	if criteria.Smaller != 0 && s.messageSize(message) >= criteria.Smaller {
		return false, nil
	}
	for _, header := range criteria.Header {
		if !strings.Contains(strings.ToLower(messageHeaderValue(message, header.Key)), strings.ToLower(header.Value)) {
			return false, nil
		}
	}
	for _, pattern := range append(append([]string{}, criteria.Body...), criteria.Text...) {
		reader, _, err := s.openMessage(message)
		if err != nil {
			return false, err
		}
		matched, searchErr := readerContainsFold(reader, pattern)
		_ = reader.Close()
		if searchErr != nil || !matched {
			return false, searchErr
		}
	}
	for _, not := range criteria.Not {
		matched, err := s.messageMatches(message, sequence, &not)
		if err != nil || matched {
			return false, err
		}
	}
	for _, pair := range criteria.Or {
		left, err := s.messageMatches(message, sequence, &pair[0])
		if err != nil {
			return false, err
		}
		right, err := s.messageMatches(message, sequence, &pair[1])
		if err != nil || !left && !right {
			return false, err
		}
	}
	if criteria.ModSeq != nil {
		return false, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: "MODSEQ is not supported"}
	}
	return true, nil
}

func (s *Session) Poll(writer *imapserver.UpdateWriter, _ bool) error {
	if !s.selected {
		return nil
	}
	if err := s.refreshBinding(); err != nil {
		return err
	}
	messages, err := s.repo.ListArchivedMailboxMessages(context.Background(), s.binding.Alias.ID)
	if err != nil {
		return err
	}
	if len(messages) != len(s.messages) {
		if err := writer.WriteNumMessages(uint32(len(messages))); err != nil {
			return err
		}
	}
	s.messages = messages
	return nil
}

func (s *Session) Idle(writer *imapserver.UpdateWriter, stop <-chan struct{}) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			if err := s.Poll(writer, true); err != nil {
				return err
			}
		}
	}
}

func (s *Session) Create(string, *imap.CreateOptions) error { return readOnlyError() }
func (s *Session) Delete(string) error                      { return readOnlyError() }
func (s *Session) Rename(string, string, *imap.RenameOptions) error {
	return readOnlyError()
}
func (s *Session) Subscribe(string) error   { return readOnlyError() }
func (s *Session) Unsubscribe(string) error { return readOnlyError() }
func (s *Session) Append(string, imap.LiteralReader, *imap.AppendOptions) (*imap.AppendData, error) {
	return nil, readOnlyError()
}
func (s *Session) Store(*imapserver.FetchWriter, imap.NumSet, *imap.StoreFlags, *imap.StoreOptions) error {
	return readOnlyError()
}
func (s *Session) Copy(imap.NumSet, string) (*imap.CopyData, error) {
	return nil, readOnlyError()
}
func (s *Session) Move(*imapserver.MoveWriter, imap.NumSet, string) error {
	return readOnlyError()
}
func (s *Session) Expunge(*imapserver.ExpungeWriter, *imap.UIDSet) error {
	return readOnlyError()
}

func readOnlyError() error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCode("READ-ONLY"), Text: "Mailbox is read-only"}
}

func noSuchMailbox() error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "No such mailbox"}
}

func isInbox(value string) bool { return strings.EqualFold(strings.TrimSpace(value), "INBOX") }

func (s *Session) ensureAuthenticated() error {
	if s.binding.Alias.ID < 1 || s.credentialVersion < 1 {
		return imapserver.ErrAuthFailed
	}
	return nil
}

func (s *Session) refreshBinding() error {
	if err := s.ensureAuthenticated(); err != nil {
		return err
	}
	binding, err := s.repo.GetMailboxBindingByAddress(context.Background(), s.binding.Alias.Address)
	if err != nil ||
		binding.Alias.ID != s.binding.Alias.ID ||
		binding.Alias.AccountID != s.binding.Alias.AccountID ||
		binding.Account.ID != s.binding.Account.ID ||
		domain.NormalizeEmail(binding.Alias.Address) != domain.NormalizeEmail(s.binding.Alias.Address) ||
		binding.Alias.CredentialMode != domain.AliasCredentialModeV2 ||
		!binding.Alias.Enabled || !binding.Account.Enabled ||
		binding.Alias.CredentialVersion != s.credentialVersion {
		return imapserver.ErrAuthFailed
	}
	s.binding = binding
	return nil
}

func numberSetContains(set imap.NumSet, sequence uint32, uid imap.UID) bool {
	switch set := set.(type) {
	case imap.SeqSet:
		return set.Contains(sequence)
	case imap.UIDSet:
		return set.Contains(uid)
	default:
		return false
	}
}

type readSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

type bytesReadSeekCloser struct{ *bytes.Reader }

func (bytesReadSeekCloser) Close() error { return nil }

func (s *Session) openMessage(message domain.ArchivedMailboxMessage) (readSeekCloser, int64, error) {
	if message.ContentState == domain.ArchiveContentAvailable {
		file, err := s.repo.OpenArchivedContent(message)
		if err == nil {
			return file, message.ContentBytes, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			// A checksum or size mismatch is represented to clients as the same
			// explanatory placeholder used for a missing file.
		}
	}
	content := placeholderMIME(message, s.binding.Alias.Address)
	return bytesReadSeekCloser{bytes.NewReader(content)}, int64(len(content)), nil
}

func (s *Session) messageSize(message domain.ArchivedMailboxMessage) int64 {
	if message.ContentState == domain.ArchiveContentAvailable && message.ContentBytes > 0 {
		file, err := s.repo.OpenArchivedContent(message)
		if err == nil {
			_ = file.Close()
			return message.ContentBytes
		}
	}
	return int64(len(placeholderMIME(message, s.binding.Alias.Address)))
}

func placeholderMIME(message domain.ArchivedMailboxMessage, recipient string) []byte {
	subject := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(message.Subject, "\r", " "), "\n", " "))
	if subject == "" {
		subject = "Archived message"
	}
	reason := "邮件正文不可用"
	switch message.ContentState {
	case domain.ArchiveContentEvicted:
		reason = "邮件正文已按全局容量上限淘汰"
	case domain.ArchiveContentOversized:
		reason = "邮件超过单封 100 MiB 上限，仅保留元数据"
	case domain.ArchiveContentMissing:
		reason = "邮件归档文件缺失，仅保留元数据"
	case domain.ArchiveContentMetadata:
		reason = "该邮件来自升级前快照，仅迁移了标题与时间"
	}
	date := message.InternalDate
	if date.IsZero() {
		date = time.Unix(0, 0).UTC()
	}
	messageID := sanitizeHeader(message.MessageID)
	if messageID == "" {
		messageID = fmt.Sprintf("<archive-%d@localhost>", message.ID)
	} else if !strings.HasPrefix(messageID, "<") || !strings.HasSuffix(messageID, ">") {
		messageID = "<" + strings.Trim(messageID, "<>") + ">"
	}
	return []byte(fmt.Sprintf(
		"From: archive@localhost\r\nTo: %s\r\nDate: %s\r\nSubject: %s\r\nMessage-ID: %s\r\nX-Mail-Archive-State: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s。\r\n原标题：%s\r\n",
		sanitizeHeader(recipient), date.Format(time.RFC1123Z), mime.QEncoding.Encode("utf-8", subject),
		messageID, sanitizeHeader(message.ContentState), reason, subject,
	))
}

func sanitizeHeader(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r", ""), "\n", ""))
}

func messageEnvelope(message domain.ArchivedMailboxMessage) *imap.Envelope {
	date := message.InternalDate
	if message.HeaderDate != nil {
		date = *message.HeaderDate
	}
	return &imap.Envelope{
		Date:      date,
		Subject:   message.Subject,
		From:      imapAddresses(message.From),
		To:        imapAddresses(message.To),
		Cc:        imapAddresses(message.CC),
		MessageID: strings.Trim(strings.TrimSpace(message.MessageID), "<>"),
	}
}

func imapAddresses(addresses []domain.MailAddress) []imap.Address {
	result := make([]imap.Address, 0, len(addresses))
	for _, address := range addresses {
		mailbox, host, ok := strings.Cut(address.Email, "@")
		if !ok {
			continue
		}
		result = append(result, imap.Address{Name: address.Name, Mailbox: mailbox, Host: host})
	}
	return result
}

func dateMatches(value, since, before time.Time) bool {
	value = time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
	return (since.IsZero() || !value.Before(since)) && (before.IsZero() || value.Before(before))
}

func messageHeaderValue(message domain.ArchivedMailboxMessage, key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "subject":
		return message.Subject
	case "message-id":
		return message.MessageID
	case "from":
		return joinAddresses(message.From)
	case "to":
		return joinAddresses(message.To)
	case "cc":
		return joinAddresses(message.CC)
	case "date":
		if message.HeaderDate != nil {
			return message.HeaderDate.Format(time.RFC1123Z)
		}
	}
	return ""
}

func joinAddresses(addresses []domain.MailAddress) string {
	values := make([]string, 0, len(addresses))
	for _, address := range addresses {
		values = append(values, address.Name+" "+address.Email)
	}
	sort.Strings(values)
	return strings.Join(values, ", ")
}

func readerContainsFold(reader io.Reader, pattern string) (bool, error) {
	if pattern == "" {
		return true, nil
	}
	needle := bytes.ToLower([]byte(pattern))
	buffer := make([]byte, 32<<10)
	var tail []byte
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			chunk := append(append([]byte(nil), tail...), buffer[:count]...)
			chunk = bytes.ToLower(chunk)
			if bytes.Contains(chunk, needle) {
				return true, nil
			}
			keep := len(needle) - 1
			if keep > len(chunk) {
				keep = len(chunk)
			}
			tail = append(tail[:0], chunk[len(chunk)-keep:]...)
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func parseMessageHeader(reader io.Reader) (stdmail.Header, error) {
	message, err := stdmail.ReadMessage(bufio.NewReader(reader))
	if err != nil {
		return nil, err
	}
	return message.Header, nil
}

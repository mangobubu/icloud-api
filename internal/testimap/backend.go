package testimap

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"
)

var (
	ErrAccountNotFound = errors.New("test IMAP account not found")
	ErrMessageNotFound = errors.New("test IMAP message not found")
	ErrAccountExists   = errors.New("test IMAP account already exists")
)

type Account struct {
	ID             int64  `json:"id"`
	Username       string `json:"username"`
	ForwardAddress string `json:"forward_address"`
	UIDValidity    uint32 `json:"uid_validity"`
	UIDNext        uint32 `json:"uid_next"`
	MessageCount   int    `json:"message_count"`
	MessageBytes   int64  `json:"message_bytes"`
}

type StoredMessage struct {
	UID          uint32    `json:"uid"`
	InternalDate time.Time `json:"internal_date"`
	Size         int       `json:"size"`
	Seen         bool      `json:"seen"`
	Subject      string    `json:"subject,omitempty"`
	From         string    `json:"from,omitempty"`
	To           string    `json:"to,omitempty"`

	flags []string
	raw   []byte
}

type accountState struct {
	id             int64
	username       string
	password       string
	forwardAddress string
	uidValidity    uint32
	nextUID        uint32
	messages       []*StoredMessage
}

type Backend struct {
	mu             sync.RWMutex
	nextAccountID  int64
	uidValidity    uint32
	accountsByID   map[int64]*accountState
	accountsByUser map[string]*accountState
}

func NewBackend() *Backend {
	return newBackendWithUIDValidity(1)
}

func newBackendWithUIDValidity(uidValidity uint32) *Backend {
	if uidValidity == 0 {
		uidValidity = 1
	}
	return &Backend{
		nextAccountID:  1,
		uidValidity:    uidValidity,
		accountsByID:   make(map[int64]*accountState),
		accountsByUser: make(map[string]*accountState),
	}
}

func normalizeUsername(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func (b *Backend) CreateAccount(username, password, forwardAddress string) (Account, error) {
	username = normalizeUsername(username)
	var err error
	forwardAddress, err = normalizeMessageAddress(forwardAddress)
	if username == "" || strings.ContainsAny(username, "\r\n") || len(username) > 512 ||
		password == "" || len(password) > 4096 || err != nil {
		return Account{}, errors.New("username, password, and a valid forwarding address are required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.accountsByUser[username]; exists {
		return Account{}, ErrAccountExists
	}
	state := &accountState{
		id:             b.nextAccountID,
		username:       username,
		password:       password,
		forwardAddress: forwardAddress,
		uidValidity:    b.uidValidity,
		nextUID:        1,
	}
	b.nextAccountID++
	b.accountsByID[state.id] = state
	b.accountsByUser[state.username] = state
	return accountSnapshot(state), nil
}

func (b *Backend) ListAccounts() []Account {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]Account, 0, len(b.accountsByID))
	for _, state := range b.accountsByID {
		result = append(result, accountSnapshot(state))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (b *Backend) GetAccount(id int64) (Account, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	state := b.accountsByID[id]
	if state == nil {
		return Account{}, ErrAccountNotFound
	}
	return accountSnapshot(state), nil
}

func (b *Backend) DeleteAccount(id int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.accountsByID[id]
	if state == nil {
		return ErrAccountNotFound
	}
	delete(b.accountsByID, id)
	delete(b.accountsByUser, state.username)
	return nil
}

func (b *Backend) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.accountsByID = make(map[int64]*accountState)
	b.accountsByUser = make(map[string]*accountState)
}

func accountSnapshot(state *accountState) Account {
	var messageBytes int64
	for _, message := range state.messages {
		messageBytes += int64(message.Size)
	}
	return Account{
		ID:             state.id,
		Username:       state.username,
		ForwardAddress: state.forwardAddress,
		UIDValidity:    state.uidValidity,
		UIDNext:        state.nextUID,
		MessageCount:   len(state.messages),
		MessageBytes:   messageBytes,
	}
}

func (b *Backend) AddMessage(accountID int64, raw []byte, internalDate time.Time, seen bool) (StoredMessage, error) {
	if len(raw) == 0 {
		return StoredMessage{}, errors.New("raw message is empty")
	}
	if internalDate.IsZero() {
		internalDate = time.Now().UTC()
	}
	metadata := parseMessageMetadata(raw)
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.accountsByID[accountID]
	if state == nil {
		return StoredMessage{}, ErrAccountNotFound
	}
	if state.nextUID == 0 {
		return StoredMessage{}, errors.New("test IMAP UID space exhausted")
	}
	flags := []string{imap.RecentFlag}
	if seen {
		flags = append(flags, imap.SeenFlag)
	}
	stored := &StoredMessage{
		UID:          state.nextUID,
		InternalDate: internalDate.UTC(),
		Size:         len(raw),
		Seen:         seen,
		Subject:      metadata.Subject,
		From:         metadata.From,
		To:           metadata.To,
		flags:        flags,
		raw:          append([]byte(nil), raw...),
	}
	state.nextUID++
	state.messages = append(state.messages, stored)
	return publicMessage(stored), nil
}

func (b *Backend) ListStoredMessages(accountID int64) ([]StoredMessage, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	state := b.accountsByID[accountID]
	if state == nil {
		return nil, ErrAccountNotFound
	}
	result := make([]StoredMessage, len(state.messages))
	for index, stored := range state.messages {
		result[index] = publicMessage(stored)
	}
	return result, nil
}

func (b *Backend) SetMessageSeen(accountID int64, uid uint32, seen bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.accountsByID[accountID]
	if state == nil {
		return ErrAccountNotFound
	}
	stored := findMessage(state, uid)
	if stored == nil {
		return ErrMessageNotFound
	}
	if seen {
		stored.flags = backendutil.UpdateFlags(stored.flags, imap.AddFlags, []string{imap.SeenFlag})
	} else {
		stored.flags = backendutil.UpdateFlags(stored.flags, imap.RemoveFlags, []string{imap.SeenFlag})
	}
	stored.Seen = hasFlag(stored.flags, imap.SeenFlag)
	return nil
}

func (b *Backend) DeleteMessage(accountID int64, uid uint32) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.accountsByID[accountID]
	if state == nil {
		return ErrAccountNotFound
	}
	for index, stored := range state.messages {
		if stored.UID != uid {
			continue
		}
		copy(state.messages[index:], state.messages[index+1:])
		state.messages[len(state.messages)-1] = nil
		state.messages = state.messages[:len(state.messages)-1]
		return nil
	}
	return ErrMessageNotFound
}

func (b *Backend) ResetUIDValidity(accountID int64, clearMessages bool) (Account, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.accountsByID[accountID]
	if state == nil {
		return Account{}, ErrAccountNotFound
	}
	state.uidValidity++
	if state.uidValidity == 0 {
		state.uidValidity = 1
	}
	if clearMessages {
		state.messages = nil
		state.nextUID = 1
	}
	return accountSnapshot(state), nil
}

func findMessage(state *accountState, uid uint32) *StoredMessage {
	for _, stored := range state.messages {
		if stored.UID == uid {
			return stored
		}
	}
	return nil
}

func publicMessage(stored *StoredMessage) StoredMessage {
	return StoredMessage{
		UID:          stored.UID,
		InternalDate: stored.InternalDate,
		Size:         stored.Size,
		Seen:         hasFlag(stored.flags, imap.SeenFlag),
		Subject:      stored.Subject,
		From:         stored.From,
		To:           stored.To,
	}
}

type messageMetadata struct {
	Subject string
	From    string
	To      string
}

func parseMessageMetadata(raw []byte) messageMetadata {
	parsed, err := message.Read(bytes.NewReader(raw))
	if err != nil {
		return messageMetadata{}
	}
	return messageMetadata{
		Subject: parsed.Header.Get("Subject"),
		From:    parsed.Header.Get("From"),
		To:      parsed.Header.Get("To"),
	}
}

func hasFlag(flags []string, target string) bool {
	for _, flag := range flags {
		if imap.CanonicalFlag(flag) == imap.CanonicalFlag(target) {
			return true
		}
	}
	return false
}

func (b *Backend) Login(_ *imap.ConnInfo, username, password string) (backend.User, error) {
	b.mu.RLock()
	state := b.accountsByUser[normalizeUsername(username)]
	valid := state != nil && subtle.ConstantTimeCompare([]byte(state.password), []byte(password)) == 1
	var accountID int64
	var canonicalUsername string
	if valid {
		accountID = state.id
		canonicalUsername = state.username
	}
	b.mu.RUnlock()
	if !valid {
		return nil, backend.ErrInvalidCredentials
	}
	return &imapUser{backend: b, accountID: accountID, username: canonicalUsername}, nil
}

type imapUser struct {
	backend   *Backend
	accountID int64
	username  string
}

func (u *imapUser) Username() string { return u.username }

func (u *imapUser) ListMailboxes(subscribed bool) ([]backend.Mailbox, error) {
	return []backend.Mailbox{&imapMailbox{backend: u.backend, accountID: u.accountID}}, nil
}

func (u *imapUser) GetMailbox(name string) (backend.Mailbox, error) {
	if !strings.EqualFold(name, "INBOX") {
		return nil, backend.ErrNoSuchMailbox
	}
	return &imapMailbox{backend: u.backend, accountID: u.accountID}, nil
}

func (u *imapUser) CreateMailbox(name string) error { return backend.ErrMailboxAlreadyExists }
func (u *imapUser) DeleteMailbox(name string) error { return backend.ErrNoSuchMailbox }
func (u *imapUser) RenameMailbox(existingName, newName string) error {
	return backend.ErrNoSuchMailbox
}
func (u *imapUser) Logout() error { return nil }

type imapMailbox struct {
	backend   *Backend
	accountID int64
}

func (m *imapMailbox) Name() string { return "INBOX" }

func (m *imapMailbox) Info() (*imap.MailboxInfo, error) {
	return &imap.MailboxInfo{Name: "INBOX", Delimiter: "/"}, nil
}

func (m *imapMailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	m.backend.mu.RLock()
	defer m.backend.mu.RUnlock()
	state := m.backend.accountsByID[m.accountID]
	if state == nil {
		return nil, ErrAccountNotFound
	}
	status := imap.NewMailboxStatus("INBOX", items)
	status.Flags = []string{imap.SeenFlag, imap.DeletedFlag, imap.RecentFlag}
	status.PermanentFlags = []string{imap.SeenFlag, imap.DeletedFlag}
	status.UidValidity = state.uidValidity
	status.UidNext = state.nextUID
	status.Messages = uint32(len(state.messages))
	for index, stored := range state.messages {
		if !hasFlag(stored.flags, imap.SeenFlag) {
			status.Unseen++
			if status.UnseenSeqNum == 0 {
				status.UnseenSeqNum = uint32(index + 1)
			}
		}
		if hasFlag(stored.flags, imap.RecentFlag) {
			status.Recent++
		}
	}
	return status, nil
}

func (m *imapMailbox) SetSubscribed(bool) error { return nil }
func (m *imapMailbox) Check() error             { return nil }

func (m *imapMailbox) ListMessages(uid bool, seqset *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	defer close(ch)
	m.backend.mu.RLock()
	state := m.backend.accountsByID[m.accountID]
	if state == nil {
		m.backend.mu.RUnlock()
		return ErrAccountNotFound
	}
	messages := cloneMessages(state.messages)
	m.backend.mu.RUnlock()
	for index, stored := range messages {
		sequence := uint32(index + 1)
		identity := sequence
		if uid {
			identity = stored.UID
		}
		if !seqset.Contains(identity) {
			continue
		}
		fetched, err := fetchStoredMessage(stored, sequence, items)
		if err != nil {
			return err
		}
		ch <- fetched
	}
	return nil
}

func (m *imapMailbox) SearchMessages(uid bool, criteria *imap.SearchCriteria) ([]uint32, error) {
	m.backend.mu.RLock()
	state := m.backend.accountsByID[m.accountID]
	if state == nil {
		m.backend.mu.RUnlock()
		return nil, ErrAccountNotFound
	}
	messages := cloneMessages(state.messages)
	m.backend.mu.RUnlock()
	var result []uint32
	for index, stored := range messages {
		sequence := uint32(index + 1)
		matched, err := matchStoredMessage(stored, sequence, criteria)
		if err != nil || !matched {
			continue
		}
		if uid {
			result = append(result, stored.UID)
		} else {
			result = append(result, sequence)
		}
	}
	return result, nil
}

func (m *imapMailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	raw, err := io.ReadAll(io.LimitReader(body, 16<<20+1))
	if err != nil {
		return err
	}
	if len(raw) > 16<<20 {
		return errors.New("test message exceeds 16 MiB")
	}
	_, err = m.backend.AddMessage(m.accountID, raw, date, hasFlag(flags, imap.SeenFlag))
	return err
}

func (m *imapMailbox) UpdateMessagesFlags(uid bool, seqset *imap.SeqSet, operation imap.FlagsOp, flags []string) error {
	m.backend.mu.Lock()
	defer m.backend.mu.Unlock()
	state := m.backend.accountsByID[m.accountID]
	if state == nil {
		return ErrAccountNotFound
	}
	for index, stored := range state.messages {
		identity := uint32(index + 1)
		if uid {
			identity = stored.UID
		}
		if !seqset.Contains(identity) {
			continue
		}
		stored.flags = backendutil.UpdateFlags(stored.flags, operation, flags)
		stored.Seen = hasFlag(stored.flags, imap.SeenFlag)
	}
	return nil
}

func (m *imapMailbox) CopyMessages(uid bool, seqset *imap.SeqSet, destination string) error {
	if !strings.EqualFold(destination, "INBOX") {
		return backend.ErrNoSuchMailbox
	}
	m.backend.mu.Lock()
	defer m.backend.mu.Unlock()
	state := m.backend.accountsByID[m.accountID]
	if state == nil {
		return ErrAccountNotFound
	}
	original := append([]*StoredMessage(nil), state.messages...)
	for index, stored := range original {
		identity := uint32(index + 1)
		if uid {
			identity = stored.UID
		}
		if !seqset.Contains(identity) {
			continue
		}
		copy := *stored
		copy.UID = state.nextUID
		copy.flags = append([]string(nil), stored.flags...)
		copy.raw = append([]byte(nil), stored.raw...)
		state.nextUID++
		state.messages = append(state.messages, &copy)
	}
	return nil
}

func (m *imapMailbox) Expunge() error {
	m.backend.mu.Lock()
	defer m.backend.mu.Unlock()
	state := m.backend.accountsByID[m.accountID]
	if state == nil {
		return ErrAccountNotFound
	}
	kept := make([]*StoredMessage, 0, len(state.messages))
	for _, stored := range state.messages {
		if !hasFlag(stored.flags, imap.DeletedFlag) {
			kept = append(kept, stored)
		}
	}
	state.messages = kept
	return nil
}

func cloneMessages(source []*StoredMessage) []*StoredMessage {
	result := make([]*StoredMessage, len(source))
	for index, stored := range source {
		copy := *stored
		copy.flags = append([]string(nil), stored.flags...)
		copy.raw = append([]byte(nil), stored.raw...)
		result[index] = &copy
	}
	return result
}

func fetchStoredMessage(stored *StoredMessage, sequence uint32, items []imap.FetchItem) (*imap.Message, error) {
	fetched := imap.NewMessage(sequence, items)
	for _, item := range items {
		switch item {
		case imap.FetchEnvelope:
			header, _, _ := storedHeaderAndBody(stored)
			fetched.Envelope, _ = backendutil.FetchEnvelope(header)
		case imap.FetchBody, imap.FetchBodyStructure:
			header, body, _ := storedHeaderAndBody(stored)
			fetched.BodyStructure, _ = backendutil.FetchBodyStructure(header, body, item == imap.FetchBodyStructure)
		case imap.FetchFlags:
			fetched.Flags = append([]string(nil), stored.flags...)
		case imap.FetchInternalDate:
			fetched.InternalDate = stored.InternalDate
		case imap.FetchRFC822Size:
			fetched.Size = uint32(stored.Size)
		case imap.FetchUid:
			fetched.Uid = stored.UID
		default:
			section, err := imap.ParseBodySectionName(item)
			if err != nil {
				continue
			}
			body := bufio.NewReader(bytes.NewReader(stored.raw))
			header, err := textproto.ReadHeader(body)
			if err != nil {
				return nil, err
			}
			literal, err := backendutil.FetchBodySection(header, body, section)
			if err != nil {
				return nil, err
			}
			fetched.Body[section] = literal
		}
	}
	return fetched, nil
}

func matchStoredMessage(stored *StoredMessage, sequence uint32, criteria *imap.SearchCriteria) (bool, error) {
	entity, err := message.Read(bytes.NewReader(stored.raw))
	if err != nil {
		return false, err
	}
	return backendutil.Match(entity, sequence, stored.UID, stored.InternalDate, stored.flags, criteria)
}

func storedHeaderAndBody(stored *StoredMessage) (textproto.Header, io.Reader, error) {
	body := bufio.NewReader(bytes.NewReader(stored.raw))
	header, err := textproto.ReadHeader(body)
	return header, body, err
}

var _ backend.Backend = (*Backend)(nil)
var _ backend.User = (*imapUser)(nil)
var _ backend.Mailbox = (*imapMailbox)(nil)

func (b *Backend) String() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return fmt.Sprintf("test IMAP backend with %d account(s)", len(b.accountsByID))
}

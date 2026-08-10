// Package applog provides a bounded, process-local application log for the
// administrator UI. It is deliberately independent from the stdout handler so
// stdout remains the deployment log of record.
package applog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// MaxMessageBytes bounds one rendered log message.
	MaxMessageBytes = 4 << 10
	// MaxFieldKeyBytes bounds one flattened structured field key.
	MaxFieldKeyBytes = 256
	// MaxFieldValueBytes bounds one rendered structured field value.
	MaxFieldValueBytes = 2 << 10
	// MaxEntryBytes bounds the approximate encoded size of one entry.
	MaxEntryBytes = 16 << 10
	// MaxStoredBytes bounds the approximate encoded size of the entire ring.
	// It accommodates 2,000 entries at MaxEntryBytes without early eviction.
	MaxStoredBytes = 32 << 20
	// MaxFields bounds the number of structured fields retained per entry.
	MaxFields = 64

	DefaultListLimit = 100
	MaxListLimit     = 200

	maxSourceBytes = 512
	fieldOverhead  = 16
	entryOverhead  = 64
	redactedValue  = "[REDACTED]"
)

// Entry is a sanitized snapshot of one slog record. Fields is detached from
// the ring before it is returned to a caller.
type Entry struct {
	ID      uint64            `json:"id"`
	Time    time.Time         `json:"time"`
	Level   slog.Level        `json:"level"`
	Message string            `json:"message"`
	Source  string            `json:"source,omitempty"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// Filter selects entries from newest to oldest. BeforeID is exclusive; zero
// starts at the newest retained entry. Level is empty for all levels or a slog
// level name such as INFO, WARN, or ERROR.
type Filter struct {
	Level           string
	Query           string
	AccountID       *int64
	SyncRunID       string
	AutoCreateRunID string
	BeforeID        uint64
	Limit           int
}

// Page is one reverse-chronological cursor page.
type Page struct {
	Items        []Entry
	HasMore      bool
	NextBeforeID uint64
}

type limits struct {
	messageBytes    int
	fieldKeyBytes   int
	fieldValueBytes int
	entryBytes      int
	totalBytes      int
	fields          int
	sourceBytes     int
}

func defaultLimits() limits {
	return limits{
		messageBytes:    MaxMessageBytes,
		fieldKeyBytes:   MaxFieldKeyBytes,
		fieldValueBytes: MaxFieldValueBytes,
		entryBytes:      MaxEntryBytes,
		totalBytes:      MaxStoredBytes,
		fields:          MaxFields,
		sourceBytes:     maxSourceBytes,
	}
}

type storedEntry struct {
	entry Entry
	size  int
}

type ring struct {
	mu         sync.RWMutex
	entries    []storedEntry
	head       int
	count      int
	nextID     uint64
	totalBytes int
	limits     limits
}

type boundAttr struct {
	groups []string
	attr   slog.Attr
}

// Handler is a thread-safe, in-memory slog.Handler. Handler values returned by
// WithAttrs and WithGroup share the same ring while retaining immutable slog
// formatting state.
type Handler struct {
	ring   *ring
	attrs  []boundAttr
	groups []string
}

// New creates a bounded application log. Capacity must be positive. Entries
// may be evicted before capacity is reached when MaxStoredBytes is exhausted.
func New(capacity int) *Handler {
	return newHandler(capacity, defaultLimits())
}

func newHandler(capacity int, configured limits) *Handler {
	if capacity <= 0 {
		panic("applog: capacity must be positive")
	}
	configured = normalizedLimits(configured)
	return &Handler{ring: &ring{
		entries: make([]storedEntry, capacity),
		limits:  configured,
	}}
}

func normalizedLimits(value limits) limits {
	defaults := defaultLimits()
	if value.messageBytes <= 0 {
		value.messageBytes = defaults.messageBytes
	}
	if value.fieldKeyBytes <= 0 {
		value.fieldKeyBytes = defaults.fieldKeyBytes
	}
	if value.fieldValueBytes <= 0 {
		value.fieldValueBytes = defaults.fieldValueBytes
	}
	if value.entryBytes <= entryOverhead {
		value.entryBytes = defaults.entryBytes
	}
	if value.totalBytes < value.entryBytes {
		value.totalBytes = value.entryBytes
	}
	if value.fields <= 0 {
		value.fields = defaults.fields
	}
	if value.sourceBytes <= 0 {
		value.sourceBytes = defaults.sourceBytes
	}
	return value
}

// Enabled captures every level selected by slog. Deployment handlers can
// still independently apply their own level thresholds when combined by Tee.
func (h *Handler) Enabled(context.Context, slog.Level) bool {
	return h != nil && h.ring != nil
}

// Handle snapshots a record without performing file, network, or database I/O.
func (h *Handler) Handle(_ context.Context, record slog.Record) error {
	if h == nil || h.ring == nil {
		return nil
	}

	collector := newFieldCollector(h.ring.limits)
	for _, bound := range h.attrs {
		collector.addAttr(bound.groups, bound.attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		collector.addAttr(h.groups, attr)
		return true
	})

	entry, size := buildEntry(record, collector.values, h.ring.limits)
	h.ring.append(entry, size)
	return nil
}

// WithAttrs returns a handler with attrs bound to the current group path.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if h == nil {
		return (*Handler)(nil)
	}
	clone := &Handler{
		ring:   h.ring,
		attrs:  append([]boundAttr(nil), h.attrs...),
		groups: append([]string(nil), h.groups...),
	}
	for _, attr := range attrs {
		clone.attrs = append(clone.attrs, boundAttr{
			groups: append([]string(nil), h.groups...),
			attr:   attr,
		})
	}
	return clone
}

// WithGroup returns a handler that qualifies subsequent attrs with name.
func (h *Handler) WithGroup(name string) slog.Handler {
	if h == nil {
		return (*Handler)(nil)
	}
	if name == "" {
		return h
	}
	return &Handler{
		ring:   h.ring,
		attrs:  append([]boundAttr(nil), h.attrs...),
		groups: append(append([]string(nil), h.groups...), name),
	}
}

// List returns a reverse-chronological page. Invalid level names match no
// entries; HTTP callers should reject them as invalid query parameters.
func (h *Handler) List(filter Filter) Page {
	page := Page{Items: make([]Entry, 0)}
	if h == nil || h.ring == nil {
		return page
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	level, levelSet, valid := parseLevel(filter.Level)
	if !valid {
		return page
	}
	query := strings.ToLower(strings.TrimSpace(filter.Query))

	h.ring.mu.RLock()
	defer h.ring.mu.RUnlock()
	for offset := 0; offset < h.ring.count; offset++ {
		index := (h.ring.head + h.ring.count - 1 - offset) % len(h.ring.entries)
		candidate := h.ring.entries[index].entry
		if filter.BeforeID != 0 && candidate.ID >= filter.BeforeID {
			continue
		}
		if levelSet && !matchesLevel(candidate.Level, level) {
			continue
		}
		if filter.AccountID != nil && !entryHasAccountID(candidate, *filter.AccountID) {
			continue
		}
		if filter.SyncRunID != "" && !entryHasExactField(candidate, "sync_run_id", filter.SyncRunID) {
			continue
		}
		if filter.AutoCreateRunID != "" && !entryHasExactField(candidate, "auto_create_run_id", filter.AutoCreateRunID) {
			continue
		}
		if query != "" && !entryContains(candidate, query) {
			continue
		}
		if len(page.Items) == limit {
			page.HasMore = true
			page.NextBeforeID = page.Items[len(page.Items)-1].ID
			break
		}
		page.Items = append(page.Items, cloneEntry(candidate))
	}
	return page
}

func parseLevel(value string) (slog.Level, bool, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false, true
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(value))); err != nil {
		return 0, true, false
	}
	return level, true, true
}

func matchesLevel(candidate, filter slog.Level) bool {
	switch filter {
	case slog.LevelDebug:
		return candidate < slog.LevelInfo
	case slog.LevelInfo:
		return candidate >= slog.LevelInfo && candidate < slog.LevelWarn
	case slog.LevelWarn:
		return candidate >= slog.LevelWarn && candidate < slog.LevelError
	case slog.LevelError:
		return candidate >= slog.LevelError
	default:
		return candidate == filter
	}
}

func entryHasAccountID(entry Entry, want int64) bool {
	for key, value := range entry.Fields {
		if !FieldKeyHasSuffix(key, "account_id") {
			continue
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err == nil && parsed == want {
			return true
		}
	}
	return false
}

func entryHasExactField(entry Entry, name, want string) bool {
	for key, value := range entry.Fields {
		if !FieldKeyHasSuffix(key, name) {
			continue
		}
		if value == want {
			return true
		}
	}
	return false
}

func entryContains(entry Entry, query string) bool {
	if strings.Contains(strings.ToLower(entry.Message), query) ||
		strings.Contains(strings.ToLower(entry.Source), query) ||
		strings.Contains(strings.ToLower(entry.Level.String()), query) {
		return true
	}
	for key, value := range entry.Fields {
		if strings.Contains(strings.ToLower(key), query) ||
			strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func cloneEntry(entry Entry) Entry {
	clone := entry
	if entry.Fields != nil {
		clone.Fields = make(map[string]string, len(entry.Fields))
		for key, value := range entry.Fields {
			clone.Fields[key] = value
		}
	}
	return clone
}

func (r *ring) append(entry Entry, size int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID++
	entry.ID = r.nextID
	for r.count > 0 && (r.count == len(r.entries) || r.totalBytes+size > r.limits.totalBytes) {
		r.evictOldest()
	}
	index := (r.head + r.count) % len(r.entries)
	r.entries[index] = storedEntry{entry: entry, size: size}
	r.count++
	r.totalBytes += size
}

func (r *ring) evictOldest() {
	oldest := &r.entries[r.head]
	r.totalBytes -= oldest.size
	*oldest = storedEntry{}
	r.head = (r.head + 1) % len(r.entries)
	r.count--
}

type fieldValue struct {
	key   string
	value string
}

type fieldCollector struct {
	limits limits
	values []fieldValue
	index  map[string]int
}

func newFieldCollector(configured limits) *fieldCollector {
	return &fieldCollector{limits: configured, index: make(map[string]int)}
}

func (c *fieldCollector) addAttr(groups []string, attr slog.Attr) {
	if attr.Equal(slog.Attr{}) {
		return
	}
	parts := append(append([]string(nil), groups...), attr.Key)
	if attr.Value.Kind() != slog.KindGroup && sensitiveKeyParts(parts) {
		c.addValue(parts, redactedValue)
		return
	}
	if attr.Value.Kind() == slog.KindGroup && attr.Key != "" && sensitiveKeyParts(parts) {
		c.addValue(parts, redactedValue)
		return
	}
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		nestedGroups := groups
		if attr.Key != "" {
			nestedGroups = append(append([]string(nil), groups...), attr.Key)
		}
		for _, nested := range value.Group() {
			c.addAttr(nestedGroups, nested)
		}
		return
	}
	field := renderValue(value)
	if sensitiveKeyParts(parts) {
		field = redactedValue
	} else {
		field = truncateUTF8(field, c.limits.fieldValueBytes)
	}
	c.addValue(parts, field)
}

func (c *fieldCollector) addValue(parts []string, field string) {
	key := truncateUTF8(flattenKey(parts), c.limits.fieldKeyBytes)
	if existing, ok := c.index[key]; ok {
		c.values[existing].value = field
		return
	}
	c.index[key] = len(c.values)
	c.values = append(c.values, fieldValue{key: key, value: field})
}

// FieldKeyHasSuffix matches a root field or a true grouped field path. A
// literal dot in a slog key is escaped by flattenKey and must not count as a
// group separator.
func FieldKeyHasSuffix(key, name string) bool {
	segments := splitFlattenedKey(key)
	return len(segments) > 0 && segments[len(segments)-1] == name
}

func splitFlattenedKey(key string) []string {
	segments := []string{}
	current := strings.Builder{}
	escaped := false
	for _, character := range key {
		if escaped {
			current.WriteRune(character)
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if character == '.' {
			segments = append(segments, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(character)
	}
	if escaped {
		current.WriteRune('\\')
	}
	segments = append(segments, current.String())
	return segments
}

func sensitiveKeyParts(parts []string) bool {
	for _, part := range parts {
		if sensitiveKey(part) {
			return true
		}
	}
	return false
}

func flattenKey(parts []string) string {
	escaped := make([]string, len(parts))
	for index, part := range parts {
		part = strings.ReplaceAll(part, `\`, `\\`)
		escaped[index] = strings.ReplaceAll(part, ".", `\.`)
	}
	return strings.Join(escaped, ".")
}

func renderValue(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		return value.String()
	case slog.KindTime:
		return value.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindBool:
		return strconv.FormatBool(value.Bool())
	case slog.KindInt64:
		return strconv.FormatInt(value.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(value.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(value.Float64(), 'g', -1, 64)
	case slog.KindAny:
		if err, ok := value.Any().(error); ok {
			return err.Error()
		}
		return fmt.Sprint(value.Any())
	default:
		return value.String()
	}
}

func sensitiveKey(key string) bool {
	normalized := strings.ToLower(key)
	normalized = strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(normalized)
	compact := strings.ReplaceAll(normalized, "_", "")
	for _, marker := range []string{
		"password", "passwd", "secret", "token", "apikey", "authorization",
		"cookie", "session", "credential", "ciphertext", "privatekey", "masterkey",
		"rawquery", "requestbody", "responsebody",
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return normalized == "query" || normalized == "body" || normalized == "headers"
}

func buildEntry(record slog.Record, values []fieldValue, configured limits) (Entry, int) {
	entry := Entry{
		Time:   record.Time.UTC(),
		Level:  record.Level,
		Fields: make(map[string]string),
	}
	remaining := configured.entryBytes - entryOverhead
	entry.Message, remaining = fitText(record.Message, configured.messageBytes, remaining)
	entry.Source, remaining = fitText(sourceLocation(record.PC), configured.sourceBytes, remaining)

	retained := 0
	for _, field := range values {
		if retained >= configured.fields {
			break
		}
		key := truncateUTF8(field.key, configured.fieldKeyBytes)
		need := len(key) + fieldOverhead
		if remaining <= need {
			break
		}
		valueBudget := min(configured.fieldValueBytes, remaining-need)
		value := truncateUTF8(field.value, valueBudget)
		entry.Fields[key] = value
		remaining -= need + len(value)
		retained++
	}
	if len(entry.Fields) == 0 {
		entry.Fields = nil
	}
	return entry, configured.entryBytes - remaining
}

func fitText(value string, individualLimit, remaining int) (string, int) {
	if remaining <= 0 {
		return "", remaining
	}
	limit := min(individualLimit, remaining)
	value = truncateUTF8(value, limit)
	return value, remaining - len(value)
}

func truncateUTF8(value string, limit int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		end := limit
		for end > 0 && !utf8.RuneStart(value[end]) {
			end--
		}
		return value[:end]
	}
	end := limit - 3
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "..."
}

func sourceLocation(pc uintptr) string {
	if pc == 0 {
		return ""
	}
	frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	if frame.File == "" || frame.Line <= 0 {
		return ""
	}
	path := filepath.ToSlash(frame.File)
	for _, marker := range []string{"/internal/", "/cmd/"} {
		if index := strings.LastIndex(path, marker); index >= 0 {
			path = path[index+1:]
			return path + ":" + strconv.Itoa(frame.Line)
		}
	}
	return filepath.Base(frame.File) + ":" + strconv.Itoa(frame.Line)
}

// Tee fans slog records out to all enabled handlers. It preserves each
// handler's independent level decision and applies attributes/groups to every
// branch. The in-memory Handler branch remains free of external I/O.
func Tee(handlers ...slog.Handler) slog.Handler {
	filtered := make([]slog.Handler, 0, len(handlers))
	for _, handler := range handlers {
		if handler != nil {
			filtered = append(filtered, handler)
		}
	}
	return &teeHandler{handlers: filtered}
}

type teeHandler struct {
	handlers []slog.Handler
}

func (h *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *teeHandler) Handle(ctx context.Context, record slog.Record) error {
	var joined error
	for _, handler := range h.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		if err := handler.Handle(ctx, record.Clone()); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	children := make([]slog.Handler, len(h.handlers))
	for index, handler := range h.handlers {
		children[index] = handler.WithAttrs(append([]slog.Attr(nil), attrs...))
	}
	return &teeHandler{handlers: children}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	children := make([]slog.Handler, len(h.handlers))
	for index, handler := range h.handlers {
		children[index] = handler.WithGroup(name)
	}
	return &teeHandler{handlers: children}
}

package applog

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestHandlerRecordsNewestFirstWithSource(t *testing.T) {
	handler := New(10)
	logger := slog.New(handler)

	logger.Info("first", "account_id", int64(17), "request_id", "req-1")
	logger.Warn("second", "duration_ms", 12)

	page := handler.List(Filter{Limit: 10})
	if len(page.Items) != 2 {
		t.Fatalf("item count = %d, want 2", len(page.Items))
	}
	if got := page.Items[0]; got.ID != 2 || got.Level != slog.LevelWarn || got.Message != "second" {
		t.Fatalf("newest item = %#v", got)
	}
	if got := page.Items[1]; got.ID != 1 || got.Fields["account_id"] != "17" || got.Fields["request_id"] != "req-1" {
		t.Fatalf("oldest item = %#v", got)
	}
	for _, item := range page.Items {
		if item.Time.IsZero() {
			t.Fatal("entry time is zero")
		}
		if !strings.HasPrefix(item.Source, "internal/applog/handler_test.go:") &&
			!strings.HasPrefix(item.Source, "handler_test.go:") {
			t.Fatalf("source = %q, want relative handler_test.go:line", item.Source)
		}
	}
	if page.HasMore || page.NextBeforeID != 0 {
		t.Fatalf("unexpected pagination: %#v", page)
	}
}

func TestHandlerEvictsByCapacity(t *testing.T) {
	handler := New(2)
	logger := slog.New(handler)
	logger.Info("one")
	logger.Info("two")
	logger.Info("three")

	page := handler.List(Filter{Limit: 10})
	if len(page.Items) != 2 {
		t.Fatalf("item count = %d, want 2", len(page.Items))
	}
	if page.Items[0].ID != 3 || page.Items[1].ID != 2 {
		t.Fatalf("retained IDs = %d, %d; want 3, 2", page.Items[0].ID, page.Items[1].ID)
	}
}

func TestListFiltersAndCursorPagination(t *testing.T) {
	handler := New(20)
	logger := slog.New(handler)
	logger.Info("HTTP request", "account_id", int64(1), "request_id", "req-a")
	logger.Warn("mail sync delayed", "account_id", int64(2), "operation", "scan")
	logger.Error("database write failed", "account_id", int64(1), "request_id", "req-b")
	logger.Warn("mail sync failed", slog.Group("sync", "account_id", int64(1)), "error", "connection closed")

	warnPage := handler.List(Filter{Level: "warn", Limit: 10})
	assertIDs(t, warnPage.Items, 4, 2)
	queryPage := handler.List(Filter{Query: "CONNECTION CLOSED", Limit: 10})
	assertIDs(t, queryPage.Items, 4)
	fieldQueryPage := handler.List(Filter{Query: "REQ-B", Limit: 10})
	assertIDs(t, fieldQueryPage.Items, 3)
	accountID := int64(1)
	accountPage := handler.List(Filter{AccountID: &accountID, Limit: 2})
	assertIDs(t, accountPage.Items, 4, 3)
	if accountPage.Total != 3 {
		t.Fatalf("first cursor page total = %d, want 3", accountPage.Total)
	}
	if !accountPage.HasMore || accountPage.NextBeforeID != 3 {
		t.Fatalf("first cursor page = %#v", accountPage)
	}
	next := handler.List(Filter{AccountID: &accountID, BeforeID: accountPage.NextBeforeID, Limit: 2})
	assertIDs(t, next.Items, 1)
	if next.Total != 3 {
		t.Fatalf("next cursor page total = %d, want 3", next.Total)
	}
	if next.HasMore || next.NextBeforeID != 0 {
		t.Fatalf("last cursor page = %#v", next)
	}

	if got := handler.List(Filter{Level: "not-a-level", Limit: 10}); len(got.Items) != 0 {
		t.Fatalf("invalid level returned %d items", len(got.Items))
	}
}

func TestListOffsetPaginationCountsAllFilteredEntries(t *testing.T) {
	handler := New(20)
	logger := slog.New(handler)
	logger.Info("first", "kind", "included")
	logger.Info("ignored", "kind", "other")
	logger.Warn("second", "kind", "included")
	logger.Error("third", "kind", "included")
	logger.Info("fourth", "kind", "included")

	page := handler.List(Filter{Query: "included", Offset: 1, Limit: 2})
	assertIDs(t, page.Items, 4, 3)
	if page.Total != 4 || !page.HasMore || page.NextBeforeID != 3 {
		t.Fatalf("middle offset page = %#v, want total=4 has-more=true next-before=3", page)
	}

	last := handler.List(Filter{Query: "included", Offset: 3, Limit: 2})
	assertIDs(t, last.Items, 1)
	if last.Total != 4 || last.HasMore || last.NextBeforeID != 0 {
		t.Fatalf("last offset page = %#v, want total=4 and no next page", last)
	}

	empty := handler.List(Filter{Query: "included", Offset: 10, Limit: 2})
	if len(empty.Items) != 0 || empty.Total != 4 || empty.HasMore || empty.NextBeforeID != 0 {
		t.Fatalf("empty offset page = %#v, want retained total with no page", empty)
	}
}

func TestListAppliesDefaultAndMaximumLimits(t *testing.T) {
	handler := New(MaxListLimit + 1)
	logger := slog.New(handler)
	for index := 0; index < MaxListLimit+1; index++ {
		logger.Info("bounded page", "index", index)
	}

	defaultPage := handler.List(Filter{})
	if len(defaultPage.Items) != DefaultListLimit || !defaultPage.HasMore {
		t.Fatalf("default page length/has-more = %d/%t, want %d/true", len(defaultPage.Items), defaultPage.HasMore, DefaultListLimit)
	}

	maximumPage := handler.List(Filter{Limit: MaxListLimit + 1})
	if len(maximumPage.Items) != MaxListLimit || !maximumPage.HasMore {
		t.Fatalf("maximum page length/has-more = %d/%t, want %d/true", len(maximumPage.Items), maximumPage.HasMore, MaxListLimit)
	}
}

func TestListFiltersSyncRunIDExactly(t *testing.T) {
	handler := New(10)
	logger := slog.New(handler)
	logger.Info("exact", "sync_run_id", "sync-run-1")
	logger.Info("different", "sync_run_id", "sync-run-10")
	logger.Info("grouped", slog.Group("sync", "sync_run_id", "sync-run-1"))
	logger.Info("missing")

	page := handler.List(Filter{SyncRunID: "sync-run-1", Limit: 10})
	if len(page.Items) != 2 || page.Items[0].Message != "grouped" || page.Items[1].Message != "exact" {
		t.Fatalf("exact sync run page = %#v", page)
	}
}

func TestListFiltersAutoCreateRunIDExactly(t *testing.T) {
	handler := New(10)
	logger := slog.New(handler)
	logger.Info("exact", "auto_create_run_id", "auto-run-1")
	logger.Info("different", "auto_create_run_id", "auto-run-10")
	logger.Info("grouped", slog.Group("autocreate", "auto_create_run_id", "auto-run-1"))
	logger.Info("missing")

	page := handler.List(Filter{AutoCreateRunID: "auto-run-1", Limit: 10})
	if len(page.Items) != 2 || page.Items[0].Message != "grouped" || page.Items[1].Message != "exact" {
		t.Fatalf("exact auto-create run page = %#v", page)
	}
	logger.Info("literal-dot-key", slog.String("other.auto_create_run_id", "auto-run-1"))
	if page := handler.List(Filter{AutoCreateRunID: "auto-run-1", Limit: 10}); len(page.Items) != 2 {
		t.Fatalf("literal dotted key was treated as grouped run id: %#v", page.Items)
	}
}

func TestFieldKeyHasSuffixDistinguishesEscapedDots(t *testing.T) {
	if !FieldKeyHasSuffix("autocreate.auto_create_run_id", "auto_create_run_id") {
		t.Fatal("grouped field was not recognized")
	}
	if FieldKeyHasSuffix(`other\.auto_create_run_id`, "auto_create_run_id") {
		t.Fatal("escaped literal dot was recognized as a group")
	}
}

func TestWithAttrsAndGroupsPreserveBindingOrder(t *testing.T) {
	handler := New(10)
	derived := handler.
		WithAttrs([]slog.Attr{slog.String("before", "outside")}).
		WithGroup("outer").
		WithAttrs([]slog.Attr{slog.String("bound", "inside")}).
		WithGroup("inner.with.dot")
	logger := slog.New(derived)

	logger.Info("grouped",
		slog.String("record", "value"),
		slog.Group("nested", slog.String("key", "nested-value")),
		slog.Group("", slog.String("inline", "inline-value")),
		slog.Attr{Key: "", Value: slog.StringValue("empty-key-value")},
		slog.String("record", "replacement"),
	)

	page := handler.List(Filter{Limit: 1})
	fields := page.Items[0].Fields
	want := map[string]string{
		"before":                            "outside",
		"outer.bound":                       "inside",
		`outer.inner\.with\.dot.record`:     "replacement",
		`outer.inner\.with\.dot.nested.key`: "nested-value",
		`outer.inner\.with\.dot.inline`:     "inline-value",
		`outer.inner\.with\.dot.`:           "empty-key-value",
	}
	if len(fields) != len(want) {
		t.Fatalf("fields = %#v, want %#v", fields, want)
	}
	for key, value := range want {
		if fields[key] != value {
			t.Errorf("field %q = %q, want %q", key, fields[key], value)
		}
	}
}

func TestHandlerRedactsSensitiveKeysAndBoundsEntries(t *testing.T) {
	configured := limits{
		messageBytes:    16,
		fieldKeyBytes:   32,
		fieldValueBytes: 12,
		entryBytes:      180,
		totalBytes:      500,
		fields:          3,
		sourceBytes:     20,
	}
	handler := newHandler(10, configured)
	logger := slog.New(handler)
	logger.Info(
		"abcdefghijklmnopqrstuvwxyz",
		"password", "plain-text-password",
		"oauth_token", "plain-text-token",
		"detail", "abcdefghijklmnopqrstuvwxyz",
		"ignored", "fourth field",
	)

	page := handler.List(Filter{Limit: 1})
	if len(page.Items) != 1 {
		t.Fatalf("item count = %d, want 1", len(page.Items))
	}
	item := page.Items[0]
	if len(item.Message) > configured.messageBytes || !utf8.ValidString(item.Message) || !strings.HasSuffix(item.Message, "...") {
		t.Fatalf("bounded message = %q", item.Message)
	}
	if item.Fields["password"] != redactedValue || item.Fields["oauth_token"] != redactedValue {
		t.Fatalf("sensitive fields = %#v", item.Fields)
	}
	if len(item.Fields["detail"]) > configured.fieldValueBytes {
		t.Fatalf("detail length = %d, want <= %d", len(item.Fields["detail"]), configured.fieldValueBytes)
	}
	if _, exists := item.Fields["ignored"]; exists {
		t.Fatalf("field count limit retained ignored field: %#v", item.Fields)
	}

	handler.ring.mu.RLock()
	stored := handler.ring.entries[handler.ring.head]
	total := handler.ring.totalBytes
	handler.ring.mu.RUnlock()
	if stored.size > configured.entryBytes {
		t.Fatalf("entry size = %d, want <= %d", stored.size, configured.entryBytes)
	}
	if total > configured.totalBytes {
		t.Fatalf("ring size = %d, want <= %d", total, configured.totalBytes)
	}
}

func TestHandlerRedactsNestedAndDottedSensitiveKeys(t *testing.T) {
	handler := New(10)
	logger := slog.New(handler)
	logger.Info("sensitive paths",
		slog.String("api.key", "API_KEY_SECRET"),
		slog.Group("http", slog.String("headers", "Authorization: TOKEN")),
		slog.String("private.key", "PRIVATE_KEY_SECRET"),
	)
	entry := handler.List(Filter{Limit: 1}).Items[0]
	for key, value := range entry.Fields {
		if strings.Contains(value, "API_KEY_SECRET") || strings.Contains(value, "TOKEN") || strings.Contains(value, "PRIVATE_KEY_SECRET") {
			t.Fatalf("sensitive value leaked at %q: %#v", key, entry.Fields)
		}
	}
	if entry.Fields[`api\.key`] != redactedValue || entry.Fields[`http.headers`] != redactedValue || entry.Fields[`private\.key`] != redactedValue {
		t.Fatalf("sensitive paths were not redacted: %#v", entry.Fields)
	}
}

func TestHandlerEvictsByTotalSize(t *testing.T) {
	configured := limits{
		messageBytes:    80,
		fieldKeyBytes:   32,
		fieldValueBytes: 20,
		entryBytes:      180,
		totalBytes:      360,
		fields:          2,
		sourceBytes:     20,
	}
	handler := newHandler(10, configured)
	logger := slog.New(handler)
	for index := 0; index < 5; index++ {
		logger.Info(strings.Repeat(strconv.Itoa(index), 80), "detail", strings.Repeat("x", 20))
	}

	handler.ring.mu.RLock()
	count := handler.ring.count
	total := handler.ring.totalBytes
	handler.ring.mu.RUnlock()
	if count >= 5 {
		t.Fatalf("total byte limit retained all %d entries", count)
	}
	if total > configured.totalBytes {
		t.Fatalf("ring size = %d, want <= %d", total, configured.totalBytes)
	}
	page := handler.List(Filter{Limit: 10})
	if page.Items[0].ID != 5 {
		t.Fatalf("newest ID = %d, want 5", page.Items[0].ID)
	}
}

func TestListReturnsDetachedFields(t *testing.T) {
	handler := New(2)
	slog.New(handler).Info("message", "field", "original")
	first := handler.List(Filter{Limit: 1})
	first.Items[0].Fields["field"] = "mutated"
	second := handler.List(Filter{Limit: 1})
	if second.Items[0].Fields["field"] != "original" {
		t.Fatalf("stored field was mutated: %#v", second.Items[0].Fields)
	}
}

func TestDerivedHandlersDoNotLeakState(t *testing.T) {
	handler := New(10)
	root := slog.New(handler)
	withAttr := slog.New(handler.WithAttrs([]slog.Attr{slog.String("branch", "attr")}))
	withGroup := slog.New(handler.WithGroup("group"))

	root.Info("root", "value", "root")
	withAttr.Info("attr", "value", "attr")
	withGroup.Info("group", "value", "group")

	rootItem := handler.List(Filter{Query: "root", Limit: 10}).Items[0]
	if rootItem.Fields["value"] != "root" || len(rootItem.Fields) != 1 {
		t.Fatalf("root fields = %#v", rootItem.Fields)
	}
	attrItem := handler.List(Filter{Query: "attr", Limit: 10}).Items[0]
	if attrItem.Fields["branch"] != "attr" || attrItem.Fields["value"] != "attr" {
		t.Fatalf("attr branch fields = %#v", attrItem.Fields)
	}
	groupItem := handler.List(Filter{Query: "group", Limit: 10}).Items[0]
	if groupItem.Fields["group.value"] != "group" || len(groupItem.Fields) != 1 {
		t.Fatalf("group branch fields = %#v", groupItem.Fields)
	}
}

func TestHandlePreservesZeroTimeAndSource(t *testing.T) {
	handler := New(1)
	record := slog.NewRecord(time.Time{}, slog.LevelInfo, "direct", 0)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	item := handler.List(Filter{Limit: 1}).Items[0]
	if !item.Time.IsZero() || item.Source != "" {
		t.Fatalf("zero metadata = time %v, source %q", item.Time, item.Source)
	}
}

func TestTeeHonorsChildLevelsAndPropagatesState(t *testing.T) {
	memory := New(10)
	var output bytes.Buffer
	stdout := slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelError})
	tee := Tee(stdout, memory).
		WithAttrs([]slog.Attr{slog.String("bound", "yes")}).
		WithGroup("worker")
	logger := slog.New(tee)

	logger.Info("informational", "account_id", int64(7))
	logger.Error("failed", "account_id", int64(7))

	page := memory.List(Filter{Limit: 10})
	assertIDs(t, page.Items, 2, 1)
	for _, item := range page.Items {
		if item.Fields["bound"] != "yes" || item.Fields["worker.account_id"] != "7" {
			t.Fatalf("tee fields = %#v", item.Fields)
		}
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"msg":"failed"`) {
		t.Fatalf("stdout output = %q", output.String())
	}
}

func TestTeeJoinsErrorsAndContinuesDispatch(t *testing.T) {
	firstErr := errors.New("first")
	secondErr := errors.New("second")
	first := &errorHandler{err: firstErr}
	second := &errorHandler{err: secondErr}
	handler := Tee(first, second)
	record := slog.NewRecord(testTime(), slog.LevelError, "failed", 0)

	err := handler.Handle(context.Background(), record)
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("joined error = %v", err)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("handler calls = %d, %d; want 1, 1", first.calls, second.calls)
	}
}

func TestTeeClonesAttrsForEachChild(t *testing.T) {
	mutator := &attrOwnershipHandler{mutate: true}
	observer := &attrOwnershipHandler{}
	_ = Tee(mutator, observer).WithAttrs([]slog.Attr{slog.String("original", "value")})
	if len(observer.attrs) != 1 || observer.attrs[0].Key != "original" || observer.attrs[0].Value.String() != "value" {
		t.Fatalf("second child attrs = %#v", observer.attrs)
	}
}

func TestHandlerConcurrentWritesRemainBounded(t *testing.T) {
	const (
		workers   = 10
		perWorker = 100
		capacity  = 500
		pageLimit = 200
	)
	handler := New(capacity)
	logger := slog.New(handler)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		group.Add(1)
		go func() {
			defer group.Done()
			for index := 0; index < perWorker; index++ {
				logger.Info("concurrent", "account_id", worker, "index", index)
			}
		}()
	}
	group.Wait()

	page := handler.List(Filter{Limit: pageLimit})
	if len(page.Items) != pageLimit || !page.HasMore {
		t.Fatalf("page length/has-more = %d/%t", len(page.Items), page.HasMore)
	}
	handler.ring.mu.RLock()
	count := handler.ring.count
	nextID := handler.ring.nextID
	handler.ring.mu.RUnlock()
	if count != capacity || nextID != workers*perWorker {
		t.Fatalf("ring count/next ID = %d/%d, want %d/%d", count, nextID, capacity, workers*perWorker)
	}
}

func TestHandlerConcurrentReadsAndWrites(t *testing.T) {
	handler := New(100)
	logger := slog.New(handler)
	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(5)
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < 1_000; index++ {
			logger.Info("write", "account_id", index%3, "index", index)
		}
	}()
	for reader := 0; reader < 4; reader++ {
		go func() {
			defer group.Done()
			<-start
			accountID := int64(1)
			for index := 0; index < 250; index++ {
				page := handler.List(Filter{AccountID: &accountID, Limit: 10})
				if len(page.Items) > 10 {
					t.Errorf("page returned %d items", len(page.Items))
					return
				}
			}
		}()
	}
	close(start)
	group.Wait()
}

func TestNewRejectsNonPositiveCapacity(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New(0) did not panic")
		}
	}()
	_ = New(0)
}

func TestDefaultTotalLimitRetainsTwoThousandMaxEntries(t *testing.T) {
	if MaxStoredBytes < 2_000*MaxEntryBytes {
		t.Fatalf("MaxStoredBytes = %d, want at least %d", MaxStoredBytes, 2_000*MaxEntryBytes)
	}
}

func assertIDs(t *testing.T, items []Entry, want ...uint64) {
	t.Helper()
	if len(items) != len(want) {
		t.Fatalf("IDs item count = %d, want %d", len(items), len(want))
	}
	for index, id := range want {
		if items[index].ID != id {
			t.Fatalf("ID[%d] = %d, want %d", index, items[index].ID, id)
		}
	}
}

type errorHandler struct {
	err   error
	calls int
}

func (h *errorHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *errorHandler) Handle(context.Context, slog.Record) error {
	h.calls++
	return h.err
}

func (h *errorHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *errorHandler) WithGroup(string) slog.Handler { return h }

type attrOwnershipHandler struct {
	mutate bool
	attrs  []slog.Attr
}

func (h *attrOwnershipHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *attrOwnershipHandler) Handle(context.Context, slog.Record) error { return nil }

func (h *attrOwnershipHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if h.mutate && len(attrs) > 0 {
		attrs[0] = slog.String("mutated", "changed")
	}
	h.attrs = append([]slog.Attr(nil), attrs...)
	return h
}

func (h *attrOwnershipHandler) WithGroup(string) slog.Handler { return h }

func testTime() (value time.Time) {
	return time.Unix(1, 0).UTC()
}

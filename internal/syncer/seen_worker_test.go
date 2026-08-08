package syncer

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/mail"
	"icloud-api/internal/store"
)

type seenDeleteCall struct {
	accountID   int64
	uidValidity uint32
	uids        []uint32
}

type seenWorkerRepo struct {
	mu sync.Mutex

	accounts     map[int64]domain.Account
	tasks        []domain.SeenTask
	listErr      error
	getErr       error
	deleteErr    error
	listCalls    int
	deletes      []seenDeleteCall
	deleteNotify chan seenDeleteCall
}

func (r *seenWorkerRepo) GetAccount(_ context.Context, id int64) (domain.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return domain.Account{}, r.getErr
	}
	account, ok := r.accounts[id]
	if !ok {
		return domain.Account{}, store.ErrNotFound
	}
	return account, nil
}

func (r *seenWorkerRepo) ListSeenTaskAccountIDs(_ context.Context) ([]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listCalls++
	if r.listErr != nil {
		return nil, r.listErr
	}
	result := make([]int64, 0)
	seen := make(map[int64]struct{})
	for _, task := range r.tasks {
		if _, exists := seen[task.AccountID]; exists {
			continue
		}
		seen[task.AccountID] = struct{}{}
		result = append(result, task.AccountID)
	}
	return result, nil
}

func (r *seenWorkerRepo) ListSeenTasks(_ context.Context, accountID int64, limit int) ([]domain.SeenTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	result := make([]domain.SeenTask, 0, limit)
	for _, task := range r.tasks {
		if task.AccountID != accountID {
			continue
		}
		result = append(result, task)
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

func (r *seenWorkerRepo) DeleteSeenTasks(_ context.Context, accountID int64, uidValidity uint32, uids []uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := seenDeleteCall{
		accountID: accountID, uidValidity: uidValidity, uids: append([]uint32(nil), uids...),
	}
	r.deletes = append(r.deletes, call)
	if r.deleteErr != nil {
		return r.deleteErr
	}
	wanted := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		wanted[uid] = struct{}{}
	}
	remaining := r.tasks[:0]
	for _, task := range r.tasks {
		_, deleteTask := wanted[task.UID]
		if task.AccountID != accountID || task.UIDValidity != uidValidity || !deleteTask {
			remaining = append(remaining, task)
		}
	}
	r.tasks = remaining
	if r.deleteNotify != nil {
		r.deleteNotify <- call
	}
	return nil
}

func (r *seenWorkerRepo) snapshot() ([]domain.SeenTask, []seenDeleteCall, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.SeenTask(nil), r.tasks...), append([]seenDeleteCall(nil), r.deletes...), r.listCalls
}

type seenMarkerCall struct {
	accountID   int64
	password    string
	uidValidity uint32
	uids        []uint32
}

type seenMarkerStub struct {
	mu    sync.Mutex
	fn    func(context.Context, seenMarkerCall) error
	calls []seenMarkerCall
}

func (m *seenMarkerStub) MarkSeen(ctx context.Context, account domain.Account, password string, uidValidity uint32, uids []uint32) error {
	call := seenMarkerCall{
		accountID: account.ID, password: password, uidValidity: uidValidity,
		uids: append([]uint32(nil), uids...),
	}
	m.mu.Lock()
	m.calls = append(m.calls, call)
	fn := m.fn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, call)
	}
	return nil
}

func (m *seenMarkerStub) snapshot() []seenMarkerCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]seenMarkerCall(nil), m.calls...)
}

type seenLockerStub struct {
	mu          sync.Mutex
	ids         []int64
	beforeEnter func(context.Context) error
}

func (l *seenLockerStub) WithAccountIMAPSlot(ctx context.Context, accountID int64, operation func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	l.ids = append(l.ids, accountID)
	beforeEnter := l.beforeEnter
	l.mu.Unlock()
	if beforeEnter != nil {
		if err := beforeEnter(ctx); err != nil {
			return err
		}
	}
	return operation()
}

func (l *seenLockerStub) snapshot() []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int64(nil), l.ids...)
}

func newSeenWorkerForTest(repo *seenWorkerRepo, marker *seenMarkerStub, locker *seenLockerStub) *SeenWorker {
	return NewSeenWorker(
		repo,
		cipherFunc(func(value string) (string, error) { return "plain:" + value, nil }),
		marker,
		locker,
		discardLogger(),
		time.Hour,
	)
}

func TestSeenWorkerGroupsTasksAndDeletesSuccessfulBatches(t *testing.T) {
	repo := &seenWorkerRepo{
		accounts: map[int64]domain.Account{
			1: {ID: 1, Enabled: true, PasswordCiphertext: "one"},
			2: {ID: 2, Enabled: true, PasswordCiphertext: "two"},
		},
		tasks: []domain.SeenTask{
			{AccountID: 1, UIDValidity: 10, UID: 4},
			{AccountID: 2, UIDValidity: 20, UID: 7},
			{AccountID: 1, UIDValidity: 10, UID: 9},
			{AccountID: 1, UIDValidity: 10, UID: 4},
		},
	}
	marker := &seenMarkerStub{}
	locker := &seenLockerStub{}
	worker := newSeenWorkerForTest(repo, marker, locker)

	more, err := worker.processPending(context.Background())
	if err != nil {
		t.Fatalf("process pending seen tasks: %v", err)
	}
	if more {
		t.Fatal("short batch reported more work")
	}
	wantMarkerCalls := []seenMarkerCall{
		{accountID: 1, password: "plain:one", uidValidity: 10, uids: []uint32{4, 9}},
		{accountID: 2, password: "plain:two", uidValidity: 20, uids: []uint32{7}},
	}
	calls := marker.snapshot()
	sort.Slice(calls, func(i, j int) bool { return calls[i].accountID < calls[j].accountID })
	if !reflect.DeepEqual(calls, wantMarkerCalls) {
		t.Fatalf("marker calls = %#v, want %#v", calls, wantMarkerCalls)
	}
	ids := locker.snapshot()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if !reflect.DeepEqual(ids, []int64{1, 2}) {
		t.Fatalf("locked account IDs = %v, want [1 2]", ids)
	}
	tasks, deletes, _ := repo.snapshot()
	if len(tasks) != 0 {
		t.Fatalf("tasks remain after success: %#v", tasks)
	}
	wantDeletes := []seenDeleteCall{
		{accountID: 1, uidValidity: 10, uids: []uint32{4, 9}},
		{accountID: 2, uidValidity: 20, uids: []uint32{7}},
	}
	sort.Slice(deletes, func(i, j int) bool { return deletes[i].accountID < deletes[j].accountID })
	if !reflect.DeepEqual(deletes, wantDeletes) {
		t.Fatalf("delete calls = %#v, want %#v", deletes, wantDeletes)
	}
}

func TestSeenWorkerDeletesUIDValidityMismatchButRetainsTransientFailure(t *testing.T) {
	transient := errors.New("temporary IMAP failure")
	repo := &seenWorkerRepo{
		accounts: map[int64]domain.Account{
			1: {ID: 1, Enabled: true, PasswordCiphertext: "one"},
			2: {ID: 2, Enabled: true, PasswordCiphertext: "two"},
			3: {ID: 3, Enabled: true, PasswordCiphertext: "three"},
		},
		tasks: []domain.SeenTask{
			{AccountID: 1, UIDValidity: 10, UID: 4},
			{AccountID: 2, UIDValidity: 20, UID: 7},
			{AccountID: 3, UIDValidity: 30, UID: 8},
		},
	}
	marker := &seenMarkerStub{fn: func(_ context.Context, call seenMarkerCall) error {
		switch call.accountID {
		case 1:
			return &mail.UIDValidityMismatchError{Expected: 10, Actual: 11}
		case 2:
			return transient
		default:
			return nil
		}
	}}
	worker := newSeenWorkerForTest(repo, marker, &seenLockerStub{})

	more, err := worker.processPending(context.Background())
	if !errors.Is(err, transient) {
		t.Fatalf("process error = %v, want transient marker error", err)
	}
	if more {
		t.Fatal("failed batch must wait before retrying")
	}
	tasks, deletes, _ := repo.snapshot()
	if want := []domain.SeenTask{{AccountID: 2, UIDValidity: 20, UID: 7}}; !reflect.DeepEqual(tasks, want) {
		t.Fatalf("remaining tasks = %#v, want %#v", tasks, want)
	}
	wantDeletes := []seenDeleteCall{
		{accountID: 1, uidValidity: 10, uids: []uint32{4}},
		{accountID: 3, uidValidity: 30, uids: []uint32{8}},
	}
	sort.Slice(deletes, func(i, j int) bool { return deletes[i].accountID < deletes[j].accountID })
	if !reflect.DeepEqual(deletes, wantDeletes) {
		t.Fatalf("delete calls = %#v, want %#v", deletes, wantDeletes)
	}
}

func TestSeenWorkerFailedAccountBacksOffWhileHealthyAccountDrains(t *testing.T) {
	transient := errors.New("temporary IMAP failure")
	const healthyTaskCount = defaultSeenTaskBatchSize + 44
	tasks := make([]domain.SeenTask, 0, defaultSeenTaskBatchSize+healthyTaskCount)
	for uid := uint32(1); uid <= defaultSeenTaskBatchSize; uid++ {
		tasks = append(tasks, domain.SeenTask{AccountID: 1, UIDValidity: 10, UID: uid})
	}
	for uid := uint32(1); uid <= healthyTaskCount; uid++ {
		tasks = append(tasks, domain.SeenTask{AccountID: 2, UIDValidity: 20, UID: uid})
	}
	repo := &seenWorkerRepo{
		accounts: map[int64]domain.Account{
			1: {ID: 1, Enabled: true, PasswordCiphertext: "one"},
			2: {ID: 2, Enabled: true, PasswordCiphertext: "two"},
		},
		tasks: tasks,
	}
	marker := &seenMarkerStub{fn: func(_ context.Context, call seenMarkerCall) error {
		if call.accountID == 1 {
			return transient
		}
		return nil
	}}
	worker := newSeenWorkerForTest(repo, marker, &seenLockerStub{})

	more, err := worker.processPending(context.Background())
	if !errors.Is(err, transient) {
		t.Fatalf("process error = %v, want transient marker error", err)
	}
	if !more {
		t.Fatal("healthy full account batch did not request immediate continuation")
	}
	remaining, _, _ := repo.snapshot()
	if want := defaultSeenTaskBatchSize + healthyTaskCount - defaultSeenTaskBatchSize; len(remaining) != want {
		t.Fatalf("remaining tasks after first batch = %d, want %d", len(remaining), want)
	}

	more, err = worker.processPending(context.Background())
	if err != nil {
		t.Fatalf("continue healthy account while failed account backs off: %v", err)
	}
	if more {
		t.Fatal("final short healthy batch reported more work")
	}
	remaining, _, _ = repo.snapshot()
	if len(remaining) != defaultSeenTaskBatchSize {
		t.Fatalf("remaining tasks after healthy drain = %d, want failed batch %d", len(remaining), defaultSeenTaskBatchSize)
	}
	for _, task := range remaining {
		if task.AccountID != 1 {
			t.Fatalf("healthy account task was not drained: %#v", task)
		}
	}
	calls := marker.snapshot()
	var failedCalls, healthyCalls []seenMarkerCall
	for _, call := range calls {
		if call.accountID == 1 {
			failedCalls = append(failedCalls, call)
		} else if call.accountID == 2 {
			healthyCalls = append(healthyCalls, call)
		}
	}
	if len(calls) != 3 || len(failedCalls) != 1 || len(failedCalls[0].uids) != defaultSeenTaskBatchSize ||
		len(healthyCalls) != 2 || len(healthyCalls[0].uids) != defaultSeenTaskBatchSize || len(healthyCalls[1].uids) != 44 {
		t.Fatalf("marker calls = %#v, want one failed batch then two healthy batches", calls)
	}
}

func TestSeenWorkerBlockedAccountDoesNotDelayHealthyAccount(t *testing.T) {
	transient := errors.New("temporary IMAP failure")
	healthyDeleted := make(chan seenDeleteCall, 1)
	repo := &seenWorkerRepo{
		accounts: map[int64]domain.Account{
			1: {ID: 1, Enabled: true, PasswordCiphertext: "one"},
			2: {ID: 2, Enabled: true, PasswordCiphertext: "two"},
		},
		tasks: []domain.SeenTask{
			{AccountID: 1, UIDValidity: 10, UID: 4},
			{AccountID: 2, UIDValidity: 20, UID: 7},
		},
		deleteNotify: healthyDeleted,
	}
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSlow) }) }
	defer release()
	marker := &seenMarkerStub{fn: func(ctx context.Context, call seenMarkerCall) error {
		switch call.accountID {
		case 1:
			close(slowStarted)
			select {
			case <-releaseSlow:
				return transient
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}}
	worker := newSeenWorkerForTest(repo, marker, &seenLockerStub{})
	result := make(chan error, 1)
	go func() {
		_, err := worker.processPending(context.Background())
		result <- err
	}()

	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow account did not start")
	}
	select {
	case deleted := <-healthyDeleted:
		if deleted.accountID != 2 {
			t.Fatalf("task deleted while slow account blocked = %#v, want account 2", deleted)
		}
	case <-time.After(time.Second):
		t.Fatal("healthy account did not finish while the other account was blocked")
	}
	remaining, _, _ := repo.snapshot()
	if len(remaining) != 1 || remaining[0].AccountID != 1 {
		t.Fatalf("tasks while slow account is blocked = %#v, want only account 1", remaining)
	}

	release()
	select {
	case err := <-result:
		if !errors.Is(err, transient) {
			t.Fatalf("process error = %v, want transient marker error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not finish after releasing slow account")
	}
}

func TestSeenWorkerDisabledAccountPrefixDoesNotStarveLaterAccount(t *testing.T) {
	const disabledAccounts = 256
	accounts := make(map[int64]domain.Account, disabledAccounts+1)
	tasks := make([]domain.SeenTask, 0, disabledAccounts+1)
	for id := int64(1); id <= disabledAccounts; id++ {
		accounts[id] = domain.Account{ID: id, Enabled: false, PasswordCiphertext: "disabled"}
		tasks = append(tasks, domain.SeenTask{AccountID: id, UIDValidity: 10, UID: 1})
	}
	laterAccountID := int64(disabledAccounts + 1)
	accounts[laterAccountID] = domain.Account{
		ID: laterAccountID, Enabled: true, PasswordCiphertext: "later",
	}
	tasks = append(tasks, domain.SeenTask{AccountID: laterAccountID, UIDValidity: 20, UID: 2})
	repo := &seenWorkerRepo{accounts: accounts, tasks: tasks}
	marker := &seenMarkerStub{}
	worker := newSeenWorkerForTest(repo, marker, &seenLockerStub{})

	more, err := worker.processPending(context.Background())
	if err == nil {
		t.Fatal("disabled account prefix did not report deferred work")
	}
	if more {
		t.Fatal("disabled accounts must return to the poll interval")
	}
	if calls := marker.snapshot(); len(calls) != 1 || calls[0].accountID != laterAccountID {
		t.Fatalf("marker calls = %#v, want only later account %d", calls, laterAccountID)
	}
	remaining, _, _ := repo.snapshot()
	if len(remaining) != disabledAccounts {
		t.Fatalf("remaining tasks = %d, want disabled prefix of %d", len(remaining), disabledAccounts)
	}
	for _, task := range remaining {
		if task.AccountID == laterAccountID {
			t.Fatalf("later account task was starved: %#v", task)
		}
	}
}

func TestSeenWorkerRetainsTasksWhenAccountDisabledOrDeleteFails(t *testing.T) {
	deleteErr := errors.New("database unavailable")
	repo := &seenWorkerRepo{
		accounts: map[int64]domain.Account{
			1: {ID: 1, Enabled: false, PasswordCiphertext: "one"},
			2: {ID: 2, Enabled: true, PasswordCiphertext: "two"},
		},
		tasks: []domain.SeenTask{
			{AccountID: 1, UIDValidity: 10, UID: 4},
			{AccountID: 2, UIDValidity: 20, UID: 7},
		},
		deleteErr: deleteErr,
	}
	marker := &seenMarkerStub{}
	worker := newSeenWorkerForTest(repo, marker, &seenLockerStub{})

	more, err := worker.processPending(context.Background())
	if err == nil || !errors.Is(err, deleteErr) {
		t.Fatalf("process error = %v, want disabled and delete errors", err)
	}
	if more {
		t.Fatal("failed work must not immediately retry")
	}
	if calls := marker.snapshot(); len(calls) != 1 || calls[0].accountID != 2 {
		t.Fatalf("marker calls = %#v, want only enabled account 2", calls)
	}
	tasks, _, _ := repo.snapshot()
	if len(tasks) != 2 {
		t.Fatalf("failed tasks were removed: %#v", tasks)
	}
}

func TestSeenWorkerRunDrainsFullBatchesAndStopsOnCancellation(t *testing.T) {
	repo := &seenWorkerRepo{
		accounts: map[int64]domain.Account{1: {ID: 1, Enabled: true, PasswordCiphertext: "one"}},
		tasks: []domain.SeenTask{
			{AccountID: 1, UIDValidity: 10, UID: 4},
			{AccountID: 1, UIDValidity: 10, UID: 7},
		},
	}
	marker := &seenMarkerStub{}
	worker := newSeenWorkerForTest(repo, marker, &seenLockerStub{})
	worker.batchSize = 1
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		tasks, _, calls := repo.snapshot()
		if len(tasks) == 0 && calls >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker did not drain immediately: tasks=%#v list_calls=%d", tasks, calls)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
	if calls := marker.snapshot(); len(calls) != 2 {
		t.Fatalf("marker calls = %d, want 2", len(calls))
	}
}

func TestSeenWorkerNotifyIsNonBlockingAndWakesIdleWorker(t *testing.T) {
	repo := &seenWorkerRepo{
		accounts: map[int64]domain.Account{1: {ID: 1, Enabled: true, PasswordCiphertext: "one"}},
	}
	worker := newSeenWorkerForTest(repo, &seenMarkerStub{}, &seenLockerStub{})
	worker.Notify()
	worker.Notify()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		_, _, calls := repo.snapshot()
		if calls >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notification did not wake worker, list calls = %d", calls)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestSeenWorkerOperationTimeoutStartsInsideAccountIMAPSlot(t *testing.T) {
	repo := &seenWorkerRepo{
		accounts: map[int64]domain.Account{1: {ID: 1, Enabled: true, PasswordCiphertext: "one"}},
		tasks:    []domain.SeenTask{{AccountID: 1, UIDValidity: 10, UID: 4}},
	}
	releaseSlot := make(chan struct{})
	locker := &seenLockerStub{beforeEnter: func(ctx context.Context) error {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			return errors.New("account IMAP acquisition context has no deadline")
		}
		select {
		case <-releaseSlot:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	remainingBudget := make(chan time.Duration, 1)
	marker := &seenMarkerStub{fn: func(ctx context.Context, _ seenMarkerCall) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("marker context has no deadline")
		}
		remainingBudget <- time.Until(deadline)
		<-ctx.Done()
		return ctx.Err()
	}}
	worker := newSeenWorkerForTest(repo, marker, locker)
	operationTimeout := 40 * time.Millisecond
	worker.SetAcquireTimeout(4 * operationTimeout)
	worker.SetOperationTimeout(operationTimeout)
	result := make(chan error, 1)
	go func() {
		_, err := worker.processPending(context.Background())
		result <- err
	}()

	deadline := time.Now().Add(time.Second)
	for len(locker.snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker did not reach the account IMAP slot")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(2 * operationTimeout)
	close(releaseSlot)

	select {
	case remaining := <-remainingBudget:
		if remaining < operationTimeout/2 {
			t.Fatalf("operation budget after slot acquisition = %v, want at least %v", remaining, operationTimeout/2)
		}
	case <-time.After(time.Second):
		t.Fatal("marker did not start after slot release")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("process error = %v, want %v", err, context.DeadlineExceeded)
		}
	case <-time.After(time.Second):
		t.Fatal("operation timeout did not cancel the marker")
	}
	remaining, _, _ := repo.snapshot()
	if len(remaining) != 1 {
		t.Fatalf("timed out task was removed: %#v", remaining)
	}
}

func TestSeenWorkerAccountIMAPAcquisitionHasFiniteTimeout(t *testing.T) {
	repo := &seenWorkerRepo{
		accounts: map[int64]domain.Account{1: {ID: 1, Enabled: true, PasswordCiphertext: "one"}},
		tasks:    []domain.SeenTask{{AccountID: 1, UIDValidity: 10, UID: 4}},
	}
	locker := &seenLockerStub{beforeEnter: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	marker := &seenMarkerStub{}
	worker := newSeenWorkerForTest(repo, marker, locker)
	worker.SetAcquireTimeout(20 * time.Millisecond)
	worker.SetOperationTimeout(time.Second)

	started := time.Now()
	more, err := worker.processPending(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquisition error = %v, want %v", err, context.DeadlineExceeded)
	}
	if more {
		t.Fatal("timed out acquisition requested an immediate retry")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("acquisition timeout took too long: %v", elapsed)
	}
	if calls := marker.snapshot(); len(calls) != 0 {
		t.Fatalf("marker ran without acquiring resources: %#v", calls)
	}
	remaining, _, _ := repo.snapshot()
	if len(remaining) != 1 {
		t.Fatalf("acquisition timeout removed task: %#v", remaining)
	}
}

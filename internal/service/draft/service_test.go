package draft

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

type fakeStore struct {
	mu sync.Mutex

	created     *domain.Draft
	createErr   error
	nextID      int64
	list        []domain.Draft
	listStatus  string
	listErr     error
	draft       domain.Draft
	attachments []domain.DraftAttachment
	getErr      error
	updated     *domain.Draft
	updatedRev  int64
	updateErr   error
	deletedID   int64
	deleteCalls int
	deleteErr   error
}

func (f *fakeStore) CreateDraft(_ context.Context, draft *domain.Draft) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.nextID++
	draft.ID = f.nextID
	copied := *draft
	f.created = &copied
	return nil
}
func (f *fakeStore) ListDrafts(_ context.Context, status string) ([]domain.Draft, error) {
	f.listStatus = status
	return f.list, f.listErr
}
func (f *fakeStore) GetDraft(context.Context, int64) (domain.Draft, []domain.DraftAttachment, error) {
	return f.draft, f.attachments, f.getErr
}
func (f *fakeStore) UpdateDraft(_ context.Context, draft *domain.Draft, revision int64) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	draft.Revision = revision + 1
	copied := *draft
	f.updated, f.updatedRev = &copied, revision
	return nil
}
func (f *fakeStore) DeleteDraft(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	f.deletedID = id
	return f.deleteErr
}

type fakeRemote struct {
	mu         sync.Mutex
	syncedIDs  []int64
	syncSignal chan int64
	syncErr    error

	deleteCalls int
	deletedID   int64
	deleteErr   error
}

func (f *fakeRemote) SyncDraft(_ context.Context, id int64) error {
	f.mu.Lock()
	f.syncedIDs = append(f.syncedIDs, id)
	signal := f.syncSignal
	f.mu.Unlock()
	if signal != nil {
		signal <- id
	}
	return f.syncErr
}
func (f *fakeRemote) DeleteRemoteDraft(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	f.deletedID = id
	return f.deleteErr
}
func (f *fakeRemote) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.syncedIDs)
}

type recorder struct {
	mu     sync.Mutex
	events []ports.Event
}

func (r *recorder) Publish(event ports.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}
func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func TestCreateRequiresAnAccount(t *testing.T) {
	store := &fakeStore{}
	service := New(store, &recorder{}, nil)
	for _, id := range []int64{0, -1} {
		if _, err := service.Create(context.Background(), Input{AccountID: id}); !errors.Is(err, ports.ErrInvalidInput) {
			t.Fatalf("account_id %d: err = %v, want ErrInvalidInput", id, err)
		}
	}
	if store.created != nil {
		t.Fatal("a draft was stored without an account")
	}
}

func TestCreateRejectsBadRecipientsInEveryField(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input Input
	}{
		{"to", Input{AccountID: 1, To: []string{"not-an-address"}}},
		{"cc", Input{AccountID: 1, To: []string{"a@b.com"}, CC: []string{"@nope"}}},
		{"bcc", Input{AccountID: 1, To: []string{"a@b.com"}, BCC: []string{"x y z"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			service := New(store, &recorder{}, nil)
			if _, err := service.Create(context.Background(), tc.input); !errors.Is(err, ports.ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
			if store.created != nil {
				t.Fatal("an invalid recipient still reached the store")
			}
		})
	}
}

func TestCreateStoresNormalizedDraft(t *testing.T) {
	store := &fakeStore{}
	events := &recorder{}
	service := New(store, events, nil)

	draft, err := service.Create(context.Background(), Input{
		AccountID: 5, To: []string{"a@b.com", "Someone <c@d.com>"}, Subject: "  spaced  ", BodyText: "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Subject != "spaced" {
		t.Fatalf("subject = %q, want trimmed", draft.Subject)
	}
	if draft.Status != "draft" || draft.RemoteSyncState != "dirty" || draft.Revision != 1 {
		t.Fatalf("draft = %+v", draft)
	}
	if draft.RFCMessageID == "" {
		t.Fatal("no RFC message id was minted")
	}
	// Empty groups must serialise as [] rather than null: the UI parses this with
	// JSON.parse and treats a null as a broken draft.
	if draft.CCJSON != "[]" || draft.BCCJSON != "[]" {
		t.Fatalf("cc=%q bcc=%q, want []", draft.CCJSON, draft.BCCJSON)
	}
	var to []string
	if err := json.Unmarshal([]byte(draft.ToJSON), &to); err != nil || len(to) != 2 {
		t.Fatalf("to = %q (%v)", draft.ToJSON, err)
	}
	if events.count() != 1 {
		t.Fatalf("events = %d, want 1", events.count())
	}
}

func TestCreatePropagatesStoreErrorWithoutPublishing(t *testing.T) {
	events := &recorder{}
	service := New(&fakeStore{createErr: errors.New("busy")}, events, nil)
	if _, err := service.Create(context.Background(), Input{AccountID: 1}); err == nil {
		t.Fatal("expected the store error")
	}
	if events.count() != 0 {
		t.Fatal("published an event for a draft that was never stored")
	}
}

func TestUpdateForwardsTheExpectedRevision(t *testing.T) {
	store := &fakeStore{draft: domain.Draft{ID: 8, Revision: 3}}
	events := &recorder{}
	service := New(store, events, nil)

	draft, err := service.Update(context.Background(), 8, 3, Input{To: []string{"a@b.com"}, Subject: " s ", BodyText: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if store.updatedRev != 3 {
		t.Fatalf("revision passed to the store = %d, want 3", store.updatedRev)
	}
	if draft.Subject != "s" || draft.Revision != 4 {
		t.Fatalf("draft = %+v", draft)
	}
	if store.updated.UpdatedAt == 0 {
		t.Fatal("updated_at was not refreshed")
	}
	if events.count() != 1 {
		t.Fatalf("events = %d", events.count())
	}
}

func TestUpdateValidatesBeforeReadingTheDraft(t *testing.T) {
	store := &fakeStore{getErr: errors.New("should not be reached")}
	service := New(store, &recorder{}, nil)
	if _, err := service.Update(context.Background(), 1, 1, Input{To: []string{"bad"}}); !errors.Is(err, ports.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestUpdateSurfacesConflict(t *testing.T) {
	store := &fakeStore{draft: domain.Draft{ID: 1, Revision: 5}, updateErr: ports.Conflictf("revision conflict")}
	events := &recorder{}
	service := New(store, events, nil)
	if _, err := service.Update(context.Background(), 1, 2, Input{}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if events.count() != 0 {
		t.Fatal("published an event for a conflicted update")
	}
}

func TestUpdateSurfacesMissingDraft(t *testing.T) {
	service := New(&fakeStore{getErr: ports.NotFoundf("draft 4 not found")}, &recorder{}, nil)
	if _, err := service.Update(context.Background(), 4, 1, Input{}); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestListAndGetPassThrough(t *testing.T) {
	store := &fakeStore{
		list:        []domain.Draft{{ID: 1}, {ID: 2}},
		draft:       domain.Draft{ID: 3, Subject: "s"},
		attachments: []domain.DraftAttachment{{ID: 9}},
	}
	service := New(store, &recorder{}, nil)

	items, err := service.List(context.Background(), "failed")
	if err != nil || len(items) != 2 {
		t.Fatalf("List = %+v, %v", items, err)
	}
	if store.listStatus != "failed" {
		t.Fatalf("status filter = %q", store.listStatus)
	}

	draft, attachments, err := service.Get(context.Background(), 3)
	if err != nil || draft.Subject != "s" || len(attachments) != 1 {
		t.Fatalf("Get = %+v, %+v, %v", draft, attachments, err)
	}
}

func TestListPropagatesError(t *testing.T) {
	service := New(&fakeStore{listErr: errors.New("boom")}, &recorder{}, nil)
	if _, err := service.List(context.Background(), ""); err == nil {
		t.Fatal("expected the store error")
	}
}

// A draft the provider no longer has is the outcome the caller wanted, so only a
// different remote failure may block the local delete.
func TestDeleteTreatsRemoteNotFoundAsSuccess(t *testing.T) {
	store := &fakeStore{}
	remote := &fakeRemote{deleteErr: ports.NotFoundf("no such remote draft")}
	service := New(store, &recorder{}, remote)

	if err := service.Delete(context.Background(), 6); err != nil {
		t.Fatalf("Delete = %v, want nil", err)
	}
	if store.deleteCalls != 1 || store.deletedID != 6 {
		t.Fatalf("local delete calls=%d id=%d", store.deleteCalls, store.deletedID)
	}
}

func TestDeleteStopsOnOtherRemoteFailures(t *testing.T) {
	store := &fakeStore{}
	remote := &fakeRemote{deleteErr: errors.New("connection reset")}
	service := New(store, &recorder{}, remote)

	if err := service.Delete(context.Background(), 6); err == nil {
		t.Fatal("expected the remote failure to block the local delete")
	}
	if store.deleteCalls != 0 {
		t.Fatal("deleted locally while the provider still holds the draft")
	}
}

func TestDeleteWithoutRemoteDeletesLocally(t *testing.T) {
	store := &fakeStore{}
	service := New(store, &recorder{}, nil)
	if err := service.Delete(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if store.deleteCalls != 1 {
		t.Fatalf("local delete calls = %d", store.deleteCalls)
	}
}

func TestDeletePropagatesLocalError(t *testing.T) {
	service := New(&fakeStore{deleteErr: errors.New("busy")}, &recorder{}, nil)
	if err := service.Delete(context.Background(), 1); err == nil {
		t.Fatal("expected the local delete error")
	}
}

// The remote sync is debounced: rapid edits must collapse into one push, not one
// per keystroke, or every autosave becomes an IMAP APPEND.
func TestScheduleDebouncesRapidEdits(t *testing.T) {
	store := &fakeStore{draft: domain.Draft{ID: 1, Revision: 1}}
	remote := &fakeRemote{syncSignal: make(chan int64, 4)}
	service := New(store, &recorder{}, remote)
	// Shorten the debounce so the test does not wait five seconds.
	service.syncDelay = 40 * time.Millisecond

	for i := 0; i < 4; i++ {
		if _, err := service.Update(context.Background(), 1, int64(i+1), Input{}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case id := <-remote.syncSignal:
		if id != 1 {
			t.Fatalf("synced draft %d, want 1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the debounced sync never ran")
	}
	// Give any surplus timer the chance to fire before asserting there was none.
	time.Sleep(120 * time.Millisecond)
	if got := remote.syncCount(); got != 1 {
		t.Fatalf("SyncDraft ran %d times for 4 rapid edits, want 1", got)
	}
	service.mu.Lock()
	timers := len(service.timers)
	service.mu.Unlock()
	if timers != 0 {
		t.Fatalf("%d timers left behind after the sync ran", timers)
	}
}

// Close is what shutdown calls. A push that fires after the supervisor stopped
// would dial a runtime that is already torn down.
func TestCloseStopsPendingPushes(t *testing.T) {
	remote := &fakeRemote{}
	service := New(&fakeStore{}, &recorder{}, remote)
	service.syncDelay = 60 * time.Millisecond

	for i := 0; i < 3; i++ {
		if _, err := service.Create(context.Background(), Input{AccountID: 1}); err != nil {
			t.Fatal(err)
		}
	}
	service.mu.Lock()
	pending := len(service.timers)
	service.mu.Unlock()
	if pending != 3 {
		t.Fatalf("pending timers = %d, want 3", pending)
	}

	service.Close()
	service.mu.Lock()
	left := len(service.timers)
	service.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d timers survived Close", left)
	}
	time.Sleep(200 * time.Millisecond)
	if got := remote.syncCount(); got != 0 {
		t.Fatalf("SyncDraft ran %d times after Close", got)
	}
}

// Close has to reach a push that has already left its timer, which is the case
// Stop cannot cover: it reports false for a fired timer and cannot interrupt the
// IMAP APPEND it started. Before Close cancelled the push context, that call held
// a 45s budget off context.Background() and could still be talking to the
// supervisor it was shut down alongside.
func TestCloseCancelsAnInFlightPush(t *testing.T) {
	entered := make(chan struct{})
	observed := make(chan error, 1)
	remote := &blockingRemote{entered: entered, observed: observed}
	service := New(&fakeStore{}, &recorder{}, remote)
	service.syncDelay = time.Millisecond

	if _, err := service.Create(context.Background(), Input{AccountID: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the debounced push never started")
	}

	service.Close()
	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight push saw %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel the in-flight push: it can outlive the supervisor it calls")
	}
}

// blockingRemote parks inside SyncDraft until its context ends, then reports what
// ended it.
type blockingRemote struct {
	entered  chan struct{}
	observed chan error
	once     sync.Once
}

func (b *blockingRemote) SyncDraft(ctx context.Context, _ int64) error {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	b.observed <- ctx.Err()
	return ctx.Err()
}

func (b *blockingRemote) DeleteRemoteDraft(context.Context, int64) error { return nil }

func TestScheduleIsSkippedWithoutARemote(t *testing.T) {
	service := New(&fakeStore{}, &recorder{}, nil)
	if _, err := service.Create(context.Background(), Input{AccountID: 1}); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.timers) != 0 {
		t.Fatalf("scheduled %d timers with no remote configured", len(service.timers))
	}
}

// Delete has to cancel a pending push. Without it the timer fires after the row is
// gone and re-uploads a draft the user just deleted.
func TestDeleteCancelsAPendingSync(t *testing.T) {
	store := &fakeStore{}
	remote := &fakeRemote{}
	service := New(store, &recorder{}, remote)
	service.syncDelay = 60 * time.Millisecond

	if _, err := service.Create(context.Background(), Input{AccountID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := remote.syncCount(); got != 0 {
		t.Fatalf("SyncDraft ran %d times for a deleted draft", got)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.timers) != 0 {
		t.Fatalf("%d timers survived the delete", len(service.timers))
	}
}

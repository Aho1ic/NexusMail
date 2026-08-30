package message

import (
	"context"
	"errors"
	"testing"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

type fakeStore struct {
	page        ports.MessagePage
	listErr     error
	message     domain.Message
	attachments []domain.Attachment
	getErr      error

	patched    ports.MessagePatch
	patchedID  int64
	patchErr   error
	bulkIDs    []int64
	bulkErr    error
	unread     []int64
	unreadErr  error
	unreadArgs struct {
		filter ports.MessageFilter
		limit  int
	}
}

func (f *fakeStore) ListMessages(_ context.Context, filter ports.MessageFilter) (ports.MessagePage, error) {
	f.unreadArgs.filter = filter
	return f.page, f.listErr
}
func (f *fakeStore) GetMessage(context.Context, int64) (domain.Message, []domain.Attachment, error) {
	return f.message, f.attachments, f.getErr
}
func (f *fakeStore) UpdateMessage(_ context.Context, id int64, patch ports.MessagePatch) (domain.Message, error) {
	f.patchedID, f.patched = id, patch
	return f.message, f.patchErr
}
func (f *fakeStore) UpdateMessages(_ context.Context, ids []int64, _ ports.MessagePatch) error {
	f.bulkIDs = ids
	return f.bulkErr
}
func (f *fakeStore) UnreadMessageIDs(_ context.Context, filter ports.MessageFilter, limit int) ([]int64, error) {
	f.unreadArgs.filter, f.unreadArgs.limit = filter, limit
	return f.unread, f.unreadErr
}

type fakeRemote struct {
	flagCalls   int
	flagID      int64
	flagRead    *bool
	flagStarred *bool
	flagErr     error

	archiveCalls int
	archiveErr   error

	bulkIn   []int64
	bulkOut  []int64
	bulkErr  error
	bulkSeen bool
}

func (f *fakeRemote) SetFlags(_ context.Context, id int64, read, starred *bool) error {
	f.flagCalls++
	f.flagID, f.flagRead, f.flagStarred = id, read, starred
	return f.flagErr
}
func (f *fakeRemote) SetSeenBulk(_ context.Context, ids []int64) ([]int64, error) {
	f.bulkSeen = true
	f.bulkIn = ids
	return f.bulkOut, f.bulkErr
}
func (f *fakeRemote) Archive(context.Context, int64) error {
	f.archiveCalls++
	return f.archiveErr
}

type recorder struct{ events []ports.Event }

func (r *recorder) Publish(event ports.Event) { r.events = append(r.events, event) }

func ptr[T any](value T) *T { return &value }

func TestListAndGetPassThrough(t *testing.T) {
	store := &fakeStore{
		page:        ports.MessagePage{Items: []domain.Message{{ID: 7}}, UnreadTotal: 3},
		message:     domain.Message{ID: 7, Subject: "hi"},
		attachments: []domain.Attachment{{ID: 1}},
	}
	service := New(store, nil, &recorder{})

	page, err := service.List(context.Background(), ports.MessageFilter{Folder: "inbox", Limit: 40})
	if err != nil || page.UnreadTotal != 3 || len(page.Items) != 1 {
		t.Fatalf("List = %+v, %v", page, err)
	}
	if store.unreadArgs.filter.Folder != "inbox" {
		t.Fatalf("filter not forwarded: %+v", store.unreadArgs.filter)
	}

	message, attachments, err := service.Get(context.Background(), 7)
	if err != nil || message.Subject != "hi" || len(attachments) != 1 {
		t.Fatalf("Get = %+v, %+v, %v", message, attachments, err)
	}
}

func TestListPropagatesError(t *testing.T) {
	want := errors.New("boom")
	service := New(&fakeStore{listErr: want}, nil, &recorder{})
	if _, err := service.List(context.Background(), ports.MessageFilter{}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestPatchRejectsEmptyPatch(t *testing.T) {
	store := &fakeStore{}
	remote := &fakeRemote{}
	events := &recorder{}
	service := New(store, remote, events)

	_, err := service.Patch(context.Background(), 1, ports.MessagePatch{}, false)
	if !errors.Is(err, ports.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	// An empty patch must not reach the provider or the database at all.
	if remote.flagCalls != 0 || remote.archiveCalls != 0 || store.patchedID != 0 || len(events.events) != 0 {
		t.Fatalf("empty patch had side effects: remote=%+v store=%d events=%d", remote, store.patchedID, len(events.events))
	}
}

// The provider is the source of truth, so a flag it rejected must not be written
// locally: the row would read as changed here and unchanged everywhere else.
func TestPatchDoesNotWriteLocallyWhenRemoteFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote *fakeRemote
		patch  ports.MessagePatch
		arch   bool
	}{
		{"flags", &fakeRemote{flagErr: errors.New("STORE rejected")}, ports.MessagePatch{IsRead: ptr(true)}, false},
		{"archive", &fakeRemote{archiveErr: errors.New("no archive folder")}, ports.MessagePatch{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			events := &recorder{}
			service := New(store, tc.remote, events)
			if _, err := service.Patch(context.Background(), 9, tc.patch, tc.arch); err == nil {
				t.Fatal("expected the remote failure to surface")
			}
			if store.patchedID != 0 {
				t.Fatal("local row was updated after the provider refused")
			}
			if len(events.events) != 0 {
				t.Fatal("an event was published for a change that did not happen")
			}
		})
	}
}

func TestPatchAppliesFlagsThenArchiveThenPublishes(t *testing.T) {
	store := &fakeStore{message: domain.Message{ID: 9, IsRead: true}}
	remote := &fakeRemote{}
	events := &recorder{}
	service := New(store, remote, events)

	got, err := service.Patch(context.Background(), 9, ports.MessagePatch{IsRead: ptr(true), IsStarred: ptr(false)}, true)
	if err != nil || got.ID != 9 {
		t.Fatalf("Patch = %+v, %v", got, err)
	}
	if remote.flagCalls != 1 || remote.flagID != 9 || *remote.flagRead != true || *remote.flagStarred != false {
		t.Fatalf("SetFlags not called with the patch: %+v", remote)
	}
	if remote.archiveCalls != 1 {
		t.Fatalf("Archive calls = %d, want 1", remote.archiveCalls)
	}
	if store.patchedID != 9 || store.patched.IsRead == nil {
		t.Fatalf("store patch = %d, %+v", store.patchedID, store.patched)
	}
	if len(events.events) != 1 || events.events[0].Type != "MESSAGE_UPDATED" {
		t.Fatalf("events = %+v", events.events)
	}
}

// Archive alone must not send a STORE: there are no flags to set, and QQ answers
// an empty STORE with an error.
func TestPatchArchiveOnlySkipsSetFlags(t *testing.T) {
	remote := &fakeRemote{}
	service := New(&fakeStore{}, remote, &recorder{})
	if _, err := service.Patch(context.Background(), 4, ports.MessagePatch{}, true); err != nil {
		t.Fatal(err)
	}
	if remote.flagCalls != 0 {
		t.Fatalf("SetFlags called %d times for an archive-only patch", remote.flagCalls)
	}
}

// A nil remote is the offline-account path: the local write still has to happen.
func TestPatchWithoutRemoteStillWritesLocally(t *testing.T) {
	store := &fakeStore{message: domain.Message{ID: 3}}
	events := &recorder{}
	service := New(store, nil, events)
	if _, err := service.Patch(context.Background(), 3, ports.MessagePatch{IsStarred: ptr(true)}, false); err != nil {
		t.Fatal(err)
	}
	if store.patchedID != 3 || len(events.events) != 1 {
		t.Fatalf("store=%d events=%d", store.patchedID, len(events.events))
	}
}

func TestPatchDoesNotPublishWhenTheWriteFails(t *testing.T) {
	events := &recorder{}
	service := New(&fakeStore{patchErr: errors.New("busy")}, nil, events)
	if _, err := service.Patch(context.Background(), 1, ports.MessagePatch{IsRead: ptr(true)}, false); err == nil {
		t.Fatal("expected the write error to surface")
	}
	if len(events.events) != 0 {
		t.Fatalf("published %d events for a failed write", len(events.events))
	}
}

func TestMarkReadNoUnreadIsANoOp(t *testing.T) {
	store := &fakeStore{}
	remote := &fakeRemote{}
	events := &recorder{}
	service := New(store, remote, events)

	result, err := service.MarkRead(context.Background(), ports.MessageFilter{Folder: "inbox"})
	if err != nil || result.Updated != 0 || result.Capped {
		t.Fatalf("MarkRead = %+v, %v", result, err)
	}
	if remote.bulkSeen || store.bulkIDs != nil || len(events.events) != 0 {
		t.Fatal("an empty view still touched the provider, the store or the hub")
	}
}

// Only the ids the provider accepted may be written locally, so a partial
// acceptance reports what actually changed rather than what was attempted.
func TestMarkReadWritesOnlyTheAcceptedIDs(t *testing.T) {
	store := &fakeStore{unread: []int64{1, 2, 3}}
	remote := &fakeRemote{bulkOut: []int64{1, 3}, bulkErr: errors.New("account 2 offline")}
	events := &recorder{}
	service := New(store, remote, events)

	result, err := service.MarkRead(context.Background(), ports.MessageFilter{})
	if err == nil {
		t.Fatal("a partial failure must still surface the provider error")
	}
	if result.Updated != 2 {
		t.Fatalf("Updated = %d, want 2", result.Updated)
	}
	if len(store.bulkIDs) != 2 || store.bulkIDs[0] != 1 || store.bulkIDs[1] != 3 {
		t.Fatalf("wrote %v, want [1 3]", store.bulkIDs)
	}
}

func TestMarkReadTotalRemoteFailureWritesNothing(t *testing.T) {
	store := &fakeStore{unread: []int64{1, 2}}
	remote := &fakeRemote{bulkOut: nil, bulkErr: errors.New("all accounts offline")}
	events := &recorder{}
	service := New(store, remote, events)

	result, err := service.MarkRead(context.Background(), ports.MessageFilter{})
	if err == nil || result.Updated != 0 {
		t.Fatalf("MarkRead = %+v, %v", result, err)
	}
	if store.bulkIDs != nil || len(events.events) != 0 {
		t.Fatal("nothing was accepted yet rows were written")
	}
}

// One aggregate event, never one per message: the hub drops a client whose
// buffer fills, so a per-message burst would disconnect the UI awaiting the result.
func TestMarkReadPublishesOneAggregateEvent(t *testing.T) {
	store := &fakeStore{unread: []int64{1, 2, 3, 4}}
	events := &recorder{}
	service := New(store, nil, events)

	result, err := service.MarkRead(context.Background(), ports.MessageFilter{})
	if err != nil || result.Updated != 4 {
		t.Fatalf("MarkRead = %+v, %v", result, err)
	}
	if len(events.events) != 1 {
		t.Fatalf("published %d events, want exactly 1", len(events.events))
	}
	data, ok := events.events[0].Data.(map[string]any)
	if !ok || data["bulk"] != true || data["count"] != 4 {
		t.Fatalf("event data = %+v", events.events[0].Data)
	}
}

func TestMarkReadReportsCappedAtTheLimit(t *testing.T) {
	ids := make([]int64, markReadLimit)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	store := &fakeStore{unread: ids}
	service := New(store, nil, &recorder{})

	result, err := service.MarkRead(context.Background(), ports.MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Capped || result.Updated != markReadLimit {
		t.Fatalf("MarkRead = %+v, want capped with %d updated", result, markReadLimit)
	}
	if store.unreadArgs.limit != markReadLimit {
		t.Fatalf("limit passed to the store = %d, want %d", store.unreadArgs.limit, markReadLimit)
	}
}

func TestMarkReadOneShortOfTheLimitIsNotCapped(t *testing.T) {
	ids := make([]int64, markReadLimit-1)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	service := New(&fakeStore{unread: ids}, nil, &recorder{})
	result, err := service.MarkRead(context.Background(), ports.MessageFilter{})
	if err != nil || result.Capped {
		t.Fatalf("MarkRead = %+v, %v", result, err)
	}
}

func TestMarkReadSurfacesStoreErrors(t *testing.T) {
	t.Run("lookup", func(t *testing.T) {
		service := New(&fakeStore{unreadErr: errors.New("query failed")}, nil, &recorder{})
		if _, err := service.MarkRead(context.Background(), ports.MessageFilter{}); err == nil {
			t.Fatal("expected the lookup error")
		}
	})
	t.Run("write", func(t *testing.T) {
		events := &recorder{}
		service := New(&fakeStore{unread: []int64{1}, bulkErr: errors.New("busy")}, nil, events)
		result, err := service.MarkRead(context.Background(), ports.MessageFilter{})
		if err == nil || result.Updated != 0 {
			t.Fatalf("MarkRead = %+v, %v", result, err)
		}
		if len(events.events) != 0 {
			t.Fatal("published an event for a failed write")
		}
	})
}

// The view the button acts on must be the view the list shows, filter for filter.
func TestMarkReadForwardsTheWholeFilter(t *testing.T) {
	store := &fakeStore{}
	service := New(store, nil, &recorder{})
	filter := ports.MessageFilter{AccountID: ptr(int64(4)), MailboxID: ptr(int64(9)), Folder: "inbox", Query: "invoice"}
	if _, err := service.MarkRead(context.Background(), filter); err != nil {
		t.Fatal(err)
	}
	got := store.unreadArgs.filter
	if *got.AccountID != 4 || *got.MailboxID != 9 || got.Folder != "inbox" || got.Query != "invoice" {
		t.Fatalf("filter = %+v", got)
	}
}

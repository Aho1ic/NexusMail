//go:build sqlite_fts5

package send

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

// The existing suite drives the SMTP state machine with no provider attached, so
// the post-delivery half of complete() — removing the remote draft copy and filing
// the message in Sent — was never reached. That half is where the provider
// differences live: QQ and 163 need the APPEND, Gmail and Outlook file sent mail
// themselves and would end up with two copies.

// remoteSpy records the provider coordination a completed send performs.
type remoteSpy struct {
	mu        sync.Mutex
	deleted   []int64
	appended  [][]byte
	deleteErr error
	appendErr error
}

func (r *remoteSpy) DeleteRemoteDraft(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, id)
	return r.deleteErr
}

func (r *remoteSpy) AppendSent(_ context.Context, _ int64, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appended = append(r.appended, append([]byte(nil), payload...))
	return r.appendErr
}

func (r *remoteSpy) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.deleted), len(r.appended)
}

// withRemote rebuilds the harness worker with a provider attached. The harness
// constructor passes nil, which is what leaves the coordination unexercised.
func (h *harness) withRemote(t *testing.T, spy *remoteSpy) *remoteSpy {
	t.Helper()
	h.worker.remoteDraft = spy
	return spy
}

func TestCompleteRemovesTheRemoteDraftAndFilesSent(t *testing.T) {
	h := newHarness(t, &backend{})
	spy := h.withRemote(t, &remoteSpy{})
	draft := h.queueDraft(t, "remote-coordination", "body")

	h.worker.deliver(context.Background(), draft.ID)

	if status := h.draft(t, draft.ID).Status; status != "sent" {
		t.Fatalf("status = %q, want sent", status)
	}
	deleted, appended := spy.counts()
	if deleted != 1 || spy.deleted[0] != draft.ID {
		t.Fatalf("DeleteRemoteDraft calls = %v", spy.deleted)
	}
	// qq's preset has ServerSavesSent false, so the payload has to be filed.
	if appended != 1 {
		t.Fatalf("AppendSent calls = %d, want 1", appended)
	}
	if !strings.Contains(string(spy.appended[0]), "Subject: remote-coordination") {
		t.Fatalf("the appended payload is not the sent message:\n%s", spy.appended[0])
	}
	// The payload filed remotely must be the one that was transmitted, or the copy
	// in Sent is not what the recipient received. Compared with trailing
	// whitespace normalised: the SMTP server sees the body after the dot that ends
	// DATA has been consumed, so the final line break differs.
	transmitted := h.backend.messages()
	if len(transmitted) != 1 {
		t.Fatalf("%d messages transmitted", len(transmitted))
	}
	if strings.TrimRight(transmitted[0], "\r\n") != strings.TrimRight(string(spy.appended[0]), "\r\n") {
		t.Fatalf("the payload appended to Sent differs from the transmitted message:\nsent:\n%s\nappended:\n%s", transmitted[0], spy.appended[0])
	}
}

// A provider that files sent mail itself must not be appended to, or the user sees
// the message twice in Sent.
func TestCompleteSkipsAppendWhenTheServerSavesSent(t *testing.T) {
	h := newHarness(t, &backend{})
	spy := h.withRemote(t, &remoteSpy{})
	_ = spy
	// gmail's preset sets ServerSavesSent; only the provider column is read here,
	// so the account keeps its password auth and its loopback endpoint.
	if err := h.setProvider(t, "gmail"); err != nil {
		t.Fatal(err)
	}
	draft := h.queueDraft(t, "server-files-it", "body")

	h.worker.deliver(context.Background(), draft.ID)

	if status := h.draft(t, draft.ID).Status; status != "sent" {
		t.Fatalf("status = %q, want sent", status)
	}
	deleted, appended := spy.counts()
	if deleted != 1 {
		t.Fatalf("DeleteRemoteDraft calls = %d, want 1", deleted)
	}
	if appended != 0 {
		t.Fatalf("AppendSent was called %d times for a provider that files sent mail itself", appended)
	}
}

// A failed APPEND must not undo a successful send. The message stays sent and the
// drift is surfaced on the draft instead of being retried, which would deliver it
// a second time.
func TestFailedAppendAnnotatesButKeepsTheSend(t *testing.T) {
	h := newHarness(t, &backend{})
	spy := h.withRemote(t, &remoteSpy{appendErr: errors.New("mailbox is unavailable")})
	draft := h.queueDraft(t, "append-failed", "body")

	h.worker.deliver(context.Background(), draft.ID)

	if _, appended := spy.counts(); appended != 1 {
		t.Fatalf("AppendSent was attempted %d times, want 1", appended)
	}
	current := h.draft(t, draft.ID)
	if current.Status != "sent" {
		t.Fatalf("status = %q, want sent despite the failed append", current.Status)
	}
	if current.LastError == nil || !strings.Contains(*current.LastError, "message sent") {
		t.Fatalf("last_error = %v, want it to say the message was sent", current.LastError)
	}
	if current.SentAt == nil {
		t.Fatal("sent_at is unset on a message that was delivered")
	}
	if len(h.backend.messages()) != 1 {
		t.Fatalf("the message was transmitted %d times", len(h.backend.messages()))
	}
}

// The same rule for the remote draft delete: it is cleanup, not part of delivery.
func TestFailedRemoteDeleteKeepsTheSend(t *testing.T) {
	h := newHarness(t, &backend{})
	spy := h.withRemote(t, &remoteSpy{deleteErr: errors.New("no such draft")})
	draft := h.queueDraft(t, "delete-failed", "body")

	h.worker.deliver(context.Background(), draft.ID)

	if status := h.draft(t, draft.ID).Status; status != "sent" {
		t.Fatalf("status = %q, want sent despite the failed remote delete", status)
	}
	if _, appended := spy.counts(); appended != 1 {
		t.Fatalf("AppendSent calls = %d; a failed delete must not skip filing Sent", appended)
	}
}

// TestDeliverSendsAttachments covers compose()'s attachment path: the blob is
// opened, becomes a MIME part, and its file descriptor is closed once the payload
// is built rather than being held for the whole SMTP round trip.
func TestDeliverSendsAttachments(t *testing.T) {
	h := newHarness(t, &backend{})
	draft := h.queueDraft(t, "with-attachment", "see attached")
	h.attach(t, draft.ID, "notes.txt", []byte("attachment payload"))
	h.attach(t, draft.ID, "second.bin", []byte{0x00, 0xff, 0x0d, 0x0a, '.', 0x0d, 0x0a})

	h.worker.deliver(context.Background(), draft.ID)

	if status := h.draft(t, draft.ID).Status; status != "sent" {
		t.Fatalf("status = %q, want sent", status)
	}
	if len(h.backend.messages()) != 1 {
		t.Fatalf("%d messages transmitted", len(h.backend.messages()))
	}
	message := h.backend.messages()[0]
	for _, filename := range []string{"notes.txt", "second.bin"} {
		if !strings.Contains(message, filename) {
			t.Fatalf("%s is missing from the message:\n%s", filename, message)
		}
	}
	if !strings.Contains(message, "multipart/mixed") {
		t.Fatalf("a message with attachments is not multipart:\n%s", message)
	}
}

// A blob that vanished between the upload and the send must fail the draft rather
// than transmit a message with a silently missing attachment.
func TestDeliverFailsWhenAnAttachmentBlobIsGone(t *testing.T) {
	h := newHarness(t, &backend{})
	draft := h.queueDraft(t, "missing-blob", "body")
	blobID := h.attach(t, draft.ID, "gone.txt", []byte("temporary"))
	h.removeBlobFile(t, blobID)

	h.worker.deliver(context.Background(), draft.ID)

	current := h.draft(t, draft.ID)
	if current.Status == "sent" {
		t.Fatal("the draft was marked sent with an unreadable attachment")
	}
	if len(h.backend.messages()) != 0 {
		t.Fatal("a message was transmitted despite the unreadable attachment")
	}
	if current.LastError == nil {
		t.Fatal("no error was recorded")
	}
}

// TestStartDrainsTheQueueAndStops covers the worker loop: a queued draft is
// delivered without anyone calling deliver directly, and the loop exits with its
// context so the process can shut down.
func TestStartDrainsTheQueueAndStops(t *testing.T) {
	h := newHarness(t, &backend{})
	h.withRemote(t, &remoteSpy{})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		h.worker.Start(ctx)
		close(stopped)
	}()

	draft := h.newDraft(t, "via-the-loop", "body")
	if err := h.worker.Queue(context.Background(), draft.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if h.draft(t, draft.ID).Status == "sent" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status := h.draft(t, draft.ID).Status; status != "sent" {
		t.Fatalf("status = %q after the loop ran, want sent", status)
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return when its context was cancelled")
	}
}

// TestStartPicksUpDueRetries covers the ticker branch: a draft whose retry time has
// passed is delivered without anything enqueueing it.
func TestStartPicksUpDueRetries(t *testing.T) {
	h := newHarness(t, &backend{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	draft := h.queueDraft(t, "due-retry", "body")
	past := time.Now().Add(-time.Minute).UnixMilli()
	if err := h.repo.SetDraftDelivery(context.Background(), draft.ID, "retry_wait", 1, &past, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	go h.worker.Start(ctx)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if h.draft(t, draft.ID).Status == "sent" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("a due retry was never picked up; status = %q", h.draft(t, draft.ID).Status)
}

// TestStartRecoversInterruptedSends covers RecoverSendingDrafts on startup: a
// draft left in 'sending' by a crash may already have been delivered, so it
// becomes terminal rather than being sent again.
func TestStartRecoversInterruptedSends(t *testing.T) {
	h := newHarness(t, &backend{})
	draft := h.queueDraft(t, "interrupted", "body")
	if _, _, err := h.repo.ClaimSendableDraft(context.Background(), draft.ID); err != nil {
		t.Fatal(err)
	}
	if status := h.draft(t, draft.ID).Status; status != "sending" {
		t.Fatalf("status = %q, want sending before recovery", status)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go h.worker.Start(ctx)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if h.draft(t, draft.ID).Status != "sending" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if status := h.draft(t, draft.ID).Status; status != "unknown" {
		t.Fatalf("status = %q after recovery, want unknown", status)
	}
	if len(h.backend.messages()) != 0 {
		t.Fatal("an interrupted send was retransmitted, which can deliver the message twice")
	}
}

// Queue coalesces, so a draft already waiting is not delivered twice, and a
// nonexistent draft is a classified not-found rather than a panic.
func TestQueueCoalescesAndValidates(t *testing.T) {
	h := newHarness(t, &backend{})
	draft := h.newDraft(t, "coalesce", "body")
	if err := h.worker.Queue(context.Background(), draft.ID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	// enqueue is idempotent while the id is still waiting, so the ticker rediscovering
	// a due draft cannot deliver it twice.
	for range 5 {
		h.worker.enqueue(draft.ID)
	}
	h.worker.queuedMu.Lock()
	pending := len(h.worker.queued)
	h.worker.queuedMu.Unlock()
	if pending != 1 {
		t.Fatalf("%d pending entries for one draft, want 1", pending)
	}
	if depth := len(h.worker.queue); depth != 1 {
		t.Fatalf("the queue holds %d copies of one draft, want 1", depth)
	}

	if err := h.worker.Queue(context.Background(), draft.ID+9999); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("queueing an unknown draft returned %v, want a not-found error", err)
	}
}

// enqueue drops rather than blocks when the queue is full: the ticker re-discovers
// due drafts, so a dropped signal costs latency, while blocking here would stall
// whichever request queued it.
func TestEnqueueDropsWhenFull(t *testing.T) {
	h := newHarness(t, &backend{})
	for id := range int64(cap(h.worker.queue)) {
		h.worker.enqueue(id + 1)
	}
	if len(h.worker.queue) != cap(h.worker.queue) {
		t.Fatalf("queue holds %d, want %d", len(h.worker.queue), cap(h.worker.queue))
	}
	done := make(chan struct{})
	go func() {
		h.worker.enqueue(999999)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enqueue blocked on a full queue")
	}
	// The dropped id must not be left marked as pending, or it can never be
	// enqueued again.
	h.worker.queuedMu.Lock()
	_, stuck := h.worker.queued[999999]
	h.worker.queuedMu.Unlock()
	if stuck {
		t.Fatal("a dropped id stayed marked as pending and can never be queued again")
	}
}

// A draft whose body is longer than the snippet limit must still produce a valid
// sent row, with the snippet truncated rather than the whole body stored twice.
func TestSentSnippetIsTruncated(t *testing.T) {
	h := newHarness(t, &backend{})
	long := strings.Repeat("长文本 body ", 200)
	draft := h.queueDraft(t, "long-body", long)

	h.worker.deliver(context.Background(), draft.ID)

	if status := h.draft(t, draft.ID).Status; status != "sent" {
		t.Fatalf("status = %q", status)
	}
	page, err := h.repo.ListMessages(context.Background(), ports.MessageFilter{AccountID: &h.account.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("%d sent rows, want 1", len(page.Items))
	}
	if runes := len([]rune(page.Items[0].Snippet)); runes > 241 {
		t.Fatalf("snippet is %d runes, want it truncated", runes)
	}
	// The feed projection omits body_text, so the full body is read from the detail
	// path — the snippet being short must not mean the body was truncated too.
	sent, _, err := h.repo.GetMessage(context.Background(), page.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if sent.BodyText != long {
		t.Fatalf("body_text is %d bytes, want the full %d", len(sent.BodyText), len(long))
	}
}

// Recipients across To, CC and BCC must all receive the message, and BCC must not
// appear in the transmitted headers.
func TestBCCIsDeliveredButNotDisclosed(t *testing.T) {
	h := newHarness(t, &backend{})
	draft := h.queueDraft(t, "bcc-handling", "body")
	if err := h.setRecipients(t, draft.ID, `["to@example.com"]`, `["cc@example.com"]`, `["hidden@example.com"]`); err != nil {
		t.Fatal(err)
	}

	h.worker.deliver(context.Background(), draft.ID)

	if status := h.draft(t, draft.ID).Status; status != "sent" {
		t.Fatalf("status = %q: %v", status, h.draft(t, draft.ID).LastError)
	}
	if len(h.backend.messages()) != 1 {
		t.Fatalf("%d messages transmitted", len(h.backend.messages()))
	}
	message := h.backend.messages()[0]
	if strings.Contains(message, "hidden@example.com") {
		t.Fatalf("the BCC address appears in the transmitted headers:\n%s", message)
	}
	for _, visible := range []string{"to@example.com", "cc@example.com"} {
		if !strings.Contains(message, visible) {
			t.Fatalf("%s is missing from the headers", visible)
		}
	}
	// All three still have to be envelope recipients, or the BCC never arrives.
	if got := h.backend.recipientCount(); got != 3 {
		t.Fatalf("%d envelope recipients, want 3", got)
	}
}

// A draft with no recipient at all must fail locally rather than open a connection.
func TestDeliverRejectsEmptyRecipients(t *testing.T) {
	h := newHarness(t, &backend{})
	draft := h.queueDraftWith(t, "no-recipients", "body", `[]`)

	h.worker.deliver(context.Background(), draft.ID)

	current := h.draft(t, draft.ID)
	if current.Status == "sent" {
		t.Fatal("a draft with no recipients was marked sent")
	}
	if len(h.backend.messages()) != 0 {
		t.Fatal("a message with no recipients was transmitted")
	}
	if current.LastError == nil || !strings.Contains(strings.ToLower(*current.LastError), "recipient") {
		t.Fatalf("last_error = %v, want it to name the missing recipient", current.LastError)
	}
}

// The account row is read at delivery time, so a draft whose account disappeared
// must fail rather than panic on a zero-value account.
func TestDeliverFailsWhenTheAccountIsGone(t *testing.T) {
	h := newHarness(t, &backend{})
	draft := h.queueDraft(t, "orphan", "body")
	h.detachAccount(t, draft.ID)

	h.worker.deliver(context.Background(), draft.ID)

	if status := h.draft(t, draft.ID).Status; status == "sent" {
		t.Fatal("a draft with no account was marked sent")
	}
	if len(h.backend.messages()) != 0 {
		t.Fatal("a message was transmitted for a missing account")
	}
}

// deliver is the entry point the queue calls, so an id that no longer exists must
// be a no-op rather than a panic or a stuck queue entry.
func TestDeliverIgnoresUnknownDrafts(t *testing.T) {
	h := newHarness(t, &backend{})
	h.worker.deliver(context.Background(), 987654)
	if statuses := h.statuses(); statuses != "" {
		t.Fatalf("events published for an unknown draft: %s", statuses)
	}
}

// Concurrent deliveries of the same draft must transmit once: ClaimSendableDraft
// is the guard, and a message sent twice is not recoverable.
func TestConcurrentDeliveryTransmitsOnce(t *testing.T) {
	h := newHarness(t, &backend{})
	h.withRemote(t, &remoteSpy{})
	draft := h.queueDraft(t, "single-delivery", "body")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.worker.deliver(context.Background(), draft.ID)
		}()
	}
	wg.Wait()

	if status := h.draft(t, draft.ID).Status; status != "sent" {
		t.Fatalf("status = %q", status)
	}
	if len(h.backend.messages()) != 1 {
		t.Fatalf("the message was transmitted %d times, want 1", len(h.backend.messages()))
	}
	page, err := h.repo.ListMessages(context.Background(), ports.MessageFilter{AccountID: &h.account.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("%d rows in Sent, want 1", len(page.Items))
	}
}

// newDraft creates a draft in 'draft' status. queueDraft moves it straight to
// 'queued', which Queue rejects, so the tests that exercise Queue itself start
// from the state a freshly composed draft is really in.
func (h *harness) newDraft(t *testing.T, subject, body string) domain.Draft {
	t.Helper()
	now := time.Now().UnixMilli()
	draft := domain.Draft{
		AccountID: h.account.ID, Revision: 1, RFCMessageID: "<" + subject + "@example.com>",
		ToJSON: `["recipient@example.com"]`, CCJSON: "[]", BCCJSON: "[]",
		Subject: subject, BodyText: body, Status: "draft", RemoteSyncState: "dirty",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := h.repo.CreateDraft(context.Background(), &draft); err != nil {
		t.Fatal(err)
	}
	h.events.reset()
	return draft
}

// attach stores a real blob and links it to the draft, returning the blob id.
func (h *harness) attach(t *testing.T, draftID int64, filename string, content []byte) int64 {
	t.Helper()
	ctx := context.Background()
	blob, err := h.blobs.Put(ctx, bytes.NewReader(content), "durable")
	if err != nil {
		t.Fatal(err)
	}
	attachment := domain.DraftAttachment{
		DraftID: draftID, BlobID: blob.ID, Filename: filename,
		ContentType: "application/octet-stream", SizeBytes: int64(len(content)),
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := h.repo.AddDraftAttachment(ctx, &attachment); err != nil {
		t.Fatal(err)
	}
	return blob.ID
}

// removeBlobFile deletes the stored bytes while leaving the row, which is the
// state a manual cleanup or a lost volume produces.
func (h *harness) removeBlobFile(t *testing.T, blobID int64) {
	t.Helper()
	blob, err := h.repo.GetBlob(context.Background(), blobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.blobRoot(t), blob.StorageKey)); err != nil {
		t.Fatal(err)
	}
}

// blobRoot derives the blob directory from the database path, which share a
// parent in the harness layout.
func (h *harness) blobRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(h.dbPath), "blobs")
}

// exec runs one statement against the same database file. Used for the two columns
// that have no setter because production never changes them: the account's
// provider, which comes from a compiled-in preset, and a draft's account link.
func (h *harness) exec(t *testing.T, statement string, args ...any) error {
	t.Helper()
	database, err := sql.Open("sqlite3", "file:"+h.dbPath+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return err
	}
	defer database.Close()
	_, err = database.Exec(statement, args...)
	return err
}

func (h *harness) setProvider(t *testing.T, provider string) error {
	t.Helper()
	return h.exec(t, `UPDATE accounts SET provider = ? WHERE id = ?`, provider, h.account.ID)
}

func (h *harness) setRecipients(t *testing.T, draftID int64, to, cc, bcc string) error {
	t.Helper()
	return h.exec(t, `UPDATE drafts SET to_json = ?, cc_json = ?, bcc_json = ? WHERE id = ?`, to, cc, bcc, draftID)
}

// detachAccount repoints the draft at an account id that does not exist. Foreign
// keys are on, so the account row is deleted instead and the draft follows it only
// if the schema cascades — which is why the draft is re-created rather than moved.
func (h *harness) detachAccount(t *testing.T, draftID int64) {
	t.Helper()
	if err := h.exec(t, `PRAGMA foreign_keys=off; UPDATE drafts SET account_id = 999999 WHERE id = ?`, draftID); err != nil {
		t.Fatal(err)
	}
}

// recipientCount reports how many RCPT commands the server saw, which is the only
// way to observe that a BCC address was an envelope recipient without appearing in
// the headers.
func (b *backend) recipientCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recipients
}

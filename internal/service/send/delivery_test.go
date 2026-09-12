//go:build sqlite_fts5

package send

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"nexusmail/internal/domain"
	mailbuilder "nexusmail/internal/mail"
	"nexusmail/internal/ports"

	gosmtp "github.com/emersion/go-smtp"
)

// Everything in this file is about the same invariant: once the provider has
// accepted the message, nothing that happens afterwards may put the draft back
// into a state the queue will claim. A second transmission cannot be recalled, so
// every local and remote bookkeeping failure past that point is drift to report,
// not work to retry.

// failingSentStore makes the local Sent write fail while leaving every other
// method real: a full disk or a WAL conflict lands exactly here, after the message
// has already left.
type failingSentStore struct {
	Store
	err error
}

func (f failingSentStore) CreateSentMessage(context.Context, *domain.Message, int64) error {
	return f.err
}

// A failed local Sent write used to call fail(..., temporary: true), which set
// retry_wait plus next_attempt_at. The draft then became due, was claimed, and the
// same message was transmitted a second time — to recipients who cannot unsee it.
func TestLocalSentWriteFailureDoesNotResend(t *testing.T) {
	h := newHarness(t, &backend{})
	h.worker.repo = failingSentStore{Store: h.worker.repo, err: errors.New("disk is full")}
	draft := h.queueDraft(t, "sent-write-fails", "body")

	h.worker.deliver(context.Background(), draft.ID)

	stored := h.draft(t, draft.ID)
	if stored.Status != "sent" {
		t.Fatalf("status = %q, want sent: the message was delivered (%s)", stored.Status, errText(stored))
	}
	if stored.NextAttemptAt != nil {
		t.Fatalf("a delivered message was scheduled for another attempt at %d", *stored.NextAttemptAt)
	}
	if stored.SentAt == nil {
		t.Fatal("sent_at is unset on a message the provider accepted")
	}
	// The user has to be able to tell "delivered" from "delivered, and the local
	// copy is missing" — the Sent folder will not show this message.
	if stored.LastError == nil || !strings.Contains(*stored.LastError, "message sent") {
		t.Fatalf("last_error = %v, want it to say the message was sent", stored.LastError)
	}
	if !strings.Contains(*stored.LastError, "disk is full") {
		t.Fatalf("last_error = %q, want the local write failure surfaced", *stored.LastError)
	}

	// The retry sweep is the path that resent it: a draft carrying next_attempt_at
	// becomes due on its own, with no user action.
	due, err := h.repo.ListDueDraftIDs(context.Background(), time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range due {
		if id == draft.ID {
			t.Fatal("a delivered draft is queued for another delivery")
		}
	}
	// And the wire is what actually matters. The backoff is rewound first, because
	// ClaimSendableDraft holds a retry_wait draft until next_attempt_at has elapsed:
	// without this the second delivery is refused for a reason that has nothing to
	// do with the message having been sent, and the resend goes unobserved until the
	// 5s rung comes due in production.
	if err := h.exec(t, `UPDATE drafts SET next_attempt_at = 1 WHERE id = ? AND next_attempt_at IS NOT NULL`, draft.ID); err != nil {
		t.Fatal(err)
	}
	h.worker.deliver(context.Background(), draft.ID)
	if count := len(h.backend.messages()); count != 1 {
		t.Fatalf("the message was transmitted %d times, want 1", count)
	}
}

// Shutdown cancels the worker context while a delivery is in flight. The SMTP
// client runs on socket deadlines, so the message still lands; every write that
// follows used to fail on the cancelled context, leaving the draft in 'sending'
// for RecoverSendingDrafts to downgrade to 'unknown'. The user then sees "result
// unknown" for a message that was delivered, and resends it by hand.
func TestDeliveryOutcomeSurvivesShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancelled once the server holds the body, which is the window the SMTP client
	// drives on deadlines rather than on ctx.
	h := newHarness(t, &backend{onData: cancel})
	h.withRemote(t, &remoteSpy{})
	draft := h.queueDraft(t, "shutdown-race", "body")

	h.worker.deliver(ctx, draft.ID)

	if ctx.Err() == nil {
		t.Fatal("the context was not cancelled, so the shutdown race was not exercised")
	}
	if count := len(h.backend.messages()); count != 1 {
		t.Fatalf("the server received %d messages, want 1", count)
	}
	stored := h.draft(t, draft.ID)
	if stored.Status != "sent" {
		t.Fatalf("status = %q, want sent: the message was delivered (%s)", stored.Status, errText(stored))
	}
	if stored.SentAt == nil {
		t.Fatal("sent_at is unset on a message the provider accepted")
	}
	// The next boot's recovery sweep only touches 'sending'. This is the assertion
	// that fails when the outcome write is left on the cancelled context.
	if err := h.repo.RecoverSendingDrafts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := h.draft(t, draft.ID); after.Status != "sent" {
		t.Fatalf("status after recovery = %q; a delivered message was reported as unknown", after.Status)
	}
}

// The same rule for a classified failure: a shutdown landing between the
// provider's refusal and our write must not lose the classification. 4xx is
// retryable, 'unknown' is terminal, so the downgrade costs the user a send that
// would have gone through on its own.
func TestTemporaryFailureOutcomeSurvivesShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, &backend{rcptErr: &gosmtp.SMTPError{Code: 451, Message: "try again later"}})
	// RCPT fails before DATA, so the onData hook never runs. Cancelling once the
	// account has been read puts the cancellation in the same window: the claim is
	// done, and everything after it runs on socket deadlines rather than on ctx.
	h.worker.repo = cancellingStore{Store: h.worker.repo, after: cancel}
	draft := h.queueDraft(t, "shutdown-during-4xx", "body")

	h.worker.deliver(ctx, draft.ID)

	if ctx.Err() == nil {
		t.Fatal("the context was not cancelled, so the shutdown race was not exercised")
	}
	stored := h.draft(t, draft.ID)
	if stored.Status != "retry_wait" {
		t.Fatalf("status = %q, want retry_wait (%s)", stored.Status, errText(stored))
	}
	if stored.NextAttemptAt == nil {
		t.Fatal("the retry schedule was lost")
	}
	if stored.LastSMTPCode == nil || *stored.LastSMTPCode != 451 {
		t.Fatalf("smtp code = %v, want 451: the classification itself was lost", stored.LastSMTPCode)
	}
}

// cancellingStore cancels once the account has been read, which is the last point
// before the conversation starts and the earliest one where a shutdown can no
// longer stop the delivery.
type cancellingStore struct {
	Store
	after func()
}

func (c cancellingStore) GetAccount(ctx context.Context, id int64) (domain.Account, error) {
	account, err := c.Store.GetAccount(ctx, id)
	c.after()
	return account, err
}

// refusingBlobs fails every read and counts the attempts, so a test can prove a
// guard ran before any attachment was touched.
type refusingBlobs struct {
	ports.BlobStore
	mu    sync.Mutex
	opens int
}

func (r *refusingBlobs) Open(context.Context, domain.BlobObject) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opens++
	return nil, errors.New("blob store must not be reached")
}

func (r *refusingBlobs) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.opens
}

// compose drains every attachment into one in-memory payload, so the len(message)
// guard only fires after all of them have been buffered and base64-expanded. The
// upload ceiling is per-request and says nothing about their sum, so N attachments
// at the limit each used to be read in full before the send was refused.
func TestOversizeAttachmentsFailBeforeAnyBlobIsRead(t *testing.T) {
	h := newHarness(t, &backend{})
	blobs := &refusingBlobs{BlobStore: h.blobs}
	h.worker.blobs = blobs
	h.worker.maxBytes = 1 << 20
	draft := h.queueDraft(t, "oversize-attachments", "body")
	// Four rows of 300 KB: under the ceiling individually, over it together, and
	// over it again by the base64 expansion alone.
	for i := range 4 {
		h.attachWithSize(t, draft.ID, fmt.Sprintf("part-%d.bin", i), 300<<10)
	}

	h.worker.deliver(context.Background(), draft.ID)

	if opens := blobs.count(); opens != 0 {
		t.Fatalf("%d attachment blobs were opened before the size guard ran", opens)
	}
	stored := h.draft(t, draft.ID)
	if stored.Status != "failed" {
		t.Fatalf("status = %q, want failed (%s)", stored.Status, errText(stored))
	}
	if stored.NextAttemptAt != nil {
		t.Fatal("an oversize draft was scheduled for a retry it can never pass")
	}
	if stored.LastError == nil || !strings.Contains(*stored.LastError, "attachments exceed") {
		t.Fatalf("last_error = %v, want it to name the attachments", stored.LastError)
	}
	if count := len(h.backend.messages()); count != 0 {
		t.Fatalf("%d messages were transmitted", count)
	}
}

// The base64 expansion has to be charged, or a draft that clears the pre-check is
// still refused after assembly — having been read into memory in full, which is
// what the pre-check exists to avoid. 800 KB of raw attachment is under a 1 MB
// ceiling; encoded it is not.
func TestAttachmentBudgetChargesTheBase64Expansion(t *testing.T) {
	h := newHarness(t, &backend{})
	blobs := &refusingBlobs{BlobStore: h.blobs}
	h.worker.blobs = blobs
	h.worker.maxBytes = 1 << 20
	draft := h.queueDraft(t, "base64-expansion", "body")
	h.attachWithSize(t, draft.ID, "one.bin", 800<<10)

	h.worker.deliver(context.Background(), draft.ID)

	if opens := blobs.count(); opens != 0 {
		t.Fatalf("%d blobs were opened for a draft that cannot fit once encoded", opens)
	}
	if status := h.draft(t, draft.ID).Status; status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
}

// An attachment load the budget admits must still be sent: the guard is a ceiling,
// not a blanket refusal.
func TestAttachmentBudgetAdmitsWhatFits(t *testing.T) {
	h := newHarness(t, &backend{})
	draft := h.queueDraft(t, "fits", "body")
	h.attach(t, draft.ID, "small.bin", bytes.Repeat([]byte{'x'}, 4096))

	h.worker.deliver(context.Background(), draft.ID)

	if status := h.draft(t, draft.ID).Status; status != "sent" {
		t.Fatalf("status = %q, want sent", status)
	}
}

// The pre-check is only as good as its arithmetic: overstate the encoded size and
// a draft that fits is refused, understate it and the guard passes a draft that
// still cannot be sent. The figure is compared against what the composer actually
// emits, so a change to its line width or encoding fails here rather than drifting
// silently. The sizes bracket the CRLF boundary the composer inserts at column 76
// and the base64 padding boundary at every third byte.
func TestBase64SizeMatchesTheComposer(t *testing.T) {
	for _, size := range []int64{0, 1, 2, 3, 56, 57, 58, 76, 100, 1000, 65536, 300 << 10} {
		withAttachment := composeSize(t, bytes.Repeat([]byte{'A'}, int(size)))
		// The difference between a payload with these bytes and one with an empty
		// attachment is the encoded length alone, headers and boundary excluded.
		emitted := withAttachment - composeSize(t, nil)
		if got := base64Size(size); got != emitted {
			t.Errorf("base64Size(%d) = %d, the composer emitted %d", size, got, emitted)
		}
	}
}

func composeSize(t *testing.T, content []byte) int64 {
	t.Helper()
	payload, err := mailbuilder.Compose(mailbuilder.Outgoing{
		MessageID: "<probe@example.com>",
		From:      mail.Address{Address: "sender@example.com"},
		To:        []mail.Address{{Address: "recipient@example.com"}},
		Subject:   "s", BodyText: "b",
		Attachments: []mailbuilder.OutgoingAttachment{{Filename: "f.bin", ContentType: "application/octet-stream", Data: bytes.NewReader(content)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(payload))
}

// accounts.provider is CHECK-constrained to six values and every one of them has a
// preset, so this is unreachable today. It is one edit away from being reachable:
// the CHECK and the preset table are separate files, and adding a provider to one
// without the other lands here. Skipping the APPEND silently would lose the user's
// remote record of everything they send from that account, so the branch is taken
// conservatively and the mismatch is logged.
func TestUnknownProviderStillFilesSentAndLogs(t *testing.T) {
	h := newHarness(t, &backend{})
	spy := h.withRemote(t, &remoteSpy{})
	logs := captureLogs(t)
	draft := h.queueDraft(t, "no-preset", "body")
	// complete is called directly: the provider column cannot hold this value, so
	// the account is doctored in memory instead.
	account := h.account
	account.Provider = "fastmail"

	h.worker.complete(context.Background(), account, draft, []byte("payload"), threadRefs{})

	if _, appended := spy.counts(); appended != 1 {
		t.Fatalf("AppendSent calls = %d, want 1: a provider with no preset must not lose its Sent copy", appended)
	}
	if status := h.draft(t, draft.ID).Status; status != "sent" {
		t.Fatalf("status = %q, want sent", status)
	}
	if !strings.Contains(logs.String(), "fastmail") {
		t.Fatalf("the preset mismatch was not logged:\n%s", logs.String())
	}
}

// A provider whose preset does file sent mail is still skipped, so the fallback
// above is a response to the lookup failing rather than an unconditional append.
func TestPresetThatSavesSentIsStillSkipped(t *testing.T) {
	h := newHarness(t, &backend{})
	spy := h.withRemote(t, &remoteSpy{})
	draft := h.queueDraft(t, "gmail-preset", "body")
	account := h.account
	account.Provider = "gmail"

	h.worker.complete(context.Background(), account, draft, []byte("payload"), threadRefs{})

	if _, appended := spy.counts(); appended != 0 {
		t.Fatalf("AppendSent calls = %d, want 0", appended)
	}
}

// References grows by one identifier per reply and nothing trimmed it, so a long
// enough thread produced a field past the 998 octets RFC 5322 allows on a line.
// Clients that truncate an overlong header lose the chain and the thread breaks
// apart, so the middle is dropped: the root is kept for clients that group on it
// and the most recent ancestors for the parent relationship (RFC 5537 3.4.4).
func TestReferencesChainIsTrimmedToTheHeaderBudget(t *testing.T) {
	h := newHarness(t, &backend{})
	// 30 ancestors at the length a real provider issues. Shorter synthetic ids fit
	// the budget and would not exercise the trim at all.
	chain := make([]string, 30)
	for i := range chain {
		chain[i] = fmt.Sprintf("<CAB%036d.%d@mail.example.com>", i, i)
	}
	inherited, err := json.Marshal(chain)
	if err != nil {
		t.Fatal(err)
	}
	parent := "<CABparent.30@mail.example.com>"
	source := h.sourceMessage(t, parent, string(inherited))
	draft := h.queueDraft(t, "Re: long thread", "replying")
	if err := h.exec(t, `UPDATE drafts SET source_message_id = ? WHERE id = ?`, source, draft.ID); err != nil {
		t.Fatal(err)
	}

	h.worker.deliver(context.Background(), draft.ID)

	if status := h.draft(t, draft.ID).Status; status != "sent" {
		t.Fatalf("status = %q, want sent", status)
	}
	transmitted := h.backend.messages()
	if len(transmitted) != 1 {
		t.Fatalf("%d messages transmitted", len(transmitted))
	}
	// Read through a parser so the assertion holds whether or not the field is
	// folded: net/mail unfolds it, and 998 is a limit on the unfolded field.
	parsed, err := mail.ReadMessage(strings.NewReader(transmitted[0]))
	if err != nil {
		t.Fatalf("the transmitted message does not parse: %v", err)
	}
	references := parsed.Header.Get("References")
	if unfolded := len("References: ") + len(references); unfolded > 998 {
		t.Fatalf("References is %d octets unfolded, over the 998 RFC 5322 allows:\n%s", unfolded, references)
	}
	ids := strings.Fields(references)
	if len(ids) < 3 {
		t.Fatalf("References was trimmed past what threading needs: %v", ids)
	}
	// The root anchors the thread and the parent places the reply under it; neither
	// may be dropped.
	if ids[0] != chain[0] {
		t.Fatalf("References[0] = %q, want the thread root %q", ids[0], chain[0])
	}
	if ids[len(ids)-1] != parent {
		t.Fatalf("References ends with %q, want the parent %q", ids[len(ids)-1], parent)
	}
	// What is kept beyond the root is a contiguous run of the most recent
	// ancestors, in order: a message must never precede one of its parents.
	full := append(append([]string{}, chain...), parent)
	tail := full[len(full)-(len(ids)-1):]
	for i, id := range tail {
		if ids[i+1] != id {
			t.Fatalf("References[%d] = %q, want %q; the kept tail is not the most recent ancestry: %v", i+1, ids[i+1], id, ids)
		}
	}
	// The dropped identifiers are gone from the wire, not merely reordered.
	dropped := full[1 : len(full)-(len(ids)-1)]
	if len(dropped) == 0 {
		t.Fatal("nothing was trimmed, so this test does not exercise the budget")
	}
	for _, id := range dropped {
		if strings.Contains(references, id) {
			t.Fatalf("%q was expected to be trimmed but is still on the wire", id)
		}
	}
	// The local Sent row has to carry the same chain as the headers, or our own list
	// view groups the thread differently from every other client.
	page, err := h.repo.ListMessages(context.Background(), ports.MessageFilter{AccountID: &h.account.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var stored []string
	for _, item := range page.Items {
		if item.Direction != "outgoing" {
			continue
		}
		message, _, err := h.repo.GetMessage(context.Background(), item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(message.ReferencesJSON), &stored); err != nil {
			t.Fatalf("references_json = %s: %v", message.ReferencesJSON, err)
		}
	}
	if strings.Join(stored, " ") != strings.Join(ids, " ") {
		t.Fatalf("the Sent row chain differs from the transmitted one:\nrow:  %v\nwire: %v", stored, ids)
	}
}

// A chain that fits is passed through untouched: trimming a short thread would
// lose ancestry for nothing.
func TestShortReferencesChainIsNotTrimmed(t *testing.T) {
	chain := []string{"<a@example.com>", "<b@example.com>", "<c@example.com>"}
	if got := trimReferences(chain); strings.Join(got, " ") != strings.Join(chain, " ") {
		t.Fatalf("trimReferences(%v) = %v", chain, got)
	}
}

// RFC 5537 3.4.4 forbids trimming below the first identifier and the last two even
// when they do not fit, because a chain shorter than that no longer threads at all.
func TestReferencesTrimKeepsTheProtectedFloor(t *testing.T) {
	huge := "<" + strings.Repeat("x", 900) + "@example.com>"
	got := trimReferences([]string{"<root@example.com>", huge, huge, huge, huge})
	if len(got) != protectedReferences+1 {
		t.Fatalf("kept %d identifiers, want %d", len(got), protectedReferences+1)
	}
	if got[0] != "<root@example.com>" {
		t.Fatalf("the root was dropped: %v", got[0])
	}
}

// attachWithSize links an attachment whose size_bytes is the declared length while
// the blob itself stays small. That is what lets the pre-check be observed on its
// own: the guard reads the column, so no test has to write hundreds of megabytes
// to disk to reach it.
func (h *harness) attachWithSize(t *testing.T, draftID int64, filename string, size int64) {
	t.Helper()
	ctx := context.Background()
	blob, err := h.blobs.Put(ctx, strings.NewReader("placeholder"), "durable")
	if err != nil {
		t.Fatal(err)
	}
	attachment := domain.DraftAttachment{
		DraftID: draftID, BlobID: blob.ID, Filename: filename,
		ContentType: "application/octet-stream", SizeBytes: size,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := h.repo.AddDraftAttachment(ctx, &attachment); err != nil {
		t.Fatal(err)
	}
}

// captureLogs redirects the default logger for the duration of one test. Safe
// because this package's tests do not run in parallel; the buffer is guarded
// anyway, since a worker started by another test writes from its own goroutine.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buffer := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buffer
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.String()
}

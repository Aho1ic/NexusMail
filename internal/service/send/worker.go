package send

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"strings"
	"sync"
	"time"

	"nexusmail/internal/domain"
	mailbuilder "nexusmail/internal/mail"
	"nexusmail/internal/ports"
	"nexusmail/internal/provider"
	smtpprovider "nexusmail/internal/provider/smtp"
	accountservice "nexusmail/internal/service/account"
)

type TokenProvider interface {
	AccessToken(context.Context, domain.Account, string) (string, error)
}

// RemoteDraftSyncer is the provider side of a send: the remote draft copy has to
// be removed once delivery succeeded, and for providers that do not file sent
// mail themselves the payload has to be appended to Sent.
type RemoteDraftSyncer interface {
	DeleteRemoteDraft(context.Context, int64) error
	AppendSent(context.Context, int64, []byte) error
}

// Store is the slice of persistence the send path uses: the outbox state machine,
// the draft and account being sent, the attachment blobs, and the Sent copy.
type Store interface {
	ports.OutboxRepo
	GetDraft(context.Context, int64) (domain.Draft, []domain.DraftAttachment, error)
	GetAccount(context.Context, int64) (domain.Account, error)
	GetBlob(context.Context, int64) (domain.BlobObject, error)
	GetMessage(context.Context, int64) (domain.Message, []domain.Attachment, error)
	ListMessages(context.Context, ports.MessageFilter) (ports.MessagePage, error)
	CreateSentMessage(context.Context, *domain.Message, int64) error
}

type Worker struct {
	repo        Store
	blobs       ports.BlobStore
	accounts    *accountservice.Service
	tokens      TokenProvider
	smtp        *smtpprovider.Client
	events      ports.Publisher
	maxBytes    int64
	queue       chan int64
	queuedMu    sync.Mutex
	queued      map[int64]struct{}
	remoteDraft RemoteDraftSyncer
}

func New(repo Store, blobs ports.BlobStore, accounts *accountservice.Service, tokens TokenProvider, smtp *smtpprovider.Client, events ports.Publisher, maxBytes int64, remoteDraft RemoteDraftSyncer) *Worker {
	return &Worker{repo: repo, blobs: blobs, accounts: accounts, tokens: tokens, smtp: smtp, events: events, maxBytes: maxBytes, queue: make(chan int64, 128), queued: make(map[int64]struct{}), remoteDraft: remoteDraft}
}

func (w *Worker) Start(ctx context.Context) {
	// A draft left in "sending" by a crash is claimed by nothing: the queue only
	// reads "queued" and "retry_wait". This sweep is the sole path back, it runs
	// once per process, and a failure here strands those drafts until the next
	// restart — so it is the one call in this file that must not fail quietly.
	if err := w.repo.RecoverSendingDrafts(ctx); err != nil {
		slog.Error("recover drafts left in sending", "error", err)
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-w.queue:
			w.queuedMu.Lock()
			delete(w.queued, id)
			w.queuedMu.Unlock()
			w.deliver(ctx, id)
		case <-ticker.C:
			ids, err := w.repo.ListDueDraftIDs(ctx, time.Now().UnixMilli())
			if err == nil {
				for _, id := range ids {
					w.enqueue(id)
				}
			}
		}
	}
}

func (w *Worker) Queue(ctx context.Context, id int64) error {
	draft, _, err := w.repo.GetDraft(ctx, id)
	if err != nil {
		return err
	}
	if draft.Status != "draft" && draft.Status != "failed" && draft.Status != "unknown" {
		return ports.Conflictf("draft cannot be queued in its current state")
	}
	if err := w.repo.SetDraftDelivery(ctx, id, "queued", draft.AttemptCount, nil, nil, nil, nil); err != nil {
		return err
	}
	w.events.Publish(ports.Event{Type: "OUTBOX_UPDATED", Data: map[string]any{"draft_id": id, "status": "queued"}})
	w.enqueue(id)
	return nil
}

func (w *Worker) enqueue(id int64) {
	w.queuedMu.Lock()
	if _, exists := w.queued[id]; exists {
		w.queuedMu.Unlock()
		return
	}
	w.queued[id] = struct{}{}
	w.queuedMu.Unlock()
	select {
	case w.queue <- id:
	default:
		w.queuedMu.Lock()
		delete(w.queued, id)
		w.queuedMu.Unlock()
	}
}

// postDeliveryWrite bounds the state write that records a delivery outcome. It
// runs on a context detached from the caller's, because ctx is cancelled at
// shutdown while an in-flight SMTP conversation keeps running on socket
// deadlines: the message is accepted by the provider and every write that follows
// then fails before database/sql even reaches for a connection, stranding the
// draft in 'sending' for RecoverSendingDrafts to downgrade to 'unknown' — a
// delivered message presented to the user as possibly undelivered, which invites
// a manual resend. Bounded rather than unlimited so a wedged write cannot hold up
// process exit indefinitely, and generous enough to cover WAL contention plus the
// remote Sent APPEND of a payload close to maxBytes.
const postDeliveryWrite = 30 * time.Second

// outcome derives the context the post-SMTP writes use. Everything before the
// send keeps ctx: cancelling those is correct, since nothing has left the process
// yet.
func outcome(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), postDeliveryWrite)
}

func (w *Worker) deliver(ctx context.Context, id int64) {
	draft, attachments, err := w.repo.ClaimSendableDraft(ctx, id)
	if err != nil {
		return
	}
	if err := attachmentBudget(attachments, w.maxBytes); err != nil {
		w.fail(ctx, draft, err, false, 0)
		return
	}
	account, err := w.repo.GetAccount(ctx, draft.AccountID)
	if err != nil {
		w.fail(ctx, draft, err, false, 0)
		return
	}
	credential, err := w.accounts.Credential(account)
	if err != nil {
		w.fail(ctx, draft, err, false, 0)
		return
	}
	smtpCredential := smtpprovider.Credential{Password: credential.Password}
	if account.AuthType == "oauth2" {
		smtpCredential.AccessToken, err = w.tokens.AccessToken(ctx, account, credential.RefreshToken)
		if err != nil {
			w.fail(ctx, draft, err, false, 0)
			return
		}
	}
	// Resolved once here and handed to both compose and complete: the wire headers
	// and the local Sent row have to describe the same thread, and one read of the
	// source message serves both.
	thread := w.ancestry(ctx, draft)
	message, recipients, err := w.compose(ctx, account, draft, attachments, thread)
	if err != nil {
		w.fail(ctx, draft, err, false, 0)
		return
	}
	if int64(len(message)) > w.maxBytes {
		w.fail(ctx, draft, fmt.Errorf("composed message exceeds %d bytes", w.maxBytes), false, 0)
		return
	}
	err = w.smtp.Send(ctx, account, smtpCredential, account.Email, recipients, int64(len(message)), bytes.NewReader(message))
	if err != nil {
		var deliveryErr *smtpprovider.DeliveryError
		if errors.As(err, &deliveryErr) {
			if deliveryErr.Unknown {
				text := deliveryErr.Error()
				// unknown means DATA was sent and the connection dropped before
				// we got an ack; delivery may have happened. Treat the manual
				// retry as a clean restart: reset attempt_count so a fresh
				// automatic retry ladder can run instead of falling into
				// failed on the first temporary blip after recovery.
				outcomeCtx, cancel := outcome(ctx)
				if writeErr := w.repo.SetDraftDelivery(outcomeCtx, id, "unknown", 0, nil, &deliveryErr.Code, &text, nil); writeErr != nil {
					slog.Error("set draft to unknown", "draft_id", id, "error", writeErr)
				}
				cancel()
				w.publish(id, "unknown")
				return
			}
			w.fail(ctx, draft, err, deliveryErr.Temporary, deliveryErr.Code)
			return
		}
		w.fail(ctx, draft, err, true, 0)
		return
	}
	w.complete(ctx, account, draft, message, thread)
}

// ancestry resolves the reply headers for a draft written in reply to a stored
// message. Everything is best-effort: a source row the user has since deleted, or
// one whose Message-ID the provider never supplied, must not fail the send — a
// reply that threads as a new conversation is a cosmetic loss, a reply that never
// leaves is not.
func (w *Worker) ancestry(ctx context.Context, draft domain.Draft) threadRefs {
	if draft.SourceMessageID == nil {
		return threadRefs{}
	}
	source, _, err := w.repo.GetMessage(ctx, *draft.SourceMessageID)
	if err != nil || source.RFCMessageID == nil || *source.RFCMessageID == "" {
		return threadRefs{}
	}
	parent := *source.RFCMessageID
	// RFC 5322 3.6.4: References is the parent's own References chain with the
	// parent's Message-ID appended, which is what lets a client place the reply
	// under the whole thread rather than only under its immediate parent.
	var inherited []string
	_ = json.Unmarshal([]byte(source.ReferencesJSON), &inherited)
	return threadRefs{inReplyTo: parent, references: trimReferences(append(inherited, parent))}
}

// referencesBudget is the octets a References field may occupy unfolded,
// including its field name but not the final CRLF. RFC 5322 2.1.1 caps a line at
// 998 octets and RFC 5537 3.4.4 makes that same figure the trigger for trimming
// the chain; len("References: ") is subtracted here so the budget applies to the
// value.
const referencesBudget = 998 - len("References: ")

// protectedReferences is the tail RFC 5537 3.4.4 forbids trimming. The first
// identifier plus the last two are what threading actually needs: the first roots
// the conversation for clients that group on it, the last two place the reply
// under its immediate parent. It is also a floor, not a target — the chain is
// trimmed to whatever fits the budget above that.
const protectedReferences = 2

// trimReferences bounds a chain that otherwise grows by one identifier per reply
// forever. An unfolded References field past 998 octets is beyond what RFC 5322
// permits on a line, and clients that truncate it lose threading outright, so the
// middle is dropped: the first identifier is kept and as many of the most recent
// ancestors as the budget allows. Dropping from the middle rather than the tail is
// what keeps the ordering invariant RFC 5537 requires, that a message never
// precedes one of its parents.
func trimReferences(chain []string) []string {
	total := 0
	for _, id := range chain {
		total += len(id) + 1
	}
	if total-1 <= referencesBudget || len(chain) <= protectedReferences+1 {
		return chain
	}
	// The first identifier is never dropped, so its cost comes off the budget
	// before the tail is measured.
	remaining := referencesBudget - len(chain[0])
	kept := 0
	for i := len(chain) - 1; i > 0; i-- {
		cost := len(chain[i]) + 1
		if cost > remaining && kept >= protectedReferences {
			break
		}
		remaining -= cost
		kept++
	}
	trimmed := make([]string, 0, kept+1)
	trimmed = append(trimmed, chain[0])
	return append(trimmed, chain[len(chain)-kept:]...)
}

// threadRefs carries a reply's ancestry from the source message to both the
// composed headers and the local Sent row. Empty means "not a reply".
type threadRefs struct {
	inReplyTo  string
	references []string
}

// referencesJSON encodes the chain for storage. A reply with no ancestry stores
// "[]" rather than the "null" a nil slice would marshal to, which is what every
// other writer of this column stores.
func (t threadRefs) referencesJSON() string {
	if len(t.references) == 0 {
		return "[]"
	}
	return encodeStrings(t.references)
}

// attachmentBudget rejects a draft whose attachments cannot fit the outbound
// limit, before any of them is read. compose drains every blob into one in-memory
// payload, so the len(message) guard downstream only fires after N attachments
// have already been buffered and base64-expanded — the ceiling on a single upload
// is per-request and says nothing about their sum. SizeBytes is the stored raw
// length, so the base64 expansion compose applies is charged here too, otherwise a
// draft that passes this check still fails after assembly.
func attachmentBudget(attachments []domain.DraftAttachment, maxBytes int64) error {
	total := int64(0)
	for _, attachment := range attachments {
		// Checked before the multiplication so a corrupt or absurd size_bytes
		// cannot overflow the running total.
		if attachment.SizeBytes > maxBytes {
			return fmt.Errorf("attachments exceed the %d byte message limit", maxBytes)
		}
		total += base64Size(attachment.SizeBytes)
		if total > maxBytes {
			return fmt.Errorf("attachments exceed the %d byte message limit", maxBytes)
		}
	}
	return nil
}

// base64Size is the encoded length compose produces for a raw attachment: the 4/3
// expansion with padding, plus the CRLF the composer inserts every 76 columns.
func base64Size(raw int64) int64 {
	encoded := (raw + 2) / 3 * 4
	if encoded == 0 {
		return 0
	}
	return encoded + 2*((encoded-1)/76)
}

// compose renders the draft into an RFC 5322 payload and the envelope recipient
// list. Every attachment file descriptor it opens is also closed before it
// returns, so the caller inherits nothing to clean up.
func (w *Worker) compose(ctx context.Context, account domain.Account, draft domain.Draft, attachments []domain.DraftAttachment, thread threadRefs) ([]byte, []string, error) {
	to, err := parseAddresses(draft.ToJSON)
	if err != nil {
		return nil, nil, err
	}
	cc, err := parseAddresses(draft.CCJSON)
	if err != nil {
		return nil, nil, err
	}
	bcc, err := parseAddresses(draft.BCCJSON)
	if err != nil {
		return nil, nil, err
	}
	outgoingAttachments := make([]mailbuilder.OutgoingAttachment, 0, len(attachments))
	closers := make([]io.Closer, 0, len(attachments))
	defer func() { w.closeAll(closers) }()
	for _, attachment := range attachments {
		blob, err := w.repo.GetBlob(ctx, attachment.BlobID)
		if err != nil {
			return nil, nil, err
		}
		reader, err := w.blobs.Open(ctx, blob)
		if err != nil {
			return nil, nil, err
		}
		closers = append(closers, reader)
		outgoingAttachments = append(outgoingAttachments, mailbuilder.OutgoingAttachment{Filename: attachment.Filename, ContentType: attachment.ContentType, Data: reader})
	}
	from := mail.Address{Name: account.DisplayName, Address: account.Email}
	// mailbuilder.Compose fully drains each attachment Data reader into the
	// in-memory payload, so the deferred close above runs while the FDs are
	// already spent. Keeping the lifetime inside this function rather than handing
	// closers back to the caller is what keeps an attachment's FD scoped to its
	// actual use instead of to the whole send, SMTP round-trip included.
	payload, err := mailbuilder.Compose(mailbuilder.Outgoing{MessageID: draft.RFCMessageID, From: from, To: to, CC: cc, BCC: bcc, Subject: draft.Subject, BodyText: draft.BodyText, InReplyTo: thread.inReplyTo, References: thread.references, Attachments: outgoingAttachments})
	recipients := addressValues(append(append(append([]mail.Address{}, to...), cc...), bcc...))
	return payload, recipients, err
}

// closeAll closes a list of closers and silently swallows the per-FD error
// because the only failure mode here is "blob already gone", which is benign
// and would only mask the original error if it were surfaced.
func (w *Worker) closeAll(closers []io.Closer) {
	for _, closer := range closers {
		_ = closer.Close()
	}
}

func (w *Worker) complete(ctx context.Context, account domain.Account, draft domain.Draft, payload []byte, thread threadRefs) {
	now := time.Now().UnixMilli()
	digest := sha256.Sum256([]byte(draft.RFCMessageID))
	// The Sent row carries the same ancestry as the transmitted headers. Storing an
	// empty chain here left the local copy outside the thread it was sent into, so
	// the reply and the message it answers never grouped in our own list view even
	// though every other client threaded them correctly.
	var inReplyTo *string
	if thread.inReplyTo != "" {
		inReplyTo = &thread.inReplyTo
	}
	message := domain.Message{
		AccountID: account.ID, Direction: "outgoing", DedupeKey: digest[:], RFCMessageID: &draft.RFCMessageID,
		InReplyTo: inReplyTo,
		Subject:   draft.Subject, Sender: account.Email, Recipients: recipientsText(draft), FromJSON: encodeStrings([]string{account.Email}),
		ToJSON: draft.ToJSON, CCJSON: draft.CCJSON, BCCJSON: draft.BCCJSON, ReplyToJSON: "[]", ReferencesJSON: thread.referencesJSON(),
		Snippet: snippet(draft.BodyText, 240), BodyText: draft.BodyText, BodyState: "ready", SentAt: &now, ReceivedAt: now,
		IsRead: true, CreatedAt: now, UpdatedAt: now,
	}
	// Nothing past this point may schedule a retry: the provider has the message,
	// and a second transmission is not something the user can take back. Every
	// failure below is local or remote bookkeeping — it is logged and surfaced on
	// the draft as drift, never turned into a new send.
	var drift []string
	outcomeCtx, cancel := outcome(ctx)
	defer cancel()
	if err := w.repo.CreateSentMessage(outcomeCtx, &message, draft.ID); err != nil {
		// Retrying this is what sent the message twice: the draft went to
		// retry_wait, became due, and was claimed and transmitted again. The Sent
		// copy is a local record, so losing it costs the user a row in their own
		// list view, not a delivery.
		slog.Error("store sent copy", "draft_id", draft.ID, "error", err)
		drift = append(drift, "the local Sent copy could not be stored: "+err.Error())
	}
	// Written before the two remote calls rather than after them: a crash in
	// between would otherwise leave the draft in 'sending' for the next
	// RecoverSendingDrafts to report as 'unknown'.
	w.markSent(outcomeCtx, draft, now, drift)
	if w.remoteDraft != nil {
		if err := w.remoteDraft.DeleteRemoteDraft(outcomeCtx, draft.ID); err != nil {
			// The remote Drafts folder will keep a copy until the user deletes it
			// from another client; that is preferable to re-queuing.
			slog.Error("delete remote draft", "draft_id", draft.ID, "error", err)
		}
		preset, err := provider.Get(account.Provider)
		if err != nil {
			// A provider the accounts CHECK constraint admits but the preset table
			// does not know: the two are edited independently, so one can be added
			// without the other. Appending is the conservative branch — a duplicate
			// in Sent is visible and deletable, a missing sent record is neither.
			slog.Error("provider preset for the sent copy", "draft_id", draft.ID, "provider", account.Provider, "error", err)
		}
		if err != nil || !preset.ServerSavesSent {
			if appendErr := w.remoteDraft.AppendSent(outcomeCtx, account.ID, payload); appendErr != nil {
				slog.Error("append to remote sent", "draft_id", draft.ID, "error", appendErr)
				drift = append(drift, "remote Sent coordination failed: "+appendErr.Error())
				w.markSent(outcomeCtx, draft, now, drift)
			}
		}
	}
	w.publish(draft.ID, "sent")
}

// markSent records the terminal state of a delivery that the provider accepted.
// next_attempt_at is always nil: the state machine picks up a draft that carries
// one, so writing it here is what would send the message a second time. Any drift
// collected on the way is reported through last_error, because the user has to be
// able to tell "delivered" from "delivered, and something local is out of step".
func (w *Worker) markSent(ctx context.Context, draft domain.Draft, now int64, drift []string) {
	var text *string
	if len(drift) > 0 {
		joined := "message sent, but " + strings.Join(drift, "; ")
		text = &joined
	}
	if err := w.repo.SetDraftDelivery(ctx, draft.ID, "sent", draft.AttemptCount, nil, nil, text, &now); err != nil {
		slog.Error("set draft to sent", "draft_id", draft.ID, "error", err)
	}
}

func (w *Worker) fail(ctx context.Context, draft domain.Draft, err error, temporary bool, code int) {
	status := "failed"
	var next *int64
	if temporary && draft.AttemptCount < 5 {
		status = "retry_wait"
		// Four rungs for a five-attempt cap: ClaimSendableDraft has already
		// incremented AttemptCount when this runs, so attempt N reads rung N-1 and
		// the guard above stops at attempt 4. Attempt 5 is terminal, so a fifth
		// rung could never be indexed — it was dead weight that read as a 30m
		// retry the state machine never performs.
		delay := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}[draft.AttemptCount-1]
		value := time.Now().Add(delay).UnixMilli()
		next = &value
	}
	text := err.Error()
	var codePtr *int
	if code != 0 {
		codePtr = &code
	}
	// A failed write here strands the draft in 'sending' until the next
	// RecoverSendingDrafts startup, which would mark it 'unknown' and lose
	// the retry schedule we just computed. Logging is the minimum; the DB
	// error is rare (WAL contention) and the user-visible consequence is a
	// stuck retry. The write runs detached from ctx for the same reason
	// complete's does: a shutdown that lands mid-conversation must not be what
	// turns a classified retry into an 'unknown' the user has to resolve by hand.
	outcomeCtx, cancel := outcome(ctx)
	defer cancel()
	if err := w.repo.SetDraftDelivery(outcomeCtx, draft.ID, status, draft.AttemptCount, next, codePtr, &text, nil); err != nil {
		slog.Error("set draft delivery", "draft_id", draft.ID, "status", status, "error", err)
	}
	w.publish(draft.ID, status)
}

func (w *Worker) publish(id int64, status string) {
	w.events.Publish(ports.Event{Type: "OUTBOX_UPDATED", Data: map[string]any{"draft_id": id, "status": status}})
}

func parseAddresses(raw string) ([]mail.Address, error) {
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, err
	}
	result := make([]mail.Address, 0, len(values))
	for _, value := range values {
		address, err := mail.ParseAddress(value)
		if err != nil {
			return nil, err
		}
		result = append(result, *address)
	}
	return result, nil
}
func addressValues(values []mail.Address) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.Address)
	}
	return result
}
func encodeStrings(values []string) string { b, _ := json.Marshal(values); return string(b) }
func recipientsText(draft domain.Draft) string {
	return draft.ToJSON + " " + draft.CCJSON + " " + draft.BCCJSON
}
func snippet(value string, max int) string {
	runes := []rune(value)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return value
}

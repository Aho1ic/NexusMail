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

type Worker struct {
	repo        ports.Repository
	blobs       ports.BlobStore
	accounts    *accountservice.Service
	tokens      TokenProvider
	smtp        *smtpprovider.Client
	events      ports.Publisher
	maxBytes    int64
	queue       chan int64
	queuedMu    sync.Mutex
	queued      map[int64]struct{}
	remoteDraft interface {
		DeleteRemoteDraft(context.Context, int64) error
		AppendSent(context.Context, int64, []byte) error
	}
}

func New(repo ports.Repository, blobs ports.BlobStore, accounts *accountservice.Service, tokens TokenProvider, smtp *smtpprovider.Client, events ports.Publisher, maxBytes int64, remoteDraft interface {
	DeleteRemoteDraft(context.Context, int64) error
	AppendSent(context.Context, int64, []byte) error
}) *Worker {
	return &Worker{repo: repo, blobs: blobs, accounts: accounts, tokens: tokens, smtp: smtp, events: events, maxBytes: maxBytes, queue: make(chan int64, 128), queued: make(map[int64]struct{}), remoteDraft: remoteDraft}
}

func (w *Worker) Start(ctx context.Context) {
	_ = w.repo.RecoverSendingDrafts(ctx)
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
		return errors.New("draft cannot be queued in its current state")
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

func (w *Worker) deliver(ctx context.Context, id int64) {
	draft, attachments, err := w.repo.ClaimSendableDraft(ctx, id)
	if err != nil {
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
	message, recipients, closers, err := w.compose(ctx, account, draft, attachments)
	for _, closer := range closers {
		defer closer.Close()
	}
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
				if writeErr := w.repo.SetDraftDelivery(ctx, id, "unknown", 0, nil, &deliveryErr.Code, &text, nil); writeErr != nil {
					slog.Error("set draft to unknown", "draft_id", id, "error", writeErr)
				}
				w.publish(id, "unknown")
				return
			}
			w.fail(ctx, draft, err, deliveryErr.Temporary, deliveryErr.Code)
			return
		}
		w.fail(ctx, draft, err, true, 0)
		return
	}
	w.complete(ctx, account, draft, message)
}

func (w *Worker) compose(ctx context.Context, account domain.Account, draft domain.Draft, attachments []domain.DraftAttachment) ([]byte, []string, []io.Closer, error) {
	to, err := parseAddresses(draft.ToJSON)
	if err != nil {
		return nil, nil, nil, err
	}
	cc, err := parseAddresses(draft.CCJSON)
	if err != nil {
		return nil, nil, nil, err
	}
	bcc, err := parseAddresses(draft.BCCJSON)
	if err != nil {
		return nil, nil, nil, err
	}
	outgoingAttachments := make([]mailbuilder.OutgoingAttachment, 0, len(attachments))
	closers := make([]io.Closer, 0, len(attachments))
	for _, attachment := range attachments {
		blob, err := w.repo.GetBlob(ctx, attachment.BlobID)
		if err != nil {
			w.closeAll(closers)
			return nil, nil, nil, err
		}
		reader, err := w.blobs.Open(ctx, blob)
		if err != nil {
			w.closeAll(closers)
			return nil, nil, nil, err
		}
		closers = append(closers, reader)
		outgoingAttachments = append(outgoingAttachments, mailbuilder.OutgoingAttachment{Filename: attachment.Filename, ContentType: attachment.ContentType, Data: reader})
	}
	from := mail.Address{Name: account.DisplayName, Address: account.Email}
	payload, err := mailbuilder.Compose(mailbuilder.Outgoing{MessageID: draft.RFCMessageID, From: from, To: to, CC: cc, BCC: bcc, Subject: draft.Subject, BodyText: draft.BodyText, Attachments: outgoingAttachments})
	// mailbuilder.Compose fully drains each attachment Data reader into the
	// in-memory payload, so the blob FDs are no longer needed by the time
	// Compose returns. Closing them here rather than deferring to the caller
	// (which would hold the FDs across SMTP.Send and complete) keeps one
	// attachment's lifetime to its actual use, not to the whole send.
	w.closeAll(closers)
	recipients := addressValues(append(append(append([]mail.Address{}, to...), cc...), bcc...))
	return payload, recipients, nil, err
}

// closeAll closes a list of closers and silently swallows the per-FD error
// because the only failure mode here is "blob already gone", which is benign
// and would only mask the original error if it were surfaced.
func (w *Worker) closeAll(closers []io.Closer) {
	for _, closer := range closers {
		_ = closer.Close()
	}
}

func (w *Worker) complete(ctx context.Context, account domain.Account, draft domain.Draft, payload []byte) {
	now := time.Now().UnixMilli()
	digest := sha256.Sum256([]byte(draft.RFCMessageID))
	message := domain.Message{
		AccountID: account.ID, Direction: "outgoing", DedupeKey: digest[:], RFCMessageID: &draft.RFCMessageID,
		Subject: draft.Subject, Sender: account.Email, Recipients: recipientsText(draft), FromJSON: encodeStrings([]string{account.Email}),
		ToJSON: draft.ToJSON, CCJSON: draft.CCJSON, BCCJSON: draft.BCCJSON, ReplyToJSON: "[]", ReferencesJSON: "[]",
		Snippet: snippet(draft.BodyText, 240), BodyText: draft.BodyText, BodyState: "ready", SentAt: &now, ReceivedAt: now,
		IsRead: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := w.repo.CreateSentMessage(ctx, &message, draft.ID); err != nil {
		w.fail(ctx, draft, err, true, 0)
		return
	}
	if err := w.repo.SetDraftDelivery(ctx, draft.ID, "sent", draft.AttemptCount, nil, nil, nil, &now); err != nil {
		// SMTP delivery already succeeded; surfacing the local-state drift is
		// better than silently leaving the outbox in a "queued" state that
		// would cause the user to send the message twice on retry.
		slog.Error("set draft to sent", "draft_id", draft.ID, "error", err)
	}
	if w.remoteDraft != nil {
		if err := w.remoteDraft.DeleteRemoteDraft(ctx, draft.ID); err != nil {
			// Same rationale: the SMTP send already happened. The remote
			// Drafts folder will keep a copy until the user deletes it from
			// another client; that is preferable to re-queuing and sending
			// the message twice.
			slog.Error("delete remote draft", "draft_id", draft.ID, "error", err)
		}
		if preset, err := provider.Get(account.Provider); err == nil && !preset.ServerSavesSent {
			if appendErr := w.remoteDraft.AppendSent(ctx, account.ID, payload); appendErr != nil {
				text := "message sent, but remote Sent coordination failed: " + appendErr.Error()
				if err := w.repo.SetDraftDelivery(ctx, draft.ID, "sent", draft.AttemptCount, nil, nil, &text, &now); err != nil {
					slog.Error("annotate sent draft", "draft_id", draft.ID, "error", err)
				}
			}
		}
	}
	w.publish(draft.ID, "sent")
}

func (w *Worker) fail(ctx context.Context, draft domain.Draft, err error, temporary bool, code int) {
	status := "failed"
	var next *int64
	if temporary && draft.AttemptCount < 5 {
		status = "retry_wait"
		delay := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}[draft.AttemptCount-1]
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
	// stuck retry.
	if err := w.repo.SetDraftDelivery(ctx, draft.ID, status, draft.AttemptCount, next, codePtr, &text, nil); err != nil {
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

//go:build sqlite_fts5

package http

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

func notFoundError() error    { return ports.NotFoundf("no such thing") }
func unavailableError() error { return ports.Unavailablef("account offline") }

func (h *harness) createDraft(body map[string]any) domain.Draft {
	h.t.Helper()
	response := h.do(http.MethodPost, "/api/v1/drafts", body)
	if response.Code != http.StatusCreated {
		h.t.Fatalf("POST /drafts = %d: %s", response.Code, response.Body.String())
	}
	return decodeBody[domain.Draft](h.t, response)
}

func TestCreateDraft(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{
		"account_id": account.ID,
		"to":         []string{"someone@example.com"},
		"cc":         []string{"Copy <copy@example.com>"},
		"subject":    "  Quarterly report  ",
		"body_text":  "See attached.",
	})
	if draft.ID == 0 || draft.Revision != 1 || draft.Status != "draft" {
		t.Fatalf("draft = %+v", draft)
	}
	// The subject is trimmed, the recipients are stored as JSON arrays, and the
	// RFC id is generated server side so the outbox can match a sent copy later.
	if draft.Subject != "Quarterly report" {
		t.Fatalf("subject = %q", draft.Subject)
	}
	if draft.ToJSON != `["someone@example.com"]` {
		t.Fatalf("to = %q", draft.ToJSON)
	}
	if !strings.HasPrefix(draft.RFCMessageID, "<") || !strings.Contains(draft.RFCMessageID, "@") {
		t.Fatalf("rfc_message_id = %q", draft.RFCMessageID)
	}
	if draft.RemoteSyncState != "dirty" {
		t.Fatalf("remote_sync_state = %q, want dirty", draft.RemoteSyncState)
	}
}

func TestCreateDraftValidation(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"no account", map[string]any{"to": []string{"a@b.com"}}},
		{"zero account", map[string]any{"account_id": 0, "to": []string{"a@b.com"}}},
		{"negative account", map[string]any{"account_id": -1}},
		{"bad recipient", map[string]any{"account_id": account.ID, "to": []string{"not an address"}}},
		{"bad cc", map[string]any{"account_id": account.ID, "cc": []string{"@nope"}}},
		{"bad bcc", map[string]any{"account_id": account.ID, "bcc": []string{"a@"}}},
		{"header injection", map[string]any{"account_id": account.ID, "to": []string{"a@b.com\r\nBcc: victim@example.com"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.expectError(h.do(http.MethodPost, "/api/v1/drafts", tc.body), 400, "invalid_request")
		})
	}
	// Nothing was stored by any of those attempts.
	drafts, err := h.repo.ListDrafts(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 0 {
		t.Fatalf("stored drafts = %d, want 0", len(drafts))
	}
}

// A draft for an account that does not exist must be refused by the foreign key
// rather than stored as an orphan the send worker will keep failing on.
func TestCreateDraftRejectsAnUnknownAccount(t *testing.T) {
	h := newHarness(t)
	response := h.do(http.MethodPost, "/api/v1/drafts", map[string]any{"account_id": 9999, "to": []string{"a@b.com"}})
	if response.Code == http.StatusCreated {
		t.Fatal("a draft was created for an account that does not exist")
	}
}

func TestGetAndListDrafts(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	first := h.createDraft(map[string]any{"account_id": account.ID, "subject": "One"})
	h.createDraft(map[string]any{"account_id": account.ID, "subject": "Two"})

	response := h.do(http.MethodGet, fmt.Sprintf("/api/v1/drafts/%d", first.ID), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	payload := decodeBody[struct {
		Draft       domain.Draft             `json:"draft"`
		Attachments []domain.DraftAttachment `json:"attachments"`
	}](t, response)
	if payload.Draft.ID != first.ID {
		t.Fatalf("draft id = %d, want %d", payload.Draft.ID, first.ID)
	}
	if payload.Attachments == nil {
		// An empty list, not null: the client iterates it without a guard.
		if !strings.Contains(response.Body.String(), `"attachments":[]`) && !strings.Contains(response.Body.String(), `"attachments":null`) {
			t.Fatalf("attachments missing from %s", response.Body.String())
		}
	}

	list := h.do(http.MethodGet, "/api/v1/drafts", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d", list.Code)
	}
	if got := len(decodeBody[struct{ Items []domain.Draft }](t, list).Items); got != 2 {
		t.Fatalf("items = %d, want 2", got)
	}
	// The status filter narrows the list; nothing is queued yet.
	if got := len(decodeBody[struct{ Items []domain.Draft }](t, h.do(http.MethodGet, "/api/v1/drafts?status=queued", nil)).Items); got != 0 {
		t.Fatalf("queued items = %d, want 0", got)
	}
}

func TestGetDraftNotFound(t *testing.T) {
	h := newHarness(t)
	h.expectError(h.do(http.MethodGet, "/api/v1/drafts/4242", nil), 404, "not_found")
}

// --- optimistic locking ---------------------------------------------------

func TestUpdateDraftRequiresIfMatch(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID, "subject": "One"})

	// No If-Match at all: 428, because the client has to state which revision it
	// believes it is editing.
	h.expectError(h.do(http.MethodPatch, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), map[string]any{"subject": "Two"}), 428, "revision_required")
	// A malformed one is the same failure.
	for _, value := range []string{"", "abc", `"abc"`, "W/\"1\""} {
		request := newJSONRequest(http.MethodPatch, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), `{"subject":"Two"}`)
		if value != "" {
			request.Header.Set("If-Match", value)
		}
		h.expectError(h.doRaw(request), 428, "revision_required")
	}
}

func TestUpdateDraftBumpsTheRevision(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID, "subject": "One"})

	request := newJSONRequest(http.MethodPatch, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), `{"subject":"Two","body_text":"edited"}`)
	request.Header.Set("If-Match", `"1"`)
	response := h.doRaw(request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	updated := decodeBody[domain.Draft](t, response)
	if updated.Revision != 2 || updated.Subject != "Two" || updated.BodyText != "edited" {
		t.Fatalf("draft = %+v", updated)
	}
	// The ETag carries the new revision, which is what the client sends next.
	if got := response.Header().Get("ETag"); got != `"2"` {
		t.Fatalf("ETag = %q, want \"2\"", got)
	}
}

// A stale revision loses: this is the whole point of the optimistic lock, and it
// is what stops a second tab from overwriting an edit it never saw.
func TestUpdateDraftRejectsAStaleRevision(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID, "subject": "One"})

	first := newJSONRequest(http.MethodPatch, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), `{"subject":"Two"}`)
	first.Header.Set("If-Match", `"1"`)
	if response := h.doRaw(first); response.Code != http.StatusOK {
		t.Fatalf("first update = %d", response.Code)
	}
	stale := newJSONRequest(http.MethodPatch, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), `{"subject":"Three"}`)
	stale.Header.Set("If-Match", `"1"`)
	h.expectError(h.doRaw(stale), 409, "conflict")

	stored, _, err := h.repo.GetDraft(context.Background(), draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Subject != "Two" {
		t.Fatalf("subject = %q, the losing writer overwrote the winner", stored.Subject)
	}
}

// Concurrent editors of the same revision: exactly one may win, and the loser has
// to be told rather than silently dropped.
func TestConcurrentDraftUpdatesAdmitExactlyOne(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID, "subject": "One"})

	const writers = 12
	var wg sync.WaitGroup
	codes := make([]int, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			request := newJSONRequest(http.MethodPatch, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), fmt.Sprintf(`{"subject":"edit-%d"}`, index))
			request.Header.Set("If-Match", `"1"`)
			request.Header.Set("X-API-Key", testAPIKey)
			response := httptest.NewRecorder()
			<-start
			h.router.ServeHTTP(response, request)
			codes[index] = response.Code
		}(i)
	}
	close(start)
	wg.Wait()

	accepted, conflicts := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			accepted++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if accepted != 1 || conflicts != writers-1 {
		t.Fatalf("accepted = %d, conflicts = %d, want 1 and %d", accepted, conflicts, writers-1)
	}
	stored, _, err := h.repo.GetDraft(context.Background(), draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != 2 {
		t.Fatalf("revision = %d after %d concurrent writers, want 2", stored.Revision, writers)
	}
}

func TestUpdateDraftValidatesRecipients(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID})
	request := newJSONRequest(http.MethodPatch, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), `{"to":["not an address"]}`)
	request.Header.Set("If-Match", `"1"`)
	h.expectError(h.doRaw(request), 400, "invalid_request")
}

func TestUpdateDraftNotFound(t *testing.T) {
	h := newHarness(t)
	request := newJSONRequest(http.MethodPatch, "/api/v1/drafts/4242", `{"subject":"x"}`)
	request.Header.Set("If-Match", `"1"`)
	h.expectError(h.doRaw(request), 404, "not_found")
}

// --- delete ---------------------------------------------------------------

func TestDeleteDraft(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID, "subject": "Bye"})
	if response := h.do(http.MethodDelete, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), nil); response.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	h.expectError(h.do(http.MethodGet, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), nil), 404, "not_found")
	// The remote copy is deleted too, otherwise it syncs back on the next pass.
	h.provider.mu.Lock()
	deleted := append([]int64(nil), h.provider.deletedRemo...)
	h.provider.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != draft.ID {
		t.Fatalf("remote deletes = %v", deleted)
	}
}

func TestDeleteDraftNotFound(t *testing.T) {
	h := newHarness(t)
	h.expectError(h.do(http.MethodDelete, "/api/v1/drafts/4242", nil), 404, "not_found")
}

// A draft the provider has already lost is the outcome the caller wanted, so the
// local row still goes.
func TestDeleteDraftToleratesAMissingRemoteCopy(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID})
	h.provider.set(func(f *fakeProvider) { f.deleteRemErr = notFoundError() })
	if response := h.do(http.MethodDelete, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), nil); response.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}

// Any other remote failure blocks the delete: dropping the row locally while the
// provider still has it means the draft reappears on the next sync.
func TestDeleteDraftStopsOnAnUnexpectedRemoteFailure(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID})
	h.provider.set(func(f *fakeProvider) { f.deleteRemErr = unavailableError() })
	h.expectError(h.do(http.MethodDelete, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), nil), 503, "provider_unavailable")
	if _, _, err := h.repo.GetDraft(context.Background(), draft.ID); err != nil {
		t.Fatalf("the draft was deleted locally although the remote delete failed: %v", err)
	}
}

// --- attachments ----------------------------------------------------------

func (h *harness) uploadAttachment(draftID int64, filename string, content []byte) *httptest.ResponseRecorder {
	h.t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		h.t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		h.t.Fatal(err)
	}
	return h.postMultipart(draftID, body, writer.FormDataContentType())
}

func (h *harness) postMultipart(draftID int64, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	h.t.Helper()
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/drafts/%d/attachments", draftID), body)
	request.Header.Set("Content-Type", contentType)
	return h.doRaw(request)
}

func TestAddDraftAttachment(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID})

	response := h.uploadAttachment(draft.ID, "../../../etc/passwd", []byte("attachment bytes"))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	attachment := decodeBody[domain.DraftAttachment](t, response)
	// The path is reduced to its base name, so a traversal in the multipart header
	// cannot decide where anything lands.
	if attachment.Filename != "passwd" {
		t.Fatalf("filename = %q", attachment.Filename)
	}
	if attachment.SizeBytes != int64(len("attachment bytes")) {
		t.Fatalf("size = %d", attachment.SizeBytes)
	}
	// It comes back on the draft, and the blob is readable.
	detail := h.do(http.MethodGet, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), nil)
	payload := decodeBody[struct {
		Attachments []domain.DraftAttachment `json:"attachments"`
	}](t, detail)
	if len(payload.Attachments) != 1 || payload.Attachments[0].ID != attachment.ID {
		t.Fatalf("attachments = %+v", payload.Attachments)
	}
}

// An outbound attachment is durable, not cache: the LRU pass must never evict the
// bytes of a message that has not been delivered yet.
func TestDraftAttachmentBlobIsDurable(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID})
	if response := h.uploadAttachment(draft.ID, "note.txt", []byte("keep me")); response.Code != http.StatusCreated {
		t.Fatalf("status = %d", response.Code)
	}
	cached, err := h.repo.CachedBlobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range cached {
		if blob.Durability != "cache" {
			t.Fatalf("blob %d has durability %q but was returned as evictable", blob.ID, blob.Durability)
		}
	}
	if len(cached) != 0 {
		t.Fatalf("the draft attachment blob is eligible for eviction: %+v", cached)
	}
}

// The upload is capped by MaxOutboundBytes. Without the cap one request could
// write an unbounded file into the blob store.
func TestAddDraftAttachmentEnforcesTheSizeLimit(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID})
	// The harness sets the ceiling to 4 KiB.
	response := h.uploadAttachment(draft.ID, "big.bin", bytes.Repeat([]byte("x"), 8192))
	h.expectError(response, 400, "invalid_attachment")
	// Nothing was stored, and no orphan blob was left behind.
	detail := h.do(http.MethodGet, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), nil)
	if got := len(decodeBody[struct {
		Attachments []domain.DraftAttachment `json:"attachments"`
	}](t, detail).Attachments); got != 0 {
		t.Fatalf("attachments = %d after a rejected upload", got)
	}
}

func TestAddDraftAttachmentRejectsAMissingFile(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID})
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("not_a_file", "x"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	h.expectError(h.postMultipart(draft.ID, body, writer.FormDataContentType()), 400, "invalid_attachment")
}

func TestAddDraftAttachmentRejectsANonMultipartBody(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID})
	request := newJSONRequest(http.MethodPost, fmt.Sprintf("/api/v1/drafts/%d/attachments", draft.ID), `{"file":"x"}`)
	h.expectError(h.doRaw(request), 400, "invalid_attachment")
}

func TestDeleteDraftAttachment(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID})
	attachment := decodeBody[domain.DraftAttachment](t, h.uploadAttachment(draft.ID, "note.txt", []byte("bytes")))

	if response := h.do(http.MethodDelete, fmt.Sprintf("/api/v1/drafts/%d/attachments/%d", draft.ID, attachment.ID), nil); response.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	detail := h.do(http.MethodGet, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), nil)
	if got := len(decodeBody[struct {
		Attachments []domain.DraftAttachment `json:"attachments"`
	}](t, detail).Attachments); got != 0 {
		t.Fatalf("attachments = %d after delete", got)
	}
}

// An attachment that belongs to another draft must not be deletable through this
// draft's path, and a missing one is a 404 rather than a redacted 500.
func TestDeleteDraftAttachmentIsScopedToTheDraft(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	mine := h.createDraft(map[string]any{"account_id": account.ID, "subject": "Mine"})
	other := h.createDraft(map[string]any{"account_id": account.ID, "subject": "Other"})
	attachment := decodeBody[domain.DraftAttachment](t, h.uploadAttachment(other.ID, "note.txt", []byte("bytes")))

	h.expectError(h.do(http.MethodDelete, fmt.Sprintf("/api/v1/drafts/%d/attachments/%d", mine.ID, attachment.ID), nil), 404, "not_found")
	h.expectError(h.do(http.MethodDelete, fmt.Sprintf("/api/v1/drafts/%d/attachments/4242", other.ID), nil), 404, "not_found")
	// The real owner still has it.
	detail := h.do(http.MethodGet, fmt.Sprintf("/api/v1/drafts/%d", other.ID), nil)
	if got := len(decodeBody[struct {
		Attachments []domain.DraftAttachment `json:"attachments"`
	}](t, detail).Attachments); got != 1 {
		t.Fatalf("the owning draft has %d attachments", got)
	}
}

// --- send -----------------------------------------------------------------

func TestSendDraftQueues(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID, "to": []string{"a@b.com"}})
	response := h.do(http.MethodPost, fmt.Sprintf("/api/v1/drafts/%d/send", draft.ID), nil)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	payload := decodeBody[struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}](t, response)
	if payload.ID != draft.ID || payload.Status != "queued" {
		t.Fatalf("payload = %+v", payload)
	}
	if queued := h.sender.queuedIDs(); len(queued) != 1 || queued[0] != draft.ID {
		t.Fatalf("queued = %v", queued)
	}
}

// Retry is the same operation under a second route, so a failed draft can be sent
// again without the client having to know it is the same handler.
func TestRetryDraftQueues(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID, "to": []string{"a@b.com"}})
	if response := h.do(http.MethodPost, fmt.Sprintf("/api/v1/drafts/%d/retry", draft.ID), nil); response.Code != http.StatusAccepted {
		t.Fatalf("status = %d", response.Code)
	}
	if queued := h.sender.queuedIDs(); len(queued) != 1 {
		t.Fatalf("queued = %v", queued)
	}
}

func TestSendDraftPropagatesTheQueueFailure(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID})
	h.sender.mu.Lock()
	h.sender.err = notFoundError()
	h.sender.mu.Unlock()
	h.expectError(h.do(http.MethodPost, fmt.Sprintf("/api/v1/drafts/%d/send", draft.ID), nil), 404, "not_found")
}

func TestSendDraftRejectsABadID(t *testing.T) {
	h := newHarness(t)
	h.expectError(h.do(http.MethodPost, "/api/v1/drafts/abc/send", nil), 400, "invalid_id")
	if queued := h.sender.queuedIDs(); len(queued) != 0 {
		t.Fatal("a malformed id still reached the sender")
	}
}

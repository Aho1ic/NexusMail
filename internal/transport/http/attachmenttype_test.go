package http

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"

	"nexusmail/internal/domain"
)

// uploadWithPartHeader uploads one part with headers the caller controls, which
// CreateFormFile cannot do: it always writes a Content-Type of its own, so the
// handler's fallback is unreachable through the ordinary helper.
func (h *harness) uploadWithPartHeader(draftID int64, header textproto.MIMEHeader, content []byte) *httptest.ResponseRecorder {
	h.t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreatePart(header)
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

// A part that declares no Content-Type has to be stored as application/octet-stream.
// The stored value is what Compose writes into the MIME part on send, and an empty
// Content-Type there is not a missing header — it is a malformed one, which leaves the
// recipient's client guessing at the part and commonly showing it as unopenable.
//
// A client can legitimately omit it: it is optional in multipart/form-data, and
// defaulting to octet-stream is what RFC 2045 specifies for an absent type.
func TestAddDraftAttachmentDefaultsAnAbsentContentType(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID})

	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="notes.bin"`)
	response := h.uploadWithPartHeader(draft.ID, header, []byte("no declared type"))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	attachment := decodeBody[domain.DraftAttachment](t, response)
	if attachment.ContentType != "application/octet-stream" {
		t.Errorf("content_type = %q, want application/octet-stream: an empty type is written into the MIME part on send and the recipient cannot open it", attachment.ContentType)
	}

	// The stored row carries it too, since that is what the send path reads rather
	// than the response body.
	stored := draftAttachments(t, h, draft.ID)
	if len(stored) != 1 {
		t.Fatalf("the draft holds %d attachments, want 1", len(stored))
	}
	if stored[0].ContentType != "application/octet-stream" {
		t.Errorf("stored content_type = %q, want application/octet-stream", stored[0].ContentType)
	}
}

// A declared type is kept as sent. Overwriting it would mislabel every attachment
// whose type the client did state, which is the common case.
func TestAddDraftAttachmentKeepsADeclaredContentType(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	draft := h.createDraft(map[string]any{"account_id": account.ID})

	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="report.pdf"`)
	header.Set("Content-Type", "application/pdf")
	response := h.uploadWithPartHeader(draft.ID, header, []byte("%PDF-1.4"))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if attachment := decodeBody[domain.DraftAttachment](t, response); attachment.ContentType != "application/pdf" {
		t.Errorf("content_type = %q, want application/pdf", attachment.ContentType)
	}
}

func draftAttachments(t *testing.T, h *harness, draftID int64) []domain.DraftAttachment {
	t.Helper()
	_, attachments, err := h.repo.GetDraft(context.Background(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	return attachments
}

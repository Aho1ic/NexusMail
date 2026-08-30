//go:build sqlite_fts5

package http

import (
	"fmt"
	"net/http"
	"testing"
)

// A dead database must produce a clean 500 on every read path, not a panic and not
// an empty 200 the client would render as "no mail".
func TestReadPathsFailCleanlyWithoutADatabase(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	inbox := h.seedMailbox(account.ID, "INBOX", "inbox")
	id := h.seedMessage(account.ID, inbox.ID, 1, seedOptions{subject: "Before the outage"})
	if err := h.repo.Close(); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/api/v1/accounts",
		fmt.Sprintf("/api/v1/accounts/%d/mailboxes", account.ID),
		"/api/v1/messages",
		fmt.Sprintf("/api/v1/messages/%d", id),
		"/api/v1/drafts",
		"/api/v1/drafts/1",
	} {
		response := h.do(http.MethodGet, path, nil)
		if response.Code != http.StatusInternalServerError {
			t.Errorf("GET %s = %d, want 500 (body %s)", path, response.Code, response.Body.String())
			continue
		}
		envelope := decodeBody[errorEnvelope](t, response)
		if envelope.Error.Code != "internal_error" || envelope.Error.Message != "internal server error" {
			t.Errorf("GET %s envelope = %+v", path, envelope.Error)
		}
	}
}

// Write paths fail the same way, and the caller is told rather than being handed a
// success for a change that never landed.
func TestWritePathsFailCleanlyWithoutADatabase(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	if err := h.repo.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/api/v1/accounts", map[string]any{"provider": "qq", "email": "a@qq.com", "auth": map[string]string{"password": "x"}}},
		{http.MethodPost, "/api/v1/drafts", map[string]any{"account_id": account.ID}},
		{http.MethodPost, "/api/v1/messages/mark-read", nil},
	} {
		response := h.do(tc.method, tc.path, tc.body)
		if response.Code != http.StatusInternalServerError {
			t.Errorf("%s %s = %d, want 500 (body %s)", tc.method, tc.path, response.Code, response.Body.String())
		}
	}
	// A failed account creation must not have been handed to the supervisor.
	if started, _, _, _ := h.provider.counts(); started != 0 {
		t.Fatal("an account that was not stored still started a sync")
	}
}

// Handler is what main.go serves, so it has to be the same router the tests drive.
func TestHandlerServesTheSameRoutes(t *testing.T) {
	h := newHarness(t)
	handler := h.server.Handler()
	if handler == nil {
		t.Fatal("Handler returned nil")
	}
	response := recordThrough(handler, http.MethodGet, "/healthz")
	if response.Code != http.StatusOK {
		t.Fatalf("healthz through Handler = %d", response.Code)
	}
	// And it is authenticated, so the wired handler is not a bare mux.
	if got := recordThrough(handler, http.MethodGet, "/api/v1/accounts").Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request through Handler = %d, want 401", got)
	}
}

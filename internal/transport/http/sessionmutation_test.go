//go:build sqlite_fts5

package http

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sessionservice "nexusmail/internal/service/session"
)

func TestMarkReadRequiresCSRFFromTheCurrentCookieSession(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	ctx := context.Background()
	sessions := sessionservice.New(h.repo, testAPIKey, time.Hour, 24*time.Hour)
	_, oldCSRF, _, err := sessions.Create(ctx, testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	token, currentCSRF, _, err := sessions.Create(ctx, testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	messagePath := fmt.Sprintf("/api/v1/messages/%d", fixture.ids[0])

	for _, item := range []struct {
		method string
		path   string
		csrf   string
		status int
	}{
		{http.MethodGet, "/api/v1/messages", "", http.StatusOK},
		{http.MethodPatch, messagePath, oldCSRF, http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/messages/mark-read", oldCSRF, http.StatusUnauthorized},
		{http.MethodPatch, messagePath, currentCSRF, http.StatusOK},
		{http.MethodPost, "/api/v1/messages/mark-read", currentCSRF, http.StatusOK},
	} {
		request := httptest.NewRequest(item.method, item.path, strings.NewReader(`{"is_read":true}`))
		request.AddCookie(&http.Cookie{Name: sessionservice.CookieName, Value: token})
		request.Header.Set("X-CSRF-Token", item.csrf)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://localhost:13737")
		response := httptest.NewRecorder()
		h.router.ServeHTTP(response, request)
		if response.Code != item.status {
			t.Fatalf("%s status = %d, want %d", item.method, response.Code, item.status)
		}
		if response.Code == http.StatusUnauthorized {
			stored, _, err := h.repo.GetMessage(ctx, fixture.ids[0])
			if err != nil {
				t.Fatal(err)
			}
			if stored.IsRead {
				t.Fatal("a rejected request marked the message read")
			}
		}
	}
	stored, _, err := h.repo.GetMessage(ctx, fixture.ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if !stored.IsRead {
		t.Fatal("the matching session and CSRF token did not mark the message read")
	}
}

// A websocket authenticates once at the upgrade and then streams events — otp_code
// included — for as long as the socket lives. The watcher is what makes a logout or
// an expired TTL actually disconnect it, so it has to cancel the context the hub is
// serving on rather than wait for the client to go away.
func TestWatchSessionCancelsWhenTheSessionIsGone(t *testing.T) {
	h := newHarness(t)
	token, _, _, err := h.server.sessions.Create(context.Background(), testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Still valid: the watcher must leave a live session alone.
	go h.server.watchSession(ctx, cancel, token, time.Millisecond)
	select {
	case <-ctx.Done():
		t.Fatal("the watcher cancelled a socket whose session is still valid")
	case <-time.After(50 * time.Millisecond):
	}

	if err := h.server.sessions.Delete(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the socket outlived the session it was opened with")
	}
}

// The watcher runs on a context the handler defers cancel on, so it must exit with
// it: a goroutine per socket that never returns is a leak for the life of the
// process.
func TestWatchSessionExitsWithTheHandler(t *testing.T) {
	h := newHarness(t)
	token, _, _, err := h.server.sessions.Create(context.Background(), testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.server.watchSession(ctx, cancel, token, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher is still running after its context was cancelled")
	}
}

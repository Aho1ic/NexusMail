//go:build sqlite_fts5

package http

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"nexusmail/internal/transport/http/static"
)

// hasBundle reports whether the embedded SPA is present. The dist directory is
// produced by make web-build, so a bare `go test` may find only the placeholder.
func hasBundle() bool {
	root, err := fs.Sub(static.Files, "dist")
	if err != nil {
		return false
	}
	_, err = fs.Stat(root, "index.html")
	return err == nil
}

// An unknown API route must be a JSON 404, never the SPA shell: a client parsing
// index.html as an API response is a far worse failure than a clean error.
func TestUnknownAPIRouteIsJSON(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/api/v1/nope", "/api/v1/messages/1/nope", "/api/v2/messages", "/api/"} {
		response := h.do(http.MethodGet, path, nil)
		h.expectError(response, 404, "not_found")
		if strings.Contains(response.Body.String(), "<html") {
			t.Fatalf("%s served the SPA shell", path)
		}
	}
}

// A wrong method on a real route must not fall through to the SPA either.
func TestWrongMethodOnAnAPIRouteIsJSON(t *testing.T) {
	h := newHarness(t)
	response := h.do(http.MethodPut, "/api/v1/messages", nil)
	if response.Code == http.StatusOK {
		t.Fatal("PUT /api/v1/messages was accepted")
	}
	if strings.Contains(response.Body.String(), "<html") {
		t.Fatal("a wrong method served the SPA shell")
	}
}

// Any non-API path that is not a real asset gets index.html, which is what makes
// a deep link like /message/42 survive a page reload.
func TestSPAFallbackServesTheShell(t *testing.T) {
	if !hasBundle() {
		t.Skip("no embedded SPA bundle; run make web-build")
	}
	h := newHarness(t)
	for _, path := range []string{"/", "/inbox", "/message/42", "/deep/link/that/does/not/exist"} {
		response := h.plain(http.MethodGet, path)
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, response.Code)
		}
		if !strings.Contains(response.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s Content-Type = %q", path, response.Header().Get("Content-Type"))
		}
	}
}

// The SPA is public — it has to load before the user can authenticate — but it
// must still carry the security headers, since it is the page that holds the
// session cookie.
func TestSPAIsPublicButHardened(t *testing.T) {
	if !hasBundle() {
		t.Skip("no embedded SPA bundle; run make web-build")
	}
	h := newHarness(t)
	response := h.plain(http.MethodGet, "/")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := response.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if csp := response.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q, want frame-ancestors 'none'", csp)
	}
}

// A traversal in the asset path must not escape the embedded filesystem. The path
// is cleaned before the lookup, so ../ walks up to the root and then falls back to
// index.html rather than reading a file outside dist.
func TestSPADoesNotServeFilesOutsideDist(t *testing.T) {
	if !hasBundle() {
		t.Skip("no embedded SPA bundle; run make web-build")
	}
	h := newHarness(t)
	for _, path := range []string{
		"/../server.go", "/../../go.mod", "/assets/../../store.go",
		"/%2e%2e/%2e%2e/go.mod",
	} {
		response := h.plain(http.MethodGet, path)
		body := response.Body.String()
		if strings.Contains(body, "package ") || strings.Contains(body, "module nexusmail") {
			t.Fatalf("%s leaked a source file", path)
		}
	}
}

// The websocket route is behind authentication like every other endpoint: an
// unauthenticated upgrade must be refused before it reaches the hub.
func TestWebsocketRequiresAuthentication(t *testing.T) {
	h := newHarness(t)
	if response := h.plain(http.MethodGet, "/api/v1/ws"); response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

// A request that is authenticated but is not a valid upgrade must fail cleanly
// rather than panicking on the hijack that httptest cannot support.
func TestWebsocketRejectsANonUpgradeRequest(t *testing.T) {
	h := newHarness(t)
	response := h.do(http.MethodGet, "/api/v1/ws", nil)
	if response.Code == http.StatusOK {
		t.Fatalf("a plain GET was accepted as a websocket upgrade")
	}
	if h.plain(http.MethodGet, "/healthz").Code != http.StatusOK {
		t.Fatal("the server stopped answering after the failed upgrade")
	}
}

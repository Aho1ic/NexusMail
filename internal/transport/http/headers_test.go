//go:build sqlite_fts5

package http

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"nexusmail/internal/transport/http/static"
)

// An API response carries what a cache must never keep: message body_text and
// body_html, the otp_code derived on read, and the csrf_token in the login reply.
// With no Cache-Control and no Expires all of it is heuristically cacheable, so a
// browser may write it to disk and an intermediate cache that was never configured
// otherwise may store it. no-store is the only value that forbids a stored copy.
func TestAPIResponsesAreNotStorable(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)

	for _, probe := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/accounts", nil},
		{http.MethodGet, "/api/v1/messages", nil},
		{http.MethodGet, "/api/v1/messages/" + strconv.FormatInt(fixture.ids[0], 10), nil},
		{http.MethodGet, "/api/v1/drafts", nil},
		// The unknown-route 404 and the bare /api both answer through the SPA
		// fallback, which must not hand them the shell's policy either.
		{http.MethodGet, "/api/v1/nope", nil},
		{http.MethodGet, "/api", nil},
	} {
		response := h.do(probe.method, probe.path, probe.body)
		if got := response.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s %s Cache-Control = %q, want no-store (status %d)", probe.method, probe.path, got, response.Code)
		}
	}
}

// The login reply is the single most sensitive body on the API: it hands out the
// CSRF token. Checked separately because it is the one route that is reached
// without credentials.
func TestLoginResponseIsNotStorable(t *testing.T) {
	h := newHarness(t)
	response := httptest.NewRecorder()
	h.router.ServeHTTP(response, loginRequest(testAPIKey, ""))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store: the body carries csrf_token", got)
	}
}

// index.html is the one file whose name never changes, so it is the only way a
// browser learns the current hashed asset names. Served from cache after a deploy
// it asks for assets that no longer exist — the "still seeing the old SPA after a
// release" report. no-cache still allows storage; it just forbids reuse without
// revalidating, which is exactly the intent.
func TestTheShellMustBeRevalidated(t *testing.T) {
	requireBundle(t)
	h := newHarness(t)
	for _, path := range []string{"/", "/inbox", "/message/42"} {
		response := h.plain(http.MethodGet, path)
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, response.Code)
		}
		if got := response.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", path, got)
		}
	}
}

// Content-hashed assets are the opposite case: the bytes behind one of these names
// can never change, so a year of caching costs nothing and saves every reload.
func TestHashedAssetsArePinned(t *testing.T) {
	requireBundle(t)
	h := newHarness(t)
	asset := bundleAsset(t, ".js")
	if !strings.HasPrefix(asset, "assets/") {
		t.Skipf("%s is not under assets/, so it carries no content hash", asset)
	}
	response := h.plain(http.MethodGet, "/"+asset)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /%s = %d", asset, response.Code)
	}
	cacheControl := response.Header().Get("Cache-Control")
	for _, want := range []string{"immutable", "max-age=31536000"} {
		if !strings.Contains(cacheControl, want) {
			t.Errorf("GET /%s Cache-Control = %q, want it to name %s", asset, cacheControl, want)
		}
	}
}

// A file copied verbatim from web/public keeps its name across builds, so it must
// not be pinned: sw.js is the one that matters, because a year-old worker keeps
// controlling its scope and the notification path silently stays on old code.
func TestUnhashedBundleFilesAreNotPinned(t *testing.T) {
	requireBundle(t)
	root, err := fs.Sub(static.Files, "dist")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	for _, name := range []string{"sw.js", "favicon.svg"} {
		if _, statErr := fs.Stat(root, name); statErr != nil {
			continue
		}
		response := h.plain(http.MethodGet, "/"+name)
		if response.Code != http.StatusOK {
			t.Fatalf("GET /%s = %d", name, response.Code)
		}
		if got := response.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET /%s Cache-Control = %q, want no-cache: the name is reused by every build", name, got)
		}
	}
}

// A stale asset URL is what a cached shell requests after a rebuild changed the
// hashes. It falls through to the shell, and it must carry the shell's policy: had
// immutable been set on the /assets/ request prefix instead, that response would
// pin index.html for a year and strand the tab permanently.
func TestAStaleAssetURLIsNotPinnedToTheShell(t *testing.T) {
	requireBundle(t)
	h := newHarness(t)
	response := h.plain(http.MethodGet, "/assets/index-DOESNOTEXIST.js")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want the shell at 200", response.Code)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache: this response is index.html", got)
	}
}

// The correlation id is echoed into a response header, into the error envelope
// every client parses and into two slog attributes, so an unbounded client value is
// a log-volume multiplier and a forgeable identity. A rejected value is replaced
// rather than sanitised: a generated id is always usable.
func TestRequestIDRejectsUnusableClientValues(t *testing.T) {
	h := newHarness(t)
	for name, offered := range map[string]string{
		"too long":          strings.Repeat("a", maxRequestIDLen+1),
		"absurdly long":     strings.Repeat("b", 64*1024),
		"newline":           "abc\ndef",
		"carriage return":   "abc\rdef",
		"null byte":         "abc\x00def",
		"tab":               "abc\tdef",
		"escape":            "abc\x1bdef",
		"delete":            "abc\x7f",
		"non ascii":         "请求-1",
		"utf8 continuation": "a\xc3\xa9b",
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			request.Header.Set("X-Request-ID", offered)
			response := httptest.NewRecorder()
			h.router.ServeHTTP(response, request)

			echoed := response.Header().Get("X-Request-ID")
			if echoed == offered {
				t.Fatalf("the offered value was echoed verbatim: %q", echoed)
			}
			if echoed == "" {
				t.Fatal("no request id was generated")
			}
			if len(echoed) > maxRequestIDLen {
				t.Fatalf("generated id is %d bytes", len(echoed))
			}
			if !usableRequestID(echoed) {
				t.Fatalf("generated id %q is itself not usable", echoed)
			}
		})
	}
}

// A well-formed id is still honoured: that is the whole point of accepting one, so
// a trace can be followed from the proxy that issued it into these logs.
func TestRequestIDKeepsAUsableClientValue(t *testing.T) {
	h := newHarness(t)
	for _, offered := range []string{
		"7f3c1a2b-4d5e-6f70-8192-a3b4c5d6e7f8",
		"trace-42",
		strings.Repeat("c", maxRequestIDLen),
		"with space",
		"~!@#$%^&*()_+{}|:\"<>?",
	} {
		request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		request.Header.Set("X-Request-ID", offered)
		response := httptest.NewRecorder()
		h.router.ServeHTTP(response, request)
		if got := response.Header().Get("X-Request-ID"); got != offered {
			t.Errorf("X-Request-ID = %q, want the offered %q", got, offered)
		}
	}
}

// The rejected value must not reach the error envelope either, which is the copy a
// client stores and quotes back in a support request.
func TestRejectedRequestIDDoesNotReachTheErrorEnvelope(t *testing.T) {
	h := newHarness(t)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/messages/0", nil)
	request.Header.Set("X-API-Key", testAPIKey)
	request.Header.Set("X-Request-ID", strings.Repeat("z", 4096))
	response := httptest.NewRecorder()
	h.router.ServeHTTP(response, request)

	envelope := h.expectError(response, 400, "invalid_id")
	if strings.Contains(envelope.Error.RequestID, "zzzz") {
		t.Fatalf("request_id carries the oversized client value: %q", envelope.Error.RequestID)
	}
	if envelope.Error.RequestID != response.Header().Get("X-Request-ID") {
		t.Fatalf("envelope request_id %q disagrees with the header %q", envelope.Error.RequestID, response.Header().Get("X-Request-ID"))
	}
}

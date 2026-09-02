//go:build sqlite_fts5

package http

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"nexusmail/internal/transport/http/static"
)

// The SPA route has two answers: a real file in the embedded bundle is served as
// itself, and everything else gets index.html so a deep link survives a reload.
// Only the fallback was covered. That asymmetry hides the worse failure of the
// two — if the asset branch stops matching, every script and stylesheet request
// answers 200 with index.html and text/html, the browser refuses to execute them,
// and the app is a blank page. The fallback tests all still pass, because the
// fallback is exactly what a broken asset lookup produces.

// bundleAsset finds a real file in the embedded bundle with the given extension,
// so the assertions run against whatever hashed filenames the current build
// produced rather than a name pinned in the test.
func bundleAsset(t *testing.T, extension string) string {
	t.Helper()
	root, err := fs.Sub(static.Files, "dist")
	if err != nil {
		t.Fatalf("embedded bundle: %v", err)
	}
	found := ""
	err = fs.WalkDir(root, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if found == "" && !entry.IsDir() && strings.HasSuffix(path, extension) && path != "index.html" {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk bundle: %v", err)
	}
	if found == "" {
		t.Skipf("no %s in the embedded bundle", extension)
	}
	return found
}

// TestSPAServesARealAssetAsItself covers the fs.Stat hit. The Content-Type is the
// assertion that matters: a JavaScript module answered as text/html is refused by
// the browser, so the page loads and then does nothing.
func TestSPAServesARealAssetAsItself(t *testing.T) {
	if !hasBundle() {
		t.Skip("no embedded SPA bundle; run make web-build")
	}
	h := newHarness(t)

	for _, probe := range []struct{ extension, wantType string }{
		{".js", "javascript"},
		{".css", "css"},
		{".svg", "svg"},
	} {
		asset := bundleAsset(t, probe.extension)
		response := h.plain(http.MethodGet, "/"+asset)
		if response.Code != http.StatusOK {
			t.Errorf("GET /%s = %d, want 200", asset, response.Code)
			continue
		}
		contentType := response.Header().Get("Content-Type")
		if !strings.Contains(contentType, probe.wantType) {
			t.Errorf("GET /%s Content-Type = %q, want it to name %s: the browser will not execute a module served as HTML",
				asset, contentType, probe.wantType)
		}
		// The decisive check: the asset is its own bytes, not the shell. A broken
		// lookup answers 200 with index.html, which the status and a JavaScript
		// body containing an incidental HTML string cannot catch on their own.
		want, err := fs.ReadFile(static.Files, "dist/"+asset)
		if err != nil {
			t.Fatalf("read %s: %v", asset, err)
		}
		if response.Body.String() != string(want) {
			t.Errorf("GET /%s served bytes different from the embedded asset (got %d bytes, want %d): it may have fallen through to index.html", asset, response.Body.Len(), len(want))
		}
	}
}

// TestSPAServesTheServiceWorkerFromTheRoot pins sw.js specifically. A service
// worker only controls the scope it is served from, so answering it with the shell
// silently disables the notification path the OTP feature depends on — and does so
// without any request failing.
func TestSPAServesTheServiceWorkerFromTheRoot(t *testing.T) {
	if !hasBundle() {
		t.Skip("no embedded SPA bundle; run make web-build")
	}
	root, err := fs.Sub(static.Files, "dist")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(root, "sw.js"); err != nil {
		t.Skip("no sw.js in the embedded bundle")
	}

	h := newHarness(t)
	response := h.plain(http.MethodGet, "/sw.js")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /sw.js = %d, want 200", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "javascript") {
		t.Errorf("GET /sw.js Content-Type = %q, want it to name javascript: a worker served as HTML never registers", contentType)
	}
	if strings.Contains(response.Body.String(), "<!doctype html") || strings.Contains(response.Body.String(), "<!DOCTYPE html") {
		t.Error("GET /sw.js served the SPA shell; push notifications would stop working with no request failing")
	}
}

// TestSPAFallsBackForAMissingAssetPath is the other side of the same branch: a
// path that looks like an asset but is not in the bundle must still reach the
// shell, because that is a stale cached URL after a rebuild changed the hashes,
// and answering 404 there strands the tab on an empty page.
func TestSPAFallsBackForAMissingAssetPath(t *testing.T) {
	if !hasBundle() {
		t.Skip("no embedded SPA bundle; run make web-build")
	}
	h := newHarness(t)

	response := h.plain(http.MethodGet, "/assets/index-DOESNOTEXIST.js")
	if response.Code != http.StatusOK {
		t.Fatalf("a stale asset URL = %d, want the shell at 200", response.Code)
	}
	if !strings.Contains(response.Header().Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type = %q, want text/html", response.Header().Get("Content-Type"))
	}
}

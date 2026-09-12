//go:build sqlite_fts5

package http

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nexusmail/internal/config"

	"github.com/gin-gonic/gin"
)

// httptestPeer is the RemoteAddr net/http/httptest assigns every synthetic
// request. Declaring it as the trusted proxy is what makes X-Forwarded-For
// meaningful in these tests, exactly as declaring the real reverse proxy does in a
// deployment.
const httptestPeer = "192.0.2.1"

// withTrustedProxies rebuilds the harness router with a declared proxy list. The
// router is built from cfg, so the list has to be in place before routes() runs.
func (h *harness) withTrustedProxies(entries ...string) {
	h.t.Helper()
	h.server.cfg.TrustedProxies = entries
	h.router = h.server.routes()
}

// The throttle is per client, and behind a declared proxy the client is whatever
// X-Forwarded-For says. This is the behaviour the config validation exists to
// protect: when the proxy list fails to parse, gin keeps no trusted CIDR at all,
// ClientIP() falls back to the peer address, and every client in the deployment
// collapses into one bucket — five requests from anyone then deny everyone.
func TestDeclaredProxyGivesEachClientItsOwnBucket(t *testing.T) {
	h := newHarness(t)
	h.withTrustedProxies(httptestPeer)

	clients := []string{"203.0.113.7", "203.0.113.8", "203.0.113.9"}
	for _, client := range clients {
		for attempt := 0; attempt < loginRateLimit; attempt++ {
			if code := postLogin(h.router, "wrong-key-of-a-plausible-length-here", client); code != http.StatusUnauthorized {
				t.Fatalf("client %s attempt %d = %d, want 401: the buckets are shared", client, attempt, code)
			}
		}
	}
	// Each client spent only its own budget, so each is refused independently.
	for _, client := range clients {
		if code := postLogin(h.router, "wrong-key-of-a-plausible-length-here", client); code != http.StatusTooManyRequests {
			t.Fatalf("client %s past its budget = %d, want 429", client, code)
		}
	}
	h.server.rateMu.Lock()
	buckets := len(h.server.rate)
	h.server.rateMu.Unlock()
	if buckets != len(clients) {
		t.Fatalf("%d buckets for %d distinct clients: they are not being told apart", buckets, len(clients))
	}
}

// A client the declared proxy has not vouched for must not be able to claim an
// address: the header is only trusted one hop, so a forged value from an untrusted
// peer is ignored and the peer address is used instead.
func TestUndeclaredProxyCannotClaimAnAddress(t *testing.T) {
	h := newHarness(t)
	h.withTrustedProxies("10.9.9.9")

	for attempt := 0; attempt < loginRateLimit; attempt++ {
		if code := postLogin(h.router, "wrong-key-of-a-plausible-length-here", fmt.Sprintf("203.0.113.%d", attempt)); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", attempt, code)
		}
	}
	if code := postLogin(h.router, "wrong-key-of-a-plausible-length-here", "203.0.113.200"); code != http.StatusTooManyRequests {
		t.Fatalf("a forged address from an untrusted peer bought a new budget: %d", code)
	}
}

// The invariant that keeps routes() from panicking: whatever config.Load returns,
// gin must accept. They are two separate parsers over one setting, and a list gin
// rejects leaves it with no trusted CIDR at all — ClientIP() then reports the peer
// address and the whole deployment collapses into one throttle bucket, which is
// precisely the failure the startup validation exists to prevent.
//
// The converse is checked too, on the value config actually hands over rather than
// on the raw setting: config normalises (trims, drops blanks) before gin ever sees
// an entry, so comparing against the untrimmed string would only be measuring the
// normalisation. Being stricter than gin would refuse a list the deployment could
// have used.
func TestConfigAndGinAgreeOnTrustedProxies(t *testing.T) {
	for _, entry := range []string{
		"10.0.0.1", "127.0.0.1", "192.168.1.1",
		"172.16.0.0/12", "10.0.0.0/8", "0.0.0.0/0",
		"::1", "2001:db8::1", "fd00::/8", "::/0", "::ffff:10.0.0.1",
		"not-a-cidr", "proxy.internal", "10.0.0", "10.0.0.256",
		"10.0.0.0/33", "10.0.0.0/abc", "10.0.0.0/", "/24",
		"2001:db8::/129", "10.0.0.1/", "  10.0.0.1  ", " ",
		"10.0.0.0/8/8", "300.300.300.300", "0x0a000001",
	} {
		t.Run(entry, func(t *testing.T) {
			loaded, err := loadWithProxies(t, entry)
			if err == nil {
				if ginErr := gin.New().SetTrustedProxies(loaded); ginErr != nil {
					t.Fatalf("config accepted %q as %v but gin rejects it: %v — routes() would panic", entry, loaded, ginErr)
				}
				return
			}
			// Refused: gin must refuse the same normalised entry, or config is stricter
			// than the library it is guarding.
			normalised := strings.TrimSpace(entry)
			if normalised == "" {
				t.Fatalf("a blank entry must be dropped, not refused: %v", err)
			}
			if ginErr := gin.New().SetTrustedProxies([]string{normalised}); ginErr == nil {
				t.Fatalf("config refused %q but gin accepts it", normalised)
			}
		})
	}
}

// loadWithProxies runs the real config.Load with one proxy entry, so the answer
// includes the trimming and splitting the setting goes through on the way in.
func loadWithProxies(t *testing.T, entry string) ([]string, error) {
	t.Helper()
	t.Setenv("NEXUSMAIL_API_KEY", testAPIKey)
	t.Setenv("NEXUSMAIL_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("NEXUSMAIL_TRUSTED_PROXIES", entry)
	cfg, err := config.Load()
	return cfg.TrustedProxies, err
}

// The router must never be built with a list gin rejects. config.Load is what
// guarantees that, so this asserts the guard rather than the fallback that used to
// stand in for it: the old code logged a warning and continued with no trusted
// proxy at all, turning an operator's typo into a permanent lockout of the real
// user.
func TestRoutesRefusesToBuildWithABrokenProxyList(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.TrustedProxies = []string{"not-a-cidr"}
	defer func() {
		if recover() == nil {
			t.Fatal("routes() accepted a proxy list gin rejects, so ClientIP() would silently fall back to the peer address")
		}
	}()
	h.server.routes()
}

// The forged-header case through the default configuration, kept alongside the
// declared-proxy case above: with no proxy declared the header is worthless and
// every attempt lands in the one bucket the peer address names.
func TestNoDeclaredProxyIgnoresForwardedFor(t *testing.T) {
	h := newHarness(t)
	if len(h.server.cfg.TrustedProxies) != 0 {
		t.Fatalf("the harness declares proxies: %v", h.server.cfg.TrustedProxies)
	}
	for attempt := 0; attempt < loginRateLimit; attempt++ {
		response := httptest.NewRecorder()
		h.router.ServeHTTP(response, loginRequest("wrong-key-of-a-plausible-length-here", fmt.Sprintf("198.51.100.%d", attempt)))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", attempt, response.Code)
		}
	}
	if code := postLogin(h.router, "wrong-key-of-a-plausible-length-here", "198.51.100.250"); code != http.StatusTooManyRequests {
		t.Fatalf("a forged address bought a new budget: %d", code)
	}
}

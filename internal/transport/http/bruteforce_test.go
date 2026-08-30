//go:build sqlite_fts5

package http

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sessionservice "nexusmail/internal/service/session"
)

// The API key is the only credential, so the two channels that accept it are the
// whole attack surface. The tests below hammer both.

// A guessing run must hit a ceiling. Both channels are keyed on the client
// address, so an attacker gets one budget per address and no more.
func TestBruteForceLoginHitsTheCeiling(t *testing.T) {
	h := newHarness(t)
	statuses := map[int]int{}
	for i := 0; i < 200; i++ {
		response := httptest.NewRecorder()
		h.router.ServeHTTP(response, loginRequest(fmt.Sprintf("guess-%d-padded-to-a-plausible-length", i), ""))
		statuses[response.Code]++
	}
	// The window allows loginRateLimit attempts; everything past that is refused
	// without the credential ever being checked.
	if statuses[http.StatusUnauthorized] != loginRateLimit {
		t.Fatalf("%d attempts were actually evaluated, want %d", statuses[http.StatusUnauthorized], loginRateLimit)
	}
	if statuses[http.StatusTooManyRequests] != 200-loginRateLimit {
		t.Fatalf("statuses = %v", statuses)
	}
	if statuses[http.StatusCreated] != 0 {
		t.Fatal("a guess succeeded")
	}
}

// The correct key must also be refused once the window is spent: a throttle that
// let the right key through would leak whether a guess was correct by timing the
// response class, and it would let an attacker keep trying past the ceiling by
// mixing in one known-bad attempt.
func TestBruteForceCeilingAppliesToTheValidKeyToo(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < loginRateLimit; i++ {
		response := httptest.NewRecorder()
		h.router.ServeHTTP(response, loginRequest("wrong-key-of-a-plausible-length-here", ""))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", i, response.Code)
		}
	}
	response := httptest.NewRecorder()
	h.router.ServeHTTP(response, loginRequest(testAPIKey, ""))
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("the valid key returned %d after the window was spent, want 429", response.Code)
	}
}

// A forged X-Forwarded-For must buy no new budget. Gin trusts every proxy by
// default, which would have made ClientIP() report whatever the header said, so
// each request would land in its own bucket: an unlimited guessing run and an
// unbounded map at the same time.
func TestBruteForceCannotEscapeTheCeilingWithForwardedFor(t *testing.T) {
	h := newHarness(t)
	throttled := 0
	for i := 0; i < 100; i++ {
		response := httptest.NewRecorder()
		h.router.ServeHTTP(response, loginRequest("guess-of-a-plausible-length-here-ok", fmt.Sprintf("10.%d.%d.%d", i/256, i%256, i%251)))
		if response.Code == http.StatusTooManyRequests {
			throttled++
		}
	}
	if throttled != 100-loginRateLimit {
		t.Fatalf("%d of 100 forged-address attempts were throttled, want %d", throttled, 100-loginRateLimit)
	}
	// And the map holds one bucket, not one per forged address.
	h.server.rateMu.Lock()
	buckets := len(h.server.rate)
	h.server.rateMu.Unlock()
	if buckets != 1 {
		t.Fatalf("the rate map holds %d buckets after 100 forged addresses", buckets)
	}
}

// The API key header is the other channel. Only wrong keys are counted, so a
// working integration is never throttled however busy it gets, while guessing runs
// into the same kind of ceiling.
func TestBruteForceAPIKeyHeaderHitsTheCeiling(t *testing.T) {
	h := newHarness(t)
	unauthorized, throttled := 0, 0
	for i := 0; i < 100; i++ {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
		request.Header.Set("X-API-Key", fmt.Sprintf("guess-%d-padded-out-to-look-plausible", i))
		response := httptest.NewRecorder()
		h.router.ServeHTTP(response, request)
		switch response.Code {
		case http.StatusUnauthorized:
			unauthorized++
		case http.StatusTooManyRequests:
			throttled++
		default:
			t.Fatalf("attempt %d returned %d", i, response.Code)
		}
	}
	if unauthorized != apiKeyRateLimit {
		t.Fatalf("%d guesses were evaluated, want %d", unauthorized, apiKeyRateLimit)
	}
	if throttled != 100-apiKeyRateLimit {
		t.Fatalf("throttled = %d", throttled)
	}
}

// A valid key is never counted, so no amount of legitimate traffic can throttle an
// integration out of the API.
func TestValidAPIKeyIsNeverThrottled(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < apiKeyRateLimit*5; i++ {
		if response := h.do(http.MethodGet, "/api/v1/accounts", nil); response.Code != http.StatusOK {
			t.Fatalf("request %d with the valid key returned %d", i, response.Code)
		}
	}
	h.server.rateMu.Lock()
	buckets := len(h.server.rate)
	h.server.rateMu.Unlock()
	if buckets != 0 {
		t.Fatalf("successful requests left %d rate buckets behind", buckets)
	}
}

// Guessing the session cookie is the third surface. An unknown token is refused
// and, unlike the key channels, cannot be enumerated for free either: it fails
// without reaching any handler.
func TestBruteForceSessionCookie(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 200; i++ {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
		request.AddCookie(&http.Cookie{Name: sessionservice.CookieName, Value: fmt.Sprintf("forged-token-%d", i)})
		response := httptest.NewRecorder()
		h.router.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("forged cookie %d returned %d", i, response.Code)
		}
	}
	// No session was created by any of it.
	sessions := h.do(http.MethodGet, "/api/v1/accounts", nil)
	if sessions.Code != http.StatusOK {
		t.Fatalf("the API is still reachable check failed: %d", sessions.Code)
	}
}

// The window slides: an attacker who waits gets a new budget, and a legitimate
// user who mistyped their key once is not locked out permanently. Driven by
// rewinding the recorded timestamps rather than by sleeping a minute.
func TestRateWindowRecovers(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < loginRateLimit; i++ {
		response := httptest.NewRecorder()
		h.router.ServeHTTP(response, loginRequest("wrong-key-of-a-plausible-length-here", ""))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", i, response.Code)
		}
	}
	if code := postLogin(h.router, testAPIKey, ""); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 before the window rolls", code)
	}

	h.server.rateMu.Lock()
	for key, stamps := range h.server.rate {
		rolled := make([]time.Time, len(stamps))
		for i, stamp := range stamps {
			rolled[i] = stamp.Add(-2 * rateWindow)
		}
		h.server.rate[key] = rolled
	}
	h.server.rateMu.Unlock()

	if code := postLogin(h.router, testAPIKey, ""); code != http.StatusCreated {
		t.Fatalf("status = %d after the window rolled, want 201", code)
	}
}

// The bucket map is keyed on caller-controlled input, so it has to be bounded. The
// sweep runs on the same path that inserts, and a bucket whose window has passed is
// dropped rather than kept forever.
func TestRateMapStaysBounded(t *testing.T) {
	h := newHarness(t)
	// Fill the map with buckets from many distinct keys, bypassing the router so
	// the addresses are not collapsed to 192.0.2.1 by httptest.
	for i := 0; i < 5000; i++ {
		h.server.allowAttempt(fmt.Sprintf("login:10.%d.%d.%d", i/65536, (i/256)%256, i%256), loginRateLimit)
	}
	h.server.rateMu.Lock()
	filled := len(h.server.rate)
	// Age every bucket past the window and force the next sweep to run.
	for key, stamps := range h.server.rate {
		rolled := make([]time.Time, len(stamps))
		for i, stamp := range stamps {
			rolled[i] = stamp.Add(-2 * rateWindow)
		}
		h.server.rate[key] = rolled
	}
	h.server.rateSwept = time.Now().Add(-2 * rateSweepEvery)
	h.server.rateMu.Unlock()

	if filled == 0 {
		t.Fatal("no buckets were created")
	}
	h.server.allowAttempt("login:sweep-trigger", loginRateLimit)
	h.server.rateMu.Lock()
	remaining := len(h.server.rate)
	h.server.rateMu.Unlock()
	if remaining > 1 {
		t.Fatalf("%d of %d stale buckets survived the sweep", remaining, filled)
	}
}

// The throttle is shared state on the hot path of every request, so it has to be
// correct under concurrency: the total admitted must be exactly the limit, never
// more, however many goroutines race for the last slot.
func TestRateLimitIsExactUnderConcurrency(t *testing.T) {
	h := newHarness(t)
	const callers = 64
	var admitted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if h.server.allowAttempt("login:127.0.0.1", loginRateLimit) {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if admitted.Load() != int64(loginRateLimit) {
		t.Fatalf("%d of %d concurrent callers were admitted, want exactly %d", admitted.Load(), callers, loginRateLimit)
	}
}

// Concurrent login attempts through the whole router must respect the same
// ceiling, and none may produce a session beyond it.
func TestConcurrentLoginRespectsTheCeiling(t *testing.T) {
	h := newHarness(t)
	const callers = 40
	var created, throttled atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			response := httptest.NewRecorder()
			h.router.ServeHTTP(response, loginRequest(testAPIKey, ""))
			switch response.Code {
			case http.StatusCreated:
				created.Add(1)
			case http.StatusTooManyRequests:
				throttled.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if created.Load() != int64(loginRateLimit) {
		t.Fatalf("%d sessions were created, want %d", created.Load(), loginRateLimit)
	}
	if created.Load()+throttled.Load() != callers {
		t.Fatalf("created %d + throttled %d != %d", created.Load(), throttled.Load(), callers)
	}
}

// A concurrent read burst with a valid key must all succeed: the throttle only
// counts failures, and the store's read path is unlocked.
func TestConcurrentAuthenticatedReadBurst(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(30)
	const callers, each = 16, 8
	var wg sync.WaitGroup
	errs := make(chan string, callers*each)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			for round := 0; round < each; round++ {
				path := "/api/v1/messages?limit=10"
				if index%3 == 0 {
					path = fmt.Sprintf("/api/v1/messages?mailbox_id=%d&limit=10", fixture.inbox.ID)
				}
				if index%3 == 1 {
					path = fmt.Sprintf("/api/v1/messages/%d", fixture.ids[round%len(fixture.ids)])
				}
				request := httptest.NewRequest(http.MethodGet, path, nil)
				request.Header.Set("X-API-Key", testAPIKey)
				response := httptest.NewRecorder()
				h.router.ServeHTTP(response, request)
				if response.Code != http.StatusOK && response.Code != http.StatusAccepted {
					errs <- fmt.Sprintf("GET %s = %d: %s", path, response.Code, response.Body.String())
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for message := range errs {
		t.Fatal(message)
	}
}

// Long credentials must not be treated specially. A comparison that short
// circuited on length, or a bucket key built from the guess, would show up as
// either a success or an unbounded map.
func TestOversizedCredentialsAreRejectedWithoutGrowingTheMap(t *testing.T) {
	h := newHarness(t)
	for _, key := range []string{strings.Repeat("a", 1<<16), "", " ", testAPIKey + "x", testAPIKey[:len(testAPIKey)-1]} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
		if key != "" {
			request.Header.Set("X-API-Key", key)
		}
		response := httptest.NewRecorder()
		h.router.ServeHTTP(response, request)
		if response.Code == http.StatusOK {
			t.Fatalf("a key of length %d was accepted", len(key))
		}
	}
	h.server.rateMu.Lock()
	buckets := len(h.server.rate)
	h.server.rateMu.Unlock()
	// One bucket for the one client address, regardless of how many keys were tried.
	if buckets > 1 {
		t.Fatalf("the rate map holds %d buckets", buckets)
	}
}

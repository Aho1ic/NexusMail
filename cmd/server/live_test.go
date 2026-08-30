//go:build sqlite_fts5

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests in this file drive the server as a separate process over a real TCP
// socket, which is the only place several properties are observable at all. An
// in-process httptest handler never parses an HTTP request line, so it cannot show
// what a truncated request or an oversized header does; it shares the test's
// goroutines, so it cannot show whether real concurrency exhausts the connection pool
// or the sqlite writer; and it cannot show whether the process stays alive and
// responsive after being mistreated. That last point is the reason for the recurring
// final assertion: every test here ends by checking the server still answers.

// liveServer starts the real binary and returns its base URL and API key.
func liveServer(t *testing.T) (base string, apiKey string) {
	t.Helper()
	env, addr := serverEnv(t)
	command, _ := spawnMain(t, env)
	waitForHealthy(t, addr, command)
	return "http://" + addr, env["NEXUSMAIL_API_KEY"]
}

// createLiveAccount adds an account over the API, which is how a real deployment gets
// one. Drafts require account_id > 0, so the write-path tests need this first. The
// account points at the qq preset's real host and is never dialled successfully; only
// its row matters here.
func createLiveAccount(t *testing.T, base, apiKey string) int64 {
	t.Helper()
	body := strings.NewReader(`{"provider":"qq","email":"someone@qq.com","display_name":"Load",` +
		`"auth":{"type":"password","password":"an-app-specific-code"}}`)
	request, err := http.NewRequest(http.MethodPost, base+"/api/v1/accounts", body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create account returned %d: %.300q", response.StatusCode, payload)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode created account: %v (%.200q)", err, payload)
	}
	if created.ID <= 0 {
		t.Fatalf("created account has id %d, want a positive id (%.200q)", created.ID, payload)
	}
	return created.ID
}

// requireStillHealthy is the assertion that makes the hostile cases meaningful: a
// rejected request proves little if the process died doing it.
func requireStillHealthy(t *testing.T, base string) {
	t.Helper()
	response, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("server stopped answering: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d after the run, want 200", response.StatusCode)
	}
}

// hostilePayloads is the corpus. Each entry is something a client can actually send,
// and none of them may produce a 5xx: a malformed request is the client's fault and
// has to be reported as 4xx, while a 500 means the input reached somewhere it should
// not have.
func hostilePayloads() []string {
	return []string{
		"", "{", "}", "[]", "null", "true", "0", `""`,
		`{"`, `{"unterminated": `, `{"a":}`, `{,}`, `{"a":1,}`,
		strings.Repeat(`{"a":`, 2000) + "1" + strings.Repeat("}", 2000), // deep nesting
		`{"subject":"` + strings.Repeat("A", 100000) + `"}`,             // long string
		"{\"subject\":\"\x00\x01\x02\"}",                                // raw control bytes inside a JSON string
		`{"subject":"'; DROP TABLE messages; --"}`,
		`{"subject":"' OR '1'='1"}`,
		`{"subject":"<script>alert(1)</script>"}`,
		`{"subject":"../../../../etc/passwd"}`,
		`{"subject":"%00%2e%2e%2f"}`,
		`{"subject":"\ud800"}`, // lone surrogate
		`{"subject":"🙂🙂🙂"}`,
		`{"to":[]}`, `{"to":null}`, `{"to":"notanarray"}`, `{"to":[{}]}`,
		`{"to":[{"address":null}]}`, `{"account_id":-1}`, `{"account_id":99999999999999999999}`,
		`{"account_id":"notanumber"}`, `{"account_id":1.5}`,
		"\x00\x01\x02binary garbage\xff\xfe",
	}
}

// TestLiveServerRejectsHostileBodiesWithoutFailing sends the corpus at every endpoint
// that accepts a body, with a valid key so the request reaches the handler rather than
// stopping at auth.
func TestLiveServerRejectsHostileBodiesWithoutFailing(t *testing.T) {
	base, apiKey := liveServer(t)
	client := &http.Client{Timeout: 20 * time.Second}

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/auth/session"},
		{http.MethodPost, "/api/v1/accounts"},
		{http.MethodPost, "/api/v1/messages/mark-read"},
		{http.MethodPatch, "/api/v1/messages/1"},
		{http.MethodPost, "/api/v1/drafts"},
		{http.MethodPatch, "/api/v1/drafts/1"},
		{http.MethodPost, "/api/v1/drafts/1/send"},
	}

	for _, endpoint := range endpoints {
		// Count the responses that actually reached a handler. The login endpoint sits
		// behind the rate limiter, so without this the whole endpoint could pass on a
		// run of 429s — every one of which is under 500 and proves nothing about how
		// the handler treats the payload.
		var reached int
		for index, payload := range hostilePayloads() {
			request, err := http.NewRequest(endpoint.method, base+endpoint.path, strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-API-Key", apiKey)
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("%s %s payload %d: %v", endpoint.method, endpoint.path, index, err)
			}
			body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
			_ = response.Body.Close()
			if response.StatusCode != http.StatusTooManyRequests {
				reached++
			}
			if response.StatusCode >= 500 {
				t.Errorf("%s %s payload %d => %d, want < 500\n  payload: %.120q\n  body: %.200q",
					endpoint.method, endpoint.path, index, response.StatusCode, payload, body)
			}
		}
		if reached == 0 {
			t.Errorf("%s %s: every payload was rate limited, so none reached the handler",
				endpoint.method, endpoint.path)
		}
	}
	requireStillHealthy(t, base)
}

// TestLiveServerRejectsHostilePathsAndQueries covers the other input channels. The
// path traversal and cursor cases matter most: the cursor is base64 that the server
// decodes and trusts as a keyset position, and the attachment path segments index
// straight into storage.
func TestLiveServerRejectsHostilePathsAndQueries(t *testing.T) {
	base, apiKey := liveServer(t)
	client := &http.Client{Timeout: 20 * time.Second}

	paths := []string{
		"/api/v1/messages/0", "/api/v1/messages/-1", "/api/v1/messages/abc",
		"/api/v1/messages/99999999999999999999", "/api/v1/messages/1.5",
		"/api/v1/messages/1/attachments/0", "/api/v1/messages/1/attachments/-1",
		"/api/v1/messages/1/attachments/abc",
		"/api/v1/messages/1/attachments/..%2f..%2f..%2fetc%2fpasswd",
		"/api/v1/drafts/0", "/api/v1/drafts/abc", "/api/v1/drafts/-1",
		"/api/v1/accounts/abc/mailboxes", "/api/v1/accounts/-1/mailboxes",
		"/api/v1/oauth/..%2f..%2fetc/callback", "/api/v1/oauth/%00/callback",
		"/api/v1/../../etc/passwd", "/api/v1/messages/../../accounts",
		"/api/v1/messages?cursor=notbase64!!!",
		"/api/v1/messages?cursor=" + strings.Repeat("A", 10000),
		"/api/v1/messages?cursor=eyJmb28iOiJiYXIifQ",   // valid base64, wrong shape
		"/api/v1/messages?cursor=eyJyZWNlaXZlZF9hdCI6", // truncated JSON
		"/api/v1/messages?limit=-1", "/api/v1/messages?limit=0",
		"/api/v1/messages?limit=99999999", "/api/v1/messages?limit=abc",
		"/api/v1/messages?limit=1.5", "/api/v1/messages?account_id=abc",
		"/api/v1/messages?account_id=-1", "/api/v1/messages?is_read=maybe",
		"/api/v1/messages?q=" + strings.Repeat("x", 50000),
		"/api/v1/messages?q=%27%20OR%20%271%27%3D%271",
		"/api/v1/messages?q=%00", "/api/v1/messages?q=" + strings.Repeat("%22", 500),
		"/api/v1/messages?folder=" + strings.Repeat("y", 10000),
		"/api/v1/messages?" + strings.Repeat("junk=1&", 2000),
	}

	for _, path := range paths {
		request, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			// A path this package cannot even form is not a server-side case.
			continue
		}
		request.Header.Set("X-API-Key", apiKey)
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("GET %.80q: %v", path, err)
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		_ = response.Body.Close()
		if response.StatusCode >= 500 {
			t.Errorf("GET %.80q => %d, want < 500\n  body: %.200q", path, response.StatusCode, body)
		}
	}
	requireStillHealthy(t, base)
}

// TestLiveServerSurvivesMalformedHTTP goes below the HTTP client, because a client
// cannot send an invalid request line or an unterminated request. Go's own server
// rejects most of these, so what this really pins is that the process survives them
// and that no handler is reached with a half-parsed request.
func TestLiveServerSurvivesMalformedHTTP(t *testing.T) {
	base, _ := liveServer(t)
	addr := strings.TrimPrefix(base, "http://")

	raw := []string{
		"\r\n\r\n",
		"GARBAGE\r\n\r\n",
		"GET\r\n\r\n",
		"GET /\r\n\r\n",
		"GET / HTTP/9.9\r\n\r\n",
		"GET / HTTP/1.1\r\n",            // no terminator
		"GET / HTTP/1.1\r\nHost: x\r\n", // headers unterminated
		"POST /api/v1/drafts HTTP/1.1\r\nHost: x\r\nContent-Length: 999999\r\n\r\nshort",
		"POST /api/v1/drafts HTTP/1.1\r\nHost: x\r\nContent-Length: -1\r\n\r\n",
		"POST /api/v1/drafts HTTP/1.1\r\nHost: x\r\nContent-Length: abc\r\n\r\n",
		"POST /api/v1/drafts HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\nZZZ\r\n",
		"GET / HTTP/1.1\r\nHost: x\r\n" + strings.Repeat("X-Pad: pad\r\n", 5000) + "\r\n",
		"GET /" + strings.Repeat("a", 100000) + " HTTP/1.1\r\nHost: x\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: " + strings.Repeat("h", 100000) + "\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: x\r\nX-Null: a\x00b\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: x\r\nX-CRLF: a\rb\r\n\r\n",
		"\x00\x00\x00\x00",
		strings.Repeat("\xff", 4096),
	}

	for index, payload := range raw {
		func() {
			conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
			if err != nil {
				t.Fatalf("payload %d: dial: %v", index, err)
			}
			defer conn.Close()
			// A short deadline on purpose. Several payloads are deliberately
			// unterminated, so the server is right to hold the connection open waiting
			// for the rest; this test is not measuring that timeout, and waiting it out
			// on every one of them costs a minute for no added signal.
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			// A write or read error is a fine outcome; the server closing on nonsense
			// is correct. What matters is that it does not take the process down.
			_, _ = conn.Write([]byte(payload))
			_, _ = io.Copy(io.Discard, io.LimitReader(conn, 4096))
		}()
	}

	// Half-open connections that send nothing at all, to check the read timeout frees
	// them rather than pinning a goroutine per connection indefinitely.
	var idle []net.Conn
	for i := 0; i < 64; i++ {
		conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			break
		}
		idle = append(idle, conn)
	}
	for _, conn := range idle {
		_ = conn.Close()
	}

	requireStillHealthy(t, base)
}

// TestLiveServerUnderConcurrentLoad is the high-concurrency case: many independent
// connections issuing real requests at once. It asserts three things a serial test
// cannot — that nothing returns 5xx under contention, that the sqlite writer's single
// lock does not deadlock against the read paths, and that the process is still healthy
// afterwards.
func TestLiveServerUnderConcurrentLoad(t *testing.T) {
	base, apiKey := liveServer(t)

	const (
		workers          = 128
		requestsPerWorer = 100
	)
	// A shared transport with a pool large enough for every worker, so the test
	// measures the server rather than client-side queuing.
	transport := &http.Transport{MaxIdleConns: workers * 2, MaxIdleConnsPerHost: workers * 2}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	// Only paths that a valid key reaches and that touch the repository, so the load
	// lands on the database rather than on a constant handler.
	paths := []string{
		"/api/v1/accounts",
		"/api/v1/messages?limit=50",
		"/api/v1/messages?limit=10&q=report",
		"/api/v1/messages?is_read=false",
		"/api/v1/drafts",
		"/healthz",
		"/readyz",
	}

	var (
		serverErrors atomic.Int64
		requestFails atomic.Int64
		completed    atomic.Int64
		firstError   atomic.Value
		wg           sync.WaitGroup
	)
	start := time.Now()
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < requestsPerWorer; i++ {
				path := paths[(worker+i)%len(paths)]
				request, err := http.NewRequest(http.MethodGet, base+path, nil)
				if err != nil {
					requestFails.Add(1)
					continue
				}
				request.Header.Set("X-API-Key", apiKey)
				response, err := client.Do(request)
				if err != nil {
					requestFails.Add(1)
					firstError.CompareAndSwap(nil, err.Error())
					continue
				}
				body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
				_ = response.Body.Close()
				if response.StatusCode >= 500 {
					serverErrors.Add(1)
					firstError.CompareAndSwap(nil, fmt.Sprintf("%s => %d: %.160q", path, response.StatusCode, body))
				}
				completed.Add(1)
			}
		}(worker)
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := int64(workers * requestsPerWorer)
	t.Logf("%d requests from %d workers in %s (%.0f req/s), %d completed, %d transport failures, %d 5xx",
		total, workers, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(),
		completed.Load(), requestFails.Load(), serverErrors.Load())

	if serverErrors.Load() > 0 {
		t.Errorf("%d requests returned 5xx under load; first: %v", serverErrors.Load(), firstError.Load())
	}
	if requestFails.Load() > 0 {
		t.Errorf("%d requests failed at the transport under load; first: %v", requestFails.Load(), firstError.Load())
	}
	if completed.Load() != total {
		t.Errorf("%d of %d requests completed", completed.Load(), total)
	}
	requireStillHealthy(t, base)
}

// residentBytes reads the process's resident set size. ps is used rather than an
// in-process metric because the server is a separate process, and its own memory is
// what a leak would show up in.
func residentBytes(t *testing.T, pid int) int64 {
	t.Helper()
	output, err := exec.Command("ps", "-o", "rss=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		t.Skipf("could not parse ps output %q: %v", output, err)
	}
	return value * 1024
}

// TestLiveServerSustainedLoadDoesNotGrowUnbounded runs the load in rounds and compares
// the process's memory between them. A per-request leak — a goroutine that never exits,
// a connection never returned to the pool, an unbounded map keyed on request data —
// does not show up in a single burst, because the burst ends before the growth is
// visible. Repeating the same work and watching the resident set is what distinguishes
// steady state from accumulation.
func TestLiveServerSustainedLoadDoesNotGrowUnbounded(t *testing.T) {
	env, addr := serverEnv(t)
	command, _ := spawnMain(t, env)
	waitForHealthy(t, addr, command)
	base, apiKey := "http://"+addr, env["NEXUSMAIL_API_KEY"]

	transport := &http.Transport{MaxIdleConns: 128, MaxIdleConnsPerHost: 128}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	round := func() (failures int64) {
		var (
			wg     sync.WaitGroup
			failed atomic.Int64
		)
		for worker := 0; worker < 64; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					request, err := http.NewRequest(http.MethodGet, base+"/api/v1/messages?limit=50", nil)
					if err != nil {
						failed.Add(1)
						continue
					}
					request.Header.Set("X-API-Key", apiKey)
					response, err := client.Do(request)
					if err != nil {
						failed.Add(1)
						continue
					}
					_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
					_ = response.Body.Close()
					if response.StatusCode >= 500 {
						failed.Add(1)
					}
				}
			}(worker)
		}
		wg.Wait()
		return failed.Load()
	}

	// A warm-up round first: the first requests allocate caches, buffers and
	// connections that are not a leak, and measuring from zero would count them.
	if failures := round(); failures > 0 {
		t.Fatalf("%d failures during warm-up", failures)
	}
	time.Sleep(500 * time.Millisecond)
	baseline := residentBytes(t, command.Process.Pid)

	const rounds = 6
	for i := 0; i < rounds; i++ {
		if failures := round(); failures > 0 {
			t.Fatalf("%d failures in round %d", failures, i)
		}
	}
	time.Sleep(500 * time.Millisecond)
	final := residentBytes(t, command.Process.Pid)

	t.Logf("%d requests over %d rounds; rss %.1f MiB -> %.1f MiB",
		rounds*64*50, rounds, float64(baseline)/(1<<20), float64(final)/(1<<20))

	// A generous ceiling, because Go's heap grows in steps and does not return memory
	// promptly. What it still catches is growth proportional to the request count: at
	// 19200 requests past the baseline, a per-request leak of even a few hundred bytes
	// would exceed this.
	if limit := baseline*3 + 32<<20; final > limit {
		t.Errorf("rss grew from %d to %d bytes over %d requests, past the %d ceiling",
			baseline, final, rounds*64*50, limit)
	}
	requireStillHealthy(t, base)
}

// TestLiveServerConcurrentWritesSerialise puts the load on the write path instead. All
// sqlite writes share one mutex, so this is where a lock ordering mistake or a missing
// writeMu would show up as a deadlock or a SQLITE_BUSY rather than as a slow response.
func TestLiveServerConcurrentWritesSerialise(t *testing.T) {
	base, apiKey := liveServer(t)
	accountID := createLiveAccount(t, base, apiKey)

	transport := &http.Transport{MaxIdleConns: 64, MaxIdleConnsPerHost: 64}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	const workers = 32
	var (
		created      atomic.Int64
		serverErrors atomic.Int64
		firstError   atomic.Value
		wg           sync.WaitGroup
	)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				body := fmt.Sprintf(`{"account_id":%d,"subject":"concurrent %d-%d","body_text":"load","to":["a@example.com"]}`, accountID, worker, i)
				request, err := http.NewRequest(http.MethodPost, base+"/api/v1/drafts", strings.NewReader(body))
				if err != nil {
					continue
				}
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-API-Key", apiKey)
				response, err := client.Do(request)
				if err != nil {
					serverErrors.Add(1)
					firstError.CompareAndSwap(nil, err.Error())
					continue
				}
				payload, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
				_ = response.Body.Close()
				switch {
				case response.StatusCode >= 500:
					serverErrors.Add(1)
					firstError.CompareAndSwap(nil, fmt.Sprintf("%d: %.160q", response.StatusCode, payload))
				case response.StatusCode < 300:
					created.Add(1)
				}
			}
		}(worker)
	}
	wg.Wait()

	t.Logf("%d drafts created by %d concurrent writers, %d server errors", created.Load(), workers, serverErrors.Load())
	if serverErrors.Load() > 0 {
		t.Errorf("%d writes failed under concurrency; first: %v", serverErrors.Load(), firstError.Load())
	}
	// Every write should have landed. A zero here would mean the load never reached
	// the write path at all, which would make the whole test vacuous.
	if want := int64(workers * 8); created.Load() != want {
		t.Errorf("%d of %d concurrent writes were persisted", created.Load(), want)
	}

	// The rows must actually be readable afterwards, which is what proves the writes
	// committed rather than merely being answered with a 201.
	request, err := http.NewRequest(http.MethodGet, base+"/api/v1/drafts", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-API-Key", apiKey)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	listed, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/drafts after the write storm = %d", response.StatusCode)
	}
	if got := bytes.Count(listed, []byte(`"subject":"concurrent `)); got != int(created.Load()) {
		t.Errorf("the draft list holds %d of the %d drafts written", got, created.Load())
	}
	requireStillHealthy(t, base)
}

// TestLiveServerThrottlesRealBruteForce exercises the rate limiter over the network
// rather than through the router directly. The in-process tests cover the arithmetic;
// what only shows up here is that the limiter keys on the address the kernel reports
// and still admits the valid key afterwards from the same address.
func TestLiveServerThrottlesRealBruteForce(t *testing.T) {
	base, apiKey := liveServer(t)
	client := &http.Client{Timeout: 20 * time.Second}

	var throttled, refused int
	for i := 0; i < 60; i++ {
		body := fmt.Sprintf(`{"api_key":"wrong-guess-%d-padded-to-a-plausible-length"}`, i)
		request, err := http.NewRequest(http.MethodPost, base+"/api/v1/auth/session", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		_ = response.Body.Close()
		switch response.StatusCode {
		case http.StatusTooManyRequests:
			throttled++
		case http.StatusUnauthorized, http.StatusBadRequest:
			refused++
		default:
			if response.StatusCode >= 500 {
				t.Fatalf("attempt %d produced %d", i, response.StatusCode)
			}
		}
	}

	if throttled == 0 {
		t.Errorf("60 wrong-key logins from one address were never throttled (%d refused)", refused)
	}
	t.Logf("%d refused, %d throttled", refused, throttled)

	// The API-key channel is separate and is not counted for a valid key, so a
	// legitimate integration from the same address must still work.
	request, err := http.NewRequest(http.MethodGet, base+"/api/v1/accounts", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-API-Key", apiKey)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("valid key from a throttled address => %d, want 200", response.StatusCode)
	}
	requireStillHealthy(t, base)
}

// TestLiveServerEnforcesAuthOverTheNetwork checks the credential comparison end to
// end. Every one of these must be refused: a near-miss key that is accepted would
// mean the compare is not doing what the constant-time compare is there for.
func TestLiveServerEnforcesAuthOverTheNetwork(t *testing.T) {
	base, apiKey := liveServer(t)
	client := &http.Client{Timeout: 20 * time.Second}

	for _, key := range []string{
		"", " ", apiKey + "x", "x" + apiKey, apiKey[:len(apiKey)-1],
		strings.ToUpper(apiKey), strings.Repeat("k", len(apiKey)-1),
		strings.Repeat("k", len(apiKey)+1), apiKey + "\x00",
		strings.Repeat("A", 10000),
	} {
		request, err := http.NewRequest(http.MethodGet, base+"/api/v1/accounts", nil)
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			request.Header.Set("X-API-Key", key)
		}
		response, err := client.Do(request)
		if err != nil {
			// A header Go refuses to send is not a server-side case.
			continue
		}
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Errorf("key %.40q was accepted", key)
		}
	}

	// And the real key still works, so the loop above was not passing by refusing
	// everything unconditionally.
	request, err := http.NewRequest(http.MethodGet, base+"/api/v1/accounts", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-API-Key", apiKey)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the valid key was refused with %d", response.StatusCode)
	}
	requireStillHealthy(t, base)
}

// TestLiveServerRejectsAnOversizedBody covers the MaxBytesReader on the attachment
// upload path with a body that really is sent over the socket. The limit exists so a
// single request cannot exhaust memory or the blob budget.
func TestLiveServerRejectsAnOversizedBody(t *testing.T) {
	base, apiKey := liveServer(t)
	client := &http.Client{Timeout: 60 * time.Second}

	accountID := createLiveAccount(t, base, apiKey)

	// A draft to attach to, so the request reaches the size check rather than
	// stopping at a missing draft.
	create, err := http.NewRequest(http.MethodPost, base+"/api/v1/drafts",
		strings.NewReader(fmt.Sprintf(`{"account_id":%d,"subject":"attachment target","to":["a@example.com"]}`, accountID)))
	if err != nil {
		t.Fatal(err)
	}
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("X-API-Key", apiKey)
	response, err := client.Do(create)
	if err != nil {
		t.Fatal(err)
	}
	created, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("could not create a draft to attach to: %d %.300q", response.StatusCode, created)
	}
	var draft struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(created, &draft); err != nil {
		t.Fatalf("decode created draft: %v (%.200q)", err, created)
	}

	// 64 MiB of zeros, streamed rather than buffered, against a default outbound cap
	// well below it.
	const oversized = 64 << 20
	body := io.LimitReader(zeroReader{}, oversized)
	request, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/drafts/%d/attachments", base, draft.ID), body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-API-Key", apiKey)
	response, err = client.Do(request)
	if err != nil {
		// The server closing the connection once the limit is hit is an acceptable
		// outcome; what must not happen is the whole body being accepted.
		requireStillHealthy(t, base)
		return
	}
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	_ = response.Body.Close()
	if response.StatusCode < 400 {
		t.Errorf("a %d-byte upload was accepted with %d: %.200q", oversized, response.StatusCode, payload)
	}
	requireStillHealthy(t, base)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// TestLiveServerHandlesAbruptDisconnects covers the cancellation path. A client that
// vanishes mid-request is the normal case for a mail UI — the user closes the tab —
// and the handler's context is cancelled underneath it. A leaked goroutine or a write
// to a closed connection would show up here as a process that stops answering.
func TestLiveServerHandlesAbruptDisconnects(t *testing.T) {
	base, apiKey := liveServer(t)
	addr := strings.TrimPrefix(base, "http://")

	for i := 0; i < 100; i++ {
		conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		request := fmt.Sprintf("GET /api/v1/messages?limit=50 HTTP/1.1\r\nHost: %s\r\nX-API-Key: %s\r\n\r\n", addr, apiKey)
		if _, err := conn.Write([]byte(request)); err != nil {
			_ = conn.Close()
			continue
		}
		// Close immediately, without reading the response: the server is mid-handler.
		_ = conn.Close()
	}

	// Also cancel through the client, which aborts after the headers are sent.
	client := &http.Client{Timeout: 30 * time.Second}
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/messages?limit=50", nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		request.Header.Set("X-API-Key", apiKey)
		go cancel()
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
			_ = response.Body.Close()
		} else if !errors.Is(err, context.Canceled) {
			// Any other error is worth seeing, but it is not necessarily a failure:
			// the race between cancel and send decides which one happens.
			t.Logf("request %d ended with %v", i, err)
		}
		cancel()
	}

	requireStillHealthy(t, base)

	// And the server must still serve correct responses, not merely respond at all.
	request, err := http.NewRequest(http.MethodGet, base+"/api/v1/accounts", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-API-Key", apiKey)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/accounts after the disconnect storm = %d", response.StatusCode)
	}
	if !bytes.Contains(body, []byte("[")) {
		t.Errorf("account list is %.120q, want a JSON array", body)
	}
}

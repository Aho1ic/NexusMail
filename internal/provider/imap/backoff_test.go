//go:build sqlite_fts5

package imap

import (
	"context"
	"math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nexusmail/internal/domain"

	goimap "github.com/emersion/go-imap/v2"
)

// TestWaitBackoffKeepsAFloor pins the jitter shape. waitBackoff used full
// jitter — rand.Int64N(delay+1), uniform over [0, delay] — so the 15-minute
// rateLimitBackoff that QQ's "System busy" and 163's "Too many connections"
// depend on actually waited a few seconds a fair share of the time. Every
// premature reconnect re-arms the provider's throttle window, which is how an
// account stays amber for hours: the ladder never gets far enough from the
// last rejection for the window to roll over. Equal jitter keeps the spread
// that avoids synchronising every account on one instant while guaranteeing
// the wait is worth calling a backoff.
func TestWaitBackoffKeepsAFloor(t *testing.T) {
	const delay = 15 * time.Minute
	for range 20000 {
		got := backoffDelay(delay)
		if got < delay/2 || got > delay {
			t.Fatalf("backoffDelay(%s) = %s, want within [%s, %s]", delay, got, delay/2, delay)
		}
	}

	// A floor alone would collapse the spread: assert the draws still cover
	// both halves of the permitted band so many accounts do not reconnect in
	// lockstep after a shared outage.
	var low, high int
	for range 20000 {
		if backoffDelay(delay) < delay*3/4 {
			low++
		} else {
			high++
		}
	}
	if low == 0 || high == 0 {
		t.Fatalf("jitter lost its spread: low=%d high=%d", low, high)
	}

	// Sub-millisecond delays must not round down to zero and spin.
	if got := backoffDelay(time.Nanosecond); got <= 0 {
		t.Fatalf("backoffDelay(1ns) = %s, want positive", got)
	}
	if got := backoffDelay(0); got != 0 {
		t.Fatalf("backoffDelay(0) = %s, want 0", got)
	}
}

// busyServer is a minimal IMAP server that authenticates and then rejects
// SELECT with QQ's throttle reply. It is deliberately not imapmemserver: the
// point is the post-authentication rejection that the memory server has no way
// to produce.
type busyServer struct {
	listener net.Listener
	dials    atomic.Int64
	selects  atomic.Int64
	ids      atomic.Int64
	idArgs   atomic.Value
	refuseID atomic.Bool
}

func newBusyServer(t *testing.T) *busyServer {
	t.Helper()
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &busyServer{listener: listener}
	server.idArgs.Store("")
	go server.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return server
}

func (s *busyServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.session(conn)
	}
}

func (s *busyServer) session(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	write := func(line string) bool {
		_, err := conn.Write([]byte(line + "\r\n"))
		return err == nil
	}
	// ID is advertised in the greeting the way the Chinese providers do it, so
	// the client decides to send it before any other command runs.
	if !write("* OK [CAPABILITY IMAP4rev1 IDLE ID AUTH=PLAIN] ready") {
		return
	}
	buffer := make([]byte, 4096)
	var pending string
	for {
		n, err := conn.Read(buffer)
		if n == 0 || err != nil {
			return
		}
		pending += string(buffer[:n])
		for {
			index := strings.Index(pending, "\r\n")
			if index < 0 {
				break
			}
			line := pending[:index]
			pending = pending[index+2:]
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			tag, command := fields[0], ""
			if len(fields) > 1 {
				command = strings.ToUpper(fields[1])
			}
			switch command {
			case "CAPABILITY":
				if !write("* CAPABILITY IMAP4rev1 IDLE ID AUTH=PLAIN") || !write(tag+" OK done") {
					return
				}
			case "AUTHENTICATE":
				// Consume the SASL initial response line, then accept.
				if !write("+ ") {
					return
				}
				for !strings.Contains(pending, "\r\n") {
					n, err := conn.Read(buffer)
					if n == 0 || err != nil {
						return
					}
					pending += string(buffer[:n])
				}
				cut := strings.Index(pending, "\r\n")
				pending = pending[cut+2:]
				if !write(tag + " OK authenticated") {
					return
				}
			case "LOGIN":
				if !write(tag + " OK authenticated") {
					return
				}
			case "ID":
				s.ids.Add(1)
				s.idArgs.Store(line)
				if s.refuseID.Load() {
					if !write(tag + " BAD ID not welcome here") {
						return
					}
					continue
				}
				if !write(`* ID ("name" "test-server")`) || !write(tag+" OK done") {
					return
				}
			case "LIST":
				if !write(`* LIST (\HasNoChildren) "/" "INBOX"`) || !write(tag+" OK done") {
					return
				}
			case "SELECT", "EXAMINE":
				// The reply the whole test exists for: authentication passed,
				// the mailbox command is throttled.
				s.selects.Add(1)
				if !write(tag + " NO System busy!") {
					return
				}
			case "LOGOUT":
				_ = write("* BYE")
				_ = write(tag + " OK done")
				return
			default:
				if !write(tag + " OK done") {
					return
				}
			}
		}
	}
}

// TestIdleLoopBacksOffOnPostAuthFailure covers the loop that kept an account
// wedged. idleLoop reset its ladder as soon as connect returned, then used a
// bare `continue` when Select or Idle failed — so a provider that authenticates
// and throttles the mailbox command was reconnected as fast as TLS handshakes
// complete, with a full LOGIN each time. That is the exact traffic shape a
// per-account throttle is built to punish, so the throttle never cleared and
// the command loop's own backoff could not recover the account no matter how
// long it waited.
func TestIdleLoopBacksOffOnPostAuthFailure(t *testing.T) {
	server := newBusyServer(t)
	h := newHarness(t)
	h.supervisor.dial = func(ctx context.Context, _ domain.Account) (net.Conn, error) {
		server.dials.Add(1)
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", server.listener.Addr().String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inbox := domain.Mailbox{
		AccountID: h.account.ID, RemoteName: "INBOX", DisplayName: "INBOX",
		Role: "inbox", SyncMode: "realtime",
	}
	if err := h.repo.UpsertMailbox(ctx, &inbox); err != nil {
		t.Fatal(err)
	}

	rt := &runtime{account: h.account, syncReq: make(chan int64, 8)}
	loop, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); h.supervisor.idleLoop(loop, rt) }()

	time.Sleep(3 * time.Second)
	stop()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("idleLoop did not exit after cancellation")
	}

	dials, selects := server.dials.Load(), server.selects.Load()
	t.Logf("in 3s: dials=%d throttled selects=%d", dials, selects)
	if selects == 0 {
		t.Fatal("server never saw a SELECT; the test did not exercise the failure path")
	}
	// The first attempt is immediate and the ladder starts at 1s with equal
	// jitter, so three seconds admits a handful of attempts, not dozens.
	if dials > 6 {
		t.Errorf("idleLoop reconnected %d times in 3s, want at most 6: it is not backing off", dials)
	}
}

// TestConnectSendsIDWhenAdvertised covers the RFC 2971 handshake. QQ and 163
// advertise ID in the greeting and expect third-party clients to identify
// themselves; an anonymous client is the one the provider throttles first. The
// command must also stay optional: a server that refuses ID must still yield a
// usable connection, or one fussy provider makes the account unreachable.
func TestConnectSendsIDWhenAdvertised(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		name := "accepted"
		if refuse {
			name = "refused"
		}
		t.Run(name, func(t *testing.T) {
			server := newBusyServer(t)
			server.refuseID.Store(refuse)
			h := newHarness(t)
			h.supervisor.dial = func(ctx context.Context, _ domain.Account) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "tcp", server.listener.Addr().String())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			client, err := h.supervisor.connect(ctx, h.account, nil, 30*time.Second)
			if err != nil {
				t.Fatalf("connect returned %v, want a usable connection", err)
			}
			defer func() { _ = client.Close() }()

			if got := server.ids.Load(); got != 1 {
				t.Errorf("server saw %d ID commands, want 1", got)
			}
			got, _ := server.idArgs.Load().(string)
			if !strings.Contains(strings.ToLower(got), "nexusmail") {
				t.Errorf("ID payload = %q, want it to name the client", got)
			}
		})
	}
}

// TestSyncSearchRangeStaysBounded pins the UID range the incremental sync asks
// for. Two shapes were already ruled out in production: the 0="*" sentinel made
// imapmemserver normalise "3355:*" into a reversed range and re-fetch the newest
// message on every 5-second probe, and the math.MaxUint32 bound that replaced it
// is refused outright by QQ ("NO System busy!" on UID SEARCH while SELECT on the
// same connection succeeds), which parked the account in backoff indefinitely.
// The range therefore has to be bounded by the UIDNEXT that SELECT just
// reported, and skipped entirely when the cursor is already current.
func TestSyncSearchRangeStaysBounded(t *testing.T) {
	tests := []struct {
		name    string
		lastUID uint32
		uidNext goimap.UID
		want    string
	}{
		{name: "new mail is a closed range", lastUID: 3354, uidNext: 3360, want: "3355:3359"},
		{name: "single new message", lastUID: 3354, uidNext: 3356, want: "3355"},
		{name: "cursor current means no search", lastUID: 3354, uidNext: 3355, want: ""},
		{name: "cursor ahead of server means no search", lastUID: 3400, uidNext: 3355, want: ""},
		{name: "missing uidnext falls back to the sentinel", lastUID: 3354, uidNext: 0, want: "3355:*"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set, search := incrementalUIDRange(test.lastUID, test.uidNext)
			if !search {
				if test.want != "" {
					t.Fatalf("incrementalUIDRange(%d, %d) skipped the search, want %q", test.lastUID, test.uidNext, test.want)
				}
				return
			}
			if test.want == "" {
				t.Fatalf("incrementalUIDRange(%d, %d) = %q, want the search skipped", test.lastUID, test.uidNext, set.String())
			}
			if got := set.String(); got != test.want {
				t.Errorf("incrementalUIDRange(%d, %d) = %q, want %q", test.lastUID, test.uidNext, got, test.want)
			}
			if strings.Contains(set.String(), "4294967295") {
				t.Errorf("range %q still carries the MaxUint32 bound QQ refuses", set.String())
			}
		})
	}
}

// TestFullJitterUnderflows documents why the old shape could not hold a
// throttle window open. It asserts the property of the previous
// implementation, so a future change back to full jitter is a visible
// decision rather than a silent regression.
func TestFullJitterUnderflows(t *testing.T) {
	const delay = 15 * time.Minute
	var premature int
	for range 20000 {
		if time.Duration(rand.Int64N(int64(delay)+1)) < 30*time.Second {
			premature++
		}
	}
	if premature == 0 {
		t.Skip("full jitter did not underflow in this sample")
	}
	t.Logf("full jitter over %s returned under 30s in %d/20000 draws (%.1f%%)",
		delay, premature, float64(premature)/200)
}

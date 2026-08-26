//go:build sqlite_fts5

package imap

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/platform/cryptobox"
	"nexusmail/internal/ports"
	"nexusmail/internal/repository/sqlite"
	accountservice "nexusmail/internal/service/account"
	"nexusmail/internal/storage"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

type recorder struct {
	mu     sync.Mutex
	events []ports.Event
	signal chan ports.Event
}

func newRecorder() *recorder { return &recorder{signal: make(chan ports.Event, 256)} }

func (r *recorder) Publish(event ports.Event) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	select {
	case r.signal <- event:
	default:
	}
}

func (r *recorder) await(t *testing.T, kind string, timeout time.Duration) (ports.Event, time.Duration) {
	t.Helper()
	start := time.Now()
	deadline := time.After(timeout)
	for {
		select {
		case event := <-r.signal:
			if event.Type == kind {
				return event, time.Since(start)
			}
		case <-deadline:
			t.Fatalf("timed out after %s waiting for %s", timeout, kind)
		}
	}
}

// count returns the number of events of the given kind published so far.
// Tests that need a stable before/after snapshot use this to assert "no new
// event happened" without draining the signal channel.
func (r *recorder) count(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, event := range r.events {
		if event.Type == kind {
			n++
		}
	}
	return n
}

type literal struct{ *strings.Reader }

func (l literal) Size() int64 { return l.Reader.Size() }

// searchCount tracks UID SEARCH commands sent by the supervisor so tests can
// assert the polling safety net does not hammer the provider.
var searchCount atomic.Int64

// loginCount tracks authentications, which is how tests observe a connection
// being rebuilt: a refresh or a recovery is only real if it logs in again.
var loginCount atomic.Int64

type countingConn struct{ net.Conn }

func (c countingConn) Write(payload []byte) (int, error) {
	upper := bytes.ToUpper(payload)
	if bytes.Contains(upper, []byte("UID SEARCH")) {
		searchCount.Add(1)
	}
	if bytes.Contains(upper, []byte("LOGIN ")) || bytes.Contains(upper, []byte("AUTHENTICATE ")) {
		loginCount.Add(1)
	}
	return c.Conn.Write(payload)
}

func rawMessage(subject string) string {
	return "MIME-Version: 1.0\r\nMessage-Id: <" + subject + "@example.com>\r\n" +
		"From: Sender <sender@example.com>\r\nTo: mail@example.com\r\n" +
		"Subject: " + subject + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody of " + subject + "\r\n"
}

type harness struct {
	supervisor *Supervisor
	user       *imapmemserver.User
	events     *recorder
	repo       *sqlite.Store
	account    domain.Account
	accounts   *accountservice.Service
}

func (h *harness) deliver(t *testing.T, subject string) {
	t.Helper()
	raw := rawMessage(subject)
	if _, err := h.user.Append("INBOX", literal{strings.NewReader(raw)}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatalf("append %s: %v", subject, err)
	}
}

// harnessOption tunes the fake provider a harness stands up. It exists so a test
// can model a provider that lacks an extension: QQ advertises neither MOVE nor
// UIDPLUS on some connections, and the code paths that only run without them are
// otherwise unreachable in tests.
type harnessOption func(*harnessConfig)

type harnessConfig struct{ caps goimap.CapSet }

// withoutMoveAndUIDPlus advertises bare IMAP4rev1. IMAP4rev2 implies both MOVE and
// UIDPLUS, so dropping it is what forces the COPY + \Deleted + EXPUNGE path.
func withoutMoveAndUIDPlus() harnessOption {
	return func(cfg *harnessConfig) { cfg.caps = goimap.CapSet{goimap.CapIMAP4rev1: {}} }
}

func newHarness(t *testing.T, options ...harnessOption) *harness {
	t.Helper()
	const username, password = "mail@example.com", "test-password"

	cfg := harnessConfig{caps: goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapIMAP4rev2: {}}}
	for _, option := range options {
		option(&cfg)
	}

	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(username, password)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	memServer.AddUser(user)
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         cfg.caps,
	})
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	blobs, err := storage.New(filepath.Join(dir, "blobs"), 1<<28, repo)
	if err != nil {
		t.Fatal(err)
	}
	box, err := cryptobox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	accounts := accountservice.New(repo, box)
	account, err := accounts.AddPassword(context.Background(), "qq", username, "Test", username, password)
	if err != nil {
		t.Fatal(err)
	}

	events := newRecorder()
	supervisor := NewSupervisor(repo, blobs, accounts, nil, events)
	supervisor.dial = func(ctx context.Context, _ domain.Account) (net.Conn, error) {
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", listener.Addr().String())
		if err != nil {
			return nil, err
		}
		return countingConn{conn}, nil
	}
	return &harness{supervisor: supervisor, user: user, events: events, repo: repo, account: account, accounts: accounts}
}

// TestNewMailLatency measures the delay between a message landing on the IMAP
// server and NEW_EMAIL reaching the realtime hub.
func TestNewMailLatency(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()

	waitConnected(t, h)

	for round := range 3 {
		subject := fmt.Sprintf("round-%d", round)
		start := time.Now()
		h.deliver(t, subject)
		_, _ = h.events.await(t, "NEW_EMAIL", 90*time.Second)
		elapsed := time.Since(start)
		t.Logf("round %d: NEW_EMAIL after %s", round, elapsed)
		if elapsed > 3*time.Second {
			t.Errorf("round %d: NEW_EMAIL took %s, want under 3s", round, elapsed)
		}
	}
}

// TestNewMailLatencyWithoutIdleNotifications covers providers that advertise
// IDLE but never deliver EXISTS. The polling safety net must still surface new
// mail within seconds instead of waiting for the periodic full sync.
func TestNewMailLatencyWithoutIdleNotifications(t *testing.T) {
	h := newHarness(t)
	h.supervisor.dropIdleNotifications = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()

	waitConnected(t, h)

	start := time.Now()
	h.deliver(t, "dropped-idle")
	_, _ = h.events.await(t, "NEW_EMAIL", 90*time.Second)
	elapsed := time.Since(start)
	t.Logf("NEW_EMAIL after %s with IDLE notifications dropped", elapsed)
	if elapsed > 2*realtimePollInterval {
		t.Errorf("NEW_EMAIL took %s, want under %s", elapsed, 2*realtimePollInterval)
	}
}

// TestBodyPrefetchYieldsToNewMail ensures a backlog of body prefetch work
// cannot delay discovery of new mail on the shared command connection.
func TestBodyPrefetchYieldsToNewMail(t *testing.T) {
	h := newHarness(t)
	for index := range 40 {
		h.deliver(t, fmt.Sprintf("backlog-%d", index))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()

	waitConnected(t, h)
	drain(h)

	start := time.Now()
	h.deliver(t, "urgent")
	_, _ = h.events.await(t, "NEW_EMAIL", 90*time.Second)
	elapsed := time.Since(start)
	t.Logf("NEW_EMAIL after %s while body prefetch was running", elapsed)
	if elapsed > 3*time.Second {
		t.Errorf("NEW_EMAIL took %s under prefetch load, want under 3s", elapsed)
	}
}

// TestIdleInboxProbeStaysCheap keeps the safety net from turning into a busy
// loop against the provider: with no new mail it must resolve via STATUS alone
// and never re-sync the mailbox.
func TestIdleInboxProbeStaysCheap(t *testing.T) {
	h := newHarness(t)
	h.supervisor.dropIdleNotifications = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()
	waitConnected(t, h)

	inbox, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	if inbox.UIDNext == nil {
		t.Fatal("inbox cursor missing UIDNEXT, probe cannot short-circuit")
	}
	before := searchCount.Load()
	time.Sleep(3 * realtimePollInterval)
	if searches := searchCount.Load() - before; searches != 0 {
		t.Errorf("idle probe issued %d UID searches over %s, want 0", searches, 3*realtimePollInterval)
	}

	// A real arrival must still be picked up by the same probe.
	h.deliver(t, "after-quiet-probes")
	_, _ = h.events.await(t, "NEW_EMAIL", 90*time.Second)
}

func drain(h *harness) {
	for {
		select {
		case <-h.events.signal:
		default:
			return
		}
	}
}

func waitConnected(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		account, err := h.repo.GetAccount(context.Background(), h.account.ID)
		if err == nil && account.Status == "connected" {
			time.Sleep(500 * time.Millisecond) // let the IDLE connection settle
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("account never reached connected status")
}

// connect opens a second client against the same server, standing in for another
// mail client the user has open on the same account.
func (h *harness) connect(t *testing.T, ctx context.Context) *imapclient.Client {
	t.Helper()
	conn, err := h.supervisor.dial(ctx, h.account)
	if err != nil {
		t.Fatal(err)
	}
	client := imapclient.New(conn, nil)
	if err := client.Login(h.account.Username, "test-password").Wait(); err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	return client
}

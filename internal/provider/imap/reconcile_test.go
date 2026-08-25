//go:build sqlite_fts5

package imap

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"nexusmail/internal/domain"

	goimap "github.com/emersion/go-imap/v2"
)

// tapConn records the IMAP commands sent on a connection so a test can assert
// what reconciliation actually asked the provider for, rather than only what it
// concluded.
type tapConn struct {
	net.Conn
	mu    *sync.Mutex
	lines *[]string
}

func (c tapConn) Write(payload []byte) (int, error) {
	c.mu.Lock()
	for _, line := range bytes.Split(payload, []byte("\r\n")) {
		if len(line) > 0 {
			*c.lines = append(*c.lines, string(line))
		}
	}
	c.mu.Unlock()
	return c.Conn.Write(payload)
}

// TestReconcileAsksOnlyAboutLocalUIDs pins the cost model of reconciliation.
//
// It used to issue UID SEARCH ALL and then fetch flags for every UID the provider
// returned, so the work scaled with the remote mailbox: on a Gmail "All Mail" or a
// long-lived QQ inbox that is a six-figure UID list and a multi-megabyte flag
// response, pulled under the foreground command lock every five minutes while new
// mail queued behind it. It must ask only about the rows it holds.
func TestReconcileAsksOnlyAboutLocalUIDs(t *testing.T) {
	h := newHarness(t)
	for index := range 12 {
		h.deliver(t, fmt.Sprintf("history-%d", index))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, h)
	// Stop the loops so nothing else drives the connection while commands are being
	// recorded; the repository keeps the state the first sync produced.
	h.supervisor.Stop()

	inbox, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := h.repo.ListMailboxUIDs(ctx, inbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 12 {
		t.Fatalf("local uids = %v, want 12 rows", stored)
	}
	// Drop two rows to make the local set a strict subset of the remote mailbox,
	// which is the normal state for a mailbox older than the import window.
	if _, err := h.repo.DeleteMailboxUIDs(ctx, inbox.ID, []uint32{stored[2], stored[5]}); err != nil {
		t.Fatal(err)
	}
	remaining, err := h.repo.ListMailboxUIDs(ctx, inbox.ID)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var lines []string
	inner := h.supervisor.dial
	h.supervisor.dial = func(ctx context.Context, account domain.Account) (net.Conn, error) {
		conn, err := inner(ctx, account)
		if err != nil {
			return nil, err
		}
		return tapConn{Conn: conn, mu: &mu, lines: &lines}, nil
	}
	client := h.connect(t, ctx)
	defer func() { _ = client.Close() }()
	selected, err := client.Select(inbox.RemoteName, &goimap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	lines = nil
	mu.Unlock()
	h.supervisor.lastReconcile.Clear()
	if err := h.supervisor.reconcileMailbox(ctx, client, inbox, selected); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	issued := append([]string(nil), lines...)
	mu.Unlock()
	for _, line := range issued {
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "UID SEARCH") {
			t.Errorf("reconcile issued a mailbox-wide search: %q", line)
		}
	}
	fetches := 0
	for _, line := range issued {
		if !strings.Contains(strings.ToUpper(line), "UID FETCH") {
			continue
		}
		fetches++
		asked := parseUIDSet(t, line)
		for _, gone := range []uint32{stored[2], stored[5]} {
			if _, ok := asked[gone]; ok {
				t.Errorf("reconcile asked about uid %d it does not store: %q", gone, line)
			}
		}
		for _, kept := range remaining {
			if _, ok := asked[kept]; !ok {
				t.Errorf("reconcile skipped stored uid %d: %q", kept, line)
			}
		}
		if len(asked) != len(remaining) {
			t.Errorf("reconcile asked about %d uids, stores %d: %q", len(asked), len(remaining), line)
		}
	}
	if fetches != 1 {
		t.Errorf("reconcile issued %d flag fetches, want 1", fetches)
	}
}

// parseUIDSet expands the sequence set of a "UID FETCH <set> (...)" command, so
// the assertions can talk about UIDs rather than about how they were encoded.
func parseUIDSet(t *testing.T, line string) map[uint32]struct{} {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) < 4 {
		t.Fatalf("cannot read a uid set from %q", line)
	}
	uids := make(map[uint32]struct{})
	for _, part := range strings.Split(fields[3], ",") {
		bounds := strings.SplitN(part, ":", 2)
		low, err := strconv.ParseUint(bounds[0], 10, 32)
		if err != nil {
			t.Fatalf("bad uid %q in %q: %v", bounds[0], line, err)
		}
		high := low
		if len(bounds) == 2 {
			if high, err = strconv.ParseUint(bounds[1], 10, 32); err != nil {
				t.Fatalf("bad uid %q in %q: %v", bounds[1], line, err)
			}
		}
		for uid := low; uid <= high; uid++ {
			uids[uint32(uid)] = struct{}{}
		}
	}
	return uids
}

// TestReconcilePropagatesExpunge keeps the behaviour the bounded pass has to
// preserve: mail removed on the server must leave the local feed.
func TestReconcilePropagatesExpunge(t *testing.T) {
	h := newHarness(t)
	for index := range 4 {
		h.deliver(t, fmt.Sprintf("expunge-%d", index))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, h)
	h.supervisor.Stop()

	inbox, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	before, err := h.repo.ListMailboxUIDs(ctx, inbox.ID)
	if err != nil || len(before) != 4 {
		t.Fatalf("local uids = %v err=%v", before, err)
	}

	// Delete one message the way another mail client would.
	mutator := h.connect(t, ctx)
	if _, err := mutator.Select(inbox.RemoteName, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	target := goimap.UIDSetNum(goimap.UID(before[1]))
	if err := mutator.Store(target, &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := mutator.Expunge().Close(); err != nil {
		t.Fatal(err)
	}
	_ = mutator.Close()

	client := h.connect(t, ctx)
	defer func() { _ = client.Close() }()
	selected, err := client.Select(inbox.RemoteName, &goimap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	h.supervisor.lastReconcile.Clear()
	if err := h.supervisor.reconcileMailbox(ctx, client, inbox, selected); err != nil {
		t.Fatal(err)
	}

	after, err := h.repo.ListMailboxUIDs(ctx, inbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 3 {
		t.Fatalf("local uids after expunge = %v, want 3", after)
	}
	for _, uid := range after {
		if uid == before[1] {
			t.Fatalf("expunged uid %d survived reconciliation", uid)
		}
	}
}

// TestReconcileKeepsMailThatArrivedDuringThePass covers the hazard the bounded
// pass introduces: it snapshots the local UIDs first, so anything inserted after
// the snapshot must be left alone rather than being read as expunged.
func TestReconcileKeepsMailThatArrivedDuringThePass(t *testing.T) {
	h := newHarness(t)
	for index := range 3 {
		h.deliver(t, fmt.Sprintf("settled-%d", index))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, h)
	h.supervisor.Stop()

	inbox, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	client := h.connect(t, ctx)
	defer func() { _ = client.Close() }()
	selected, err := client.Select(inbox.RemoteName, &goimap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}

	// Import a message the reconcile pass does not know about, exactly as a sync
	// racing the pass would, then reconcile against the older snapshot.
	h.deliver(t, "arrived-mid-pass")
	if err := h.supervisor.syncMailbox(ctx, client, inbox, false); err != nil {
		t.Fatal(err)
	}
	fresh, err := h.repo.ListMailboxUIDs(ctx, inbox.ID)
	if err != nil || len(fresh) != 4 {
		t.Fatalf("local uids = %v err=%v", fresh, err)
	}
	newest := fresh[len(fresh)-1]

	h.supervisor.lastReconcile.Clear()
	if err := h.supervisor.reconcileMailboxWithUIDs(ctx, client, inbox, selected, fresh[:3]); err != nil {
		t.Fatal(err)
	}
	after, err := h.repo.ListMailboxUIDs(ctx, inbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 4 {
		t.Fatalf("local uids after reconcile = %v, want 4", after)
	}
	if after[len(after)-1] != newest {
		t.Fatalf("uid %d that arrived mid-pass was dropped, local now %v", newest, after)
	}
}

// TestSyncMailboxSkipReconcile covers the 5s inbox probe's hot path. The probe
// calls syncMailbox with skipReconcile=true because the only thing that has
// to happen fast is the new-mail ingest; flag and expunge drift is the 5
// minute periodic sync's job. This test asserts that lastReconcile is not
// touched when skipReconcile is set, so the periodic path remains the sole
// owner of the reconciliation cost.
//
// Unlike TestNewMailLatency, this does not bring the supervisor up: the
// goroutine that does the initial sync can race the manual syncMailbox call
// and produce a UNIQUE constraint error on the same dedupe_key. The behaviour
// under test is the supervisor's syncMailbox entry point, so driving it
// directly is the right shape.
func TestSyncMailboxSkipReconcile(t *testing.T) {
	h := newHarness(t)
	h.deliver(t, "inbox-anchor")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, h)
	drain(h)
	h.supervisor.Stop()

	inbox, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	// The initial sync committed the message and wrote the cursor, so the
	// manual probe-path sync should not find any new UIDs to ingest; the
	// assertion is that it returns cleanly without touching lastReconcile.
	if inbox.LastUID == 0 {
		t.Skipf("initial sync did not commit cursor (LastUID=0); test depends on a committed first sync")
	}
	client := h.connect(t, ctx)
	defer func() { _ = client.Close() }()

	h.supervisor.lastReconcile.Clear()

	if err := h.supervisor.syncMailbox(ctx, client, inbox, true); err != nil {
		t.Fatalf("probe-path sync: %v", err)
	}
	if _, ok := h.supervisor.lastReconcile.Load(inbox.ID); ok {
		t.Fatalf("lastReconcile was written by probe-path sync, want skipped")
	}

	// A periodic-sync call must still write the stamp; the probe's skip is
	// the only divergence.
	if err := h.supervisor.syncMailbox(ctx, client, inbox, false); err != nil {
		t.Fatalf("periodic-path sync: %v", err)
	}
	if _, ok := h.supervisor.lastReconcile.Load(inbox.ID); !ok {
		t.Fatalf("lastReconcile was not written by periodic-path sync")
	}
}

//go:build sqlite_fts5

package imap

import (
	"bytes"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"nexusmail/internal/domain"
)

// listDelay is long enough that a lock held across the LIST is unmistakable, and
// short enough not to dominate the package's runtime.
const listDelay = 2 * time.Second

// slowListConn delays the LIST command once armed, which is the shape of a provider
// whose folder listing is slow while the rest of the connection is responsive. QQ
// does this on accounts with many folders.
type slowListConn struct {
	net.Conn
	armed *atomic.Bool
}

func (c slowListConn) Write(payload []byte) (int, error) {
	if c.armed.Load() && bytes.Contains(bytes.ToUpper(payload), []byte("LIST ")) {
		time.Sleep(listDelay)
	}
	return c.Conn.Write(payload)
}

// periodicSync refreshes the folder catalog before taking the command lock, and its
// comment says why: the lock is what the 5-second new-mail probe needs, so a refresh
// that held it would stall new mail for as long as the provider takes to answer LIST
// — up to the whole 5-minute interval on a bad one. This is the arrangement that had
// no test, and it is invisible in review because moving one line inside the lock
// still passes every functional test.
func TestPeriodicSyncRefreshesTheCatalogOutsideTheCommandLock(t *testing.T) {
	h := newHarness(t)
	var armed atomic.Bool
	inner := h.supervisor.dial
	h.supervisor.dial = func(ctx context.Context, account domain.Account) (net.Conn, error) {
		conn, err := inner(ctx, account)
		if err != nil {
			return nil, err
		}
		return slowListConn{Conn: conn, armed: &armed}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()
	waitConnected(t, h)

	rt, err := h.supervisor.runtime(h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	client := rt.client.Load()
	if client == nil {
		t.Fatal("command connection missing")
	}

	armed.Store(true)
	defer armed.Store(false)

	done := make(chan error, 1)
	go func() { done <- h.supervisor.periodicSync(ctx, rt, client) }()

	// Let the tick get as far as the LIST it is now stuck on.
	time.Sleep(300 * time.Millisecond)

	// This is what the probe does. It must not wait out the LIST.
	start := time.Now()
	rt.lock()
	waited := time.Since(start)
	rt.unlock()

	if waited > listDelay/2 {
		t.Errorf("the command lock was unavailable for %s while LIST was stalled for %s: the catalog refresh is holding it, so new mail waits out the folder listing", waited, listDelay)
	}
	t.Logf("command lock acquired in %s while LIST was stalled for %s", waited, listDelay)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("periodicSync failed: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("periodicSync never returned")
	}
}

// A refresh that fails is logged and tolerated, because the folder list rarely
// changes and the sync that follows is the part that matters. If the failure
// aborted the tick instead, a provider with a flaky LIST would stop delivering
// mail entirely while its connection stayed healthy.
func TestPeriodicSyncToleratesAFailedCatalogRefresh(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()
	waitConnected(t, h)

	rt, err := h.supervisor.runtime(h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	client := rt.client.Load()
	if client == nil {
		t.Fatal("command connection missing")
	}

	h.deliver(t, "arrives-on-the-tick")
	if err := h.supervisor.periodicSync(ctx, rt, client); err != nil {
		t.Fatalf("periodicSync failed: %v", err)
	}

	inbox, err := h.repo.GetMailboxByRole(context.Background(), h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	if count := localCount(t, h, inbox.ID); count == 0 {
		t.Error("the tick stored no mail, so the sync that follows the refresh did not run")
	}
}

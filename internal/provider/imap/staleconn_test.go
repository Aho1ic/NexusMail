//go:build sqlite_fts5

package imap

import (
	"context"
	"testing"
	"time"
)

// TestCommandConnectionRefreshes covers the stale-connection stall. A long-lived
// command connection to QQ stopped reporting new mail through both STATUS and
// SELECT, so every detection path agreed there was nothing to fetch: measured on
// a live account as four messages spanning 41 minutes of arrivals stored in the
// same second, and a 42-minute delay that cleared 6 seconds after an unrelated
// reconnect. The loop must therefore replace the connection on its own schedule
// rather than trusting it until it visibly breaks.
func TestCommandConnectionRefreshes(t *testing.T) {
	h := newHarness(t)
	// Drive the rebuild on a test timescale. The production interval is minutes,
	// which no test can wait for.
	h.supervisor.commandRefresh = 2 * time.Second
	h.supervisor.dropIdleNotifications = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()
	waitConnected(t, h)

	before := loginCount.Load()
	time.Sleep(7 * time.Second)
	logins := loginCount.Load() - before
	t.Logf("logins in 7s with a %s refresh: %d", h.supervisor.commandRefresh, logins)
	if logins == 0 {
		t.Error("command connection never rebuilt; a stale connection would stall new mail indefinitely")
	}
	// Three refresh windows fit in the sleep. Allow slack for the IDLE loop and
	// scheduling, but catch a rebuild that has become a tight reconnect loop.
	if logins > 12 {
		t.Errorf("command connection rebuilt %d times in 7s, want a bounded number", logins)
	}

	// The refresh must not break the account: a rebuild is routine, not a fault.
	account, err := h.repo.GetAccount(ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status == "auth_error" || account.LastError != nil {
		t.Errorf("account status = %q, last_error = %v; a scheduled refresh must not surface as a fault",
			account.Status, account.LastError)
	}

	// And mail must still flow across the rebuild.
	drain(h)
	h.deliver(t, "after-refresh")
	_, elapsed := h.events.await(t, "NEW_EMAIL", 90*time.Second)
	t.Logf("NEW_EMAIL after refresh took %s", elapsed)
}

// TestProbeFailureForcesReconnect covers the other half of the stall. A probe is
// one STATUS on a connection that just authenticated, so repeated failures mean
// the connection is unusable — but the loop only logged that at Debug and waited
// for client.Closed(), which never fires on a socket the host's sleep or a NAT
// dropped without an RST. New mail then waited for the 5-minute periodic sync to
// discover the same thing.
func TestProbeFailureForcesReconnect(t *testing.T) {
	h := newHarness(t)
	h.supervisor.dropIdleNotifications = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()
	waitConnected(t, h)

	// Break the probe without touching the socket, so client.Closed() stays
	// silent and only the failure counter can recover the connection.
	before := loginCount.Load()
	h.supervisor.failProbe.Store(true)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if loginCount.Load() > before {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.supervisor.failProbe.Store(false)

	if loginCount.Load() == before {
		t.Fatalf("no reconnect after %s of failing probes: a dead-but-open connection stalls new mail", 30*time.Second)
	}
	t.Logf("reconnected after %d consecutive probe failures", maxProbeFailures)

	// Recovery has to be real, not just a reconnect: mail must flow again.
	waitConnected(t, h)
	drain(h)
	h.deliver(t, "after-probe-recovery")
	_, elapsed := h.events.await(t, "NEW_EMAIL", 90*time.Second)
	t.Logf("NEW_EMAIL after probe recovery took %s", elapsed)
}

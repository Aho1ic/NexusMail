//go:build sqlite_fts5

package imap

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
)

// commandDelay simulates the round-trip latency of a real provider. Every write
// is one IMAP command, so sleeping here turns the loopback memory server into
// something that behaves like imap.gmail.com over the public internet.
var commandDelay atomic.Int64

type slowConn struct{ net.Conn }

func (c slowConn) Write(payload []byte) (int, error) {
	if delay := commandDelay.Load(); delay > 0 {
		time.Sleep(time.Duration(delay))
	}
	return c.Conn.Write(payload)
}

// fillMailbox appends count messages to a mailbox, standing in for the years of
// history a real Gmail "All Mail" or QQ inbox holds.
func fillMailbox(t *testing.T, h *harness, name string, count int) {
	t.Helper()
	if err := h.user.Create(name, nil); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	for index := range count {
		raw := rawMessage(fmt.Sprintf("history-%s-%d", name, index))
		if _, err := h.user.Append(name, literal{strings.NewReader(raw)}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
			t.Fatalf("append history: %v", err)
		}
	}
}

// TestSyncAllHoldsCommandLock measures how long a periodic full sync keeps the
// command connection, and what a message arriving during that window pays.
func TestSyncAllHoldsCommandLock(t *testing.T) {
	h := newHarness(t)
	h.supervisor.dial = slowDialer(t, h)
	fillMailbox(t, h, "[Gmail]/All Mail", 1500)
	fillMailbox(t, h, "INBOX", 400)

	commandDelay.Store(int64(8 * time.Millisecond))
	t.Cleanup(func() { commandDelay.Store(0) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()

	firstSync := time.Now()
	waitConnected(t, h)
	t.Logf("initial syncAll (cold cache) took %s", time.Since(firstSync))

	rt, err := h.supervisor.runtime(h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	client := rt.client.Load()
	if client == nil {
		t.Fatal("command connection missing")
	}

	// Let the body prefetch backlog settle so it is not what we are measuring.
	time.Sleep(2 * time.Second)
	drain(h)
	for _, mailbox := range mailboxes(t, h) {
		t.Logf("mailbox %q role=%s mode=%s local=%d", mailbox.RemoteName, mailbox.Role, mailbox.SyncMode, localCount(t, h, mailbox.ID))
	}

	// A warm periodic sync with reconciliation due, which is what the 5 minute
	// ticker actually pays: nothing new to append, everything to verify.
	h.supervisor.lastReconcile.Clear()
	rt.lock()
	warm := time.Now()
	if err := h.supervisor.syncAllMailboxes(ctx, rt, client); err != nil {
		t.Fatalf("warm syncAllMailboxes: %v", err)
	}
	warmDuration := time.Since(warm)
	rt.unlock()
	t.Logf("warm syncAllMailboxes held the command lock for %s", warmDuration)

	// Now the number that matters: mail arriving while the ticker sync runs.
	h.supervisor.lastReconcile.Clear()
	drain(h)
	done := make(chan time.Duration, 1)
	go func() {
		rt.lock()
		start := time.Now()
		_ = h.supervisor.syncAllMailboxes(ctx, rt, client)
		rt.unlock()
		done <- time.Since(start)
	}()
	time.Sleep(50 * time.Millisecond) // let syncAllMailboxes take the lock first
	arrival := time.Now()
	h.deliver(t, "arrives-during-full-sync")
	_, _ = h.events.await(t, "NEW_EMAIL", 5*time.Minute)
	latency := time.Since(arrival)
	t.Logf("NEW_EMAIL after %s while a full sync held the connection (sync took %s)", latency, <-done)
	if latency > 5*time.Second {
		t.Errorf("new mail waited %s behind the periodic sync, want under 5s", latency)
	}
}

// TestNewMailUnderPrefetchBacklog measures new-mail latency while a realistic
// first-sync body backlog is draining. The prefetch shares the one command
// connection with sync, so this is the number a user sees on the day they add a
// Gmail or QQ account.
func TestNewMailUnderPrefetchBacklog(t *testing.T) {
	h := newHarness(t)
	h.supervisor.dial = slowDialer(t, h)
	fillMailbox(t, h, "INBOX", 1200)

	commandDelay.Store(int64(8 * time.Millisecond))
	t.Cleanup(func() { commandDelay.Store(0) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()
	waitConnected(t, h)

	// Measure while the backlog is still draining, not after it finished.
	worst := time.Duration(0)
	for round := range 5 {
		drain(h)
		arrival := time.Now()
		h.deliver(t, fmt.Sprintf("urgent-%d", round))
		_, _ = h.events.await(t, "NEW_EMAIL", 2*time.Minute)
		elapsed := time.Since(arrival)
		worst = max(worst, elapsed)
		t.Logf("round %d: NEW_EMAIL after %s (prefetch backlog draining)", round, elapsed)
	}
	t.Logf("worst new-mail latency under backlog: %s", worst)
	if worst > 5*time.Second {
		t.Errorf("worst new-mail latency %s under prefetch backlog, want under 5s", worst)
	}
}

// TestPeriodicSyncSkipsQuietAndEveryMessageMailboxes pins what the periodic tick
// is allowed to touch. Everything it does runs on the connection new mail needs,
// so a mailbox that holds a second copy of the whole account, or one where nothing
// moved, must not be walked.
func TestPeriodicSyncSkipsQuietAndEveryMessageMailboxes(t *testing.T) {
	h := newHarness(t)
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
	fillMailbox(t, h, "[Gmail]/All Mail", 30)
	fillMailbox(t, h, "Sent Messages", 5)
	fillMailbox(t, h, "INBOX", 5)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()
	waitConnected(t, h)

	// Gmail's every-message view must not be imported by the periodic tier at all:
	// importing it duplicated every message the inbox and sent folder already hold.
	for _, mailbox := range mailboxes(t, h) {
		if mailbox.RemoteName != "[Gmail]/All Mail" {
			continue
		}
		if mailbox.SyncMode != "lazy" {
			t.Errorf("All Mail sync mode = %q, want lazy", mailbox.SyncMode)
		}
		if count := localCount(t, h, mailbox.ID); count != 0 {
			t.Errorf("All Mail imported %d messages on the periodic tier", count)
		}
	}

	rt, err := h.supervisor.runtime(h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	client := rt.client.Load()
	if client == nil {
		t.Fatal("command connection missing")
	}

	// A second pass with nothing changed: sent has already been reconciled and its
	// UIDNEXT has not moved, so STATUS is enough and it must not be selected again.
	mu.Lock()
	lines = nil
	mu.Unlock()
	rt.lock()
	syncErr := h.supervisor.syncAllMailboxes(ctx, rt, client)
	rt.unlock()
	if syncErr != nil {
		t.Fatal(syncErr)
	}
	mu.Lock()
	issued := append([]string(nil), lines...)
	mu.Unlock()
	for _, line := range issued {
		// A read-only select is sent as EXAMINE, which is the command that costs the
		// mailbox walk that follows it.
		upper := strings.ToUpper(line)
		if !strings.Contains(upper, "EXAMINE ") && !strings.Contains(upper, "SELECT ") {
			continue
		}
		if strings.Contains(line, "All Mail") {
			t.Errorf("periodic sync selected Gmail's every-message view: %q", line)
		}
		if strings.Contains(line, "Sent Messages") {
			t.Errorf("periodic sync re-selected an unchanged mailbox: %q", line)
		}
	}
}

// TestBackgroundReconcileOutlastsTheSyncTick pins the relationship the STATUS
// skip depends on. If a background mailbox is reconciled as often as the periodic
// sync runs, reconciliation is due on every tick, so every mailbox is selected and
// walked every time and the skip can never fire.
func TestBackgroundReconcileOutlastsTheSyncTick(t *testing.T) {
	if reconcileIntervalFor("sent") <= periodicSyncInterval {
		t.Errorf("background reconcile interval %s does not outlast the %s sync tick",
			reconcileIntervalFor("sent"), periodicSyncInterval)
	}
	if reconcileIntervalFor("inbox") != reconcileInterval {
		t.Errorf("inbox reconcile interval = %s, want %s", reconcileIntervalFor("inbox"), reconcileInterval)
	}
}

func mailboxes(t *testing.T, h *harness) []domain.Mailbox {
	t.Helper()
	items, err := h.repo.ListMailboxes(context.Background(), h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func localCount(t *testing.T, h *harness, mailboxID int64) int {
	t.Helper()
	page, err := h.repo.ListMessages(context.Background(), ports.MessageFilter{MailboxID: &mailboxID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	total := len(page.Items)
	for page.NextCursor != "" {
		page, err = h.repo.ListMessages(context.Background(), ports.MessageFilter{MailboxID: &mailboxID, Limit: 100, Cursor: page.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
		total += len(page.Items)
	}
	return total
}

func slowDialer(t *testing.T, h *harness) func(context.Context, domain.Account) (net.Conn, error) {
	t.Helper()
	inner := h.supervisor.dial
	return func(ctx context.Context, account domain.Account) (net.Conn, error) {
		conn, err := inner(ctx, account)
		if err != nil {
			return nil, err
		}
		return slowConn{conn}, nil
	}
}

//go:build sqlite_fts5

package imap

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"nexusmail/internal/ports"
)

// New mail under a prefetch backlog is covered in bigmailbox_test.go by measuring
// latency. What is not covered is the invariant that produces that latency, and
// the other half of the contention: the user's own actions. Marking read,
// archiving and opening an unfetched message all take rt.lock(), and they run
// while the four body workers drain a first-sync backlog on the same connection.
//
// These tests assert on a count rather than on a duration. A timing threshold
// loose enough not to flake on a busy machine is also loose enough to pass with
// the preemption removed — measured: 36ms with it, 91ms without, against a budget
// that has to tolerate seconds. The count is exact: from the moment a foreground
// waiter exists, at most the one background fetch already holding the connection
// may finish before the foreground gets it. Anything more means the foreground
// queued behind the backlog, which is the failure CLAUDE.md warns about.

// bodyCounter counts fetched bodies straight from the database. ListMessages
// clamps to 100 rows and the backlog here is deliberately larger, and the
// connection is opened once: a fresh sql.Open per sample costs milliseconds, which
// is long enough to distort a measurement of a lock wait.
func bodyCounter(t *testing.T, h *harness) func() int {
	t.Helper()
	database, err := sql.Open("sqlite3", "file:"+h.dbPath+"?_busy_timeout=5000&mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	statement, err := database.Prepare(`SELECT COUNT(*) FROM messages WHERE body_state = 'ready'`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = statement.Close() })
	return func() int {
		var count int
		if err := statement.QueryRow().Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
}

// backloggedAccount returns a connected supervisor whose body workers have a real
// backlog to chew through, with every command paying a provider-like round trip so
// the lock is genuinely contended rather than held for microseconds.
func backloggedAccount(t *testing.T, size int) *harness {
	t.Helper()
	h := newHarness(t)
	h.supervisor.dial = slowDialer(t, h)
	fillMailbox(t, h, "INBOX", size)
	commandDelay.Store(int64(8 * time.Millisecond))
	t.Cleanup(func() { commandDelay.Store(0) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.supervisor.Stop)
	waitConnected(t, h)
	// Wait until the prefetch is actually running: a measurement taken before the
	// workers started would show no contention and prove nothing.
	ready := bodyCounter(t, h)
	waitFor(t, 120*time.Second, func() bool { return ready() >= 3 })
	return h
}

// TestForegroundNeverQueuesBehindPrefetch is the core invariant. While the body
// workers drain a backlog, a foreground acquisition must not wait for more than
// the one fetch already in flight.
//
// This covers the lock only. Raising urgent and taking cmdMu here is the one path
// into the command connection that does not pass the bodySlots semaphore, and a
// priority inversion lived in that gap unseen: a real foreground body fetch
// blocked on the semaphore before it could raise urgent, so it waited for a
// second background fetch that this test cannot observe. The semaphore layer is
// TestForegroundBodyFetchDoesNotQueueBehindPrefetch and
// TestPrefetchNeverHoldsAForegroundSlot in bodyarrival_test.go; a change to
// either layer needs both.
func TestForegroundNeverQueuesBehindPrefetch(t *testing.T) {
	h := backloggedAccount(t, 400)
	rt, err := h.supervisor.runtime(h.account.ID)
	if err != nil {
		t.Fatal(err)
	}

	ready := bodyCounter(t, h)
	worst := 0
	trials := 0
	for range 12 {
		// lock() is spelled out rather than called so the sample can be taken
		// inside the window being measured. urgent is raised first, which is what
		// makes the sample exact: from that instant no *new* background fetch may
		// acquire the connection, so however long the count takes, it cannot
		// attribute an unrelated fetch to this wait. Sampling before raising it
		// leaves a gap that a slow build fills with real fetches.
		rt.urgent.Add(1)
		before := ready()
		start := time.Now()
		rt.cmdMu.Lock()
		during := ready() - before
		waited := time.Since(start)
		rt.urgent.Add(-1)
		rt.cmdMu.Unlock()

		if during > worst {
			worst = during
		}
		trials++
		t.Logf("trial %d: %d background fetches completed while the foreground waited (%s)", trials, during, waited)
		if ready() >= 400 {
			break // the backlog drained; later trials would measure nothing
		}
		time.Sleep(40 * time.Millisecond)
	}
	// One in-flight fetch may finish; a second one means a queue formed.
	if worst > 1 {
		t.Errorf("a foreground acquisition waited for %d background fetches, want at most 1", worst)
	}
	if trials < 5 {
		t.Fatalf("only %d trials ran; the backlog drained too fast to measure", trials)
	}
}

// TestUserActionsCompleteUnderPrefetchBacklog covers each user-facing operation
// that takes the command connection. The assertion is that they succeed and stay
// interactive; the exact bound is in the test above.
func TestUserActionsCompleteUnderPrefetchBacklog(t *testing.T) {
	h := backloggedAccount(t, 200)
	ctx := context.Background()

	page, err := h.repo.ListMessages(ctx, ports.MessageFilter{AccountID: &h.account.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) < 12 {
		t.Fatalf("only %d messages synced, not enough to act on", len(page.Items))
	}

	actions := []struct {
		name string
		run  func() error
	}{
		{"set flags", func() error {
			read := true
			return h.supervisor.SetFlags(ctx, page.Items[0].ID, &read, nil)
		}},
		{"fetch body on demand", func() error {
			return h.supervisor.FetchBody(ctx, page.Items[1].ID)
		}},
		{"archive", func() error {
			return h.supervisor.Archive(ctx, page.Items[2].ID)
		}},
		{"bulk mark read", func() error {
			ids := make([]int64, 0, 8)
			for _, item := range page.Items[3:11] {
				ids = append(ids, item.ID)
			}
			_, err := h.supervisor.SetSeenBulk(ctx, ids)
			return err
		}},
		{"open a folder", func() error {
			return h.supervisor.RequestMailbox(ctx, inboxMailboxID(t, h))
		}},
	}
	// Generous, because this asserts "not stuck", not "fast": the bound on the
	// wait is the previous test's job.
	const budget = 20 * time.Second
	for _, action := range actions {
		start := time.Now()
		if err := action.run(); err != nil {
			t.Fatalf("%s failed under backlog: %v", action.name, err)
		}
		elapsed := time.Since(start)
		t.Logf("%s completed in %s under prefetch backlog", action.name, elapsed)
		if elapsed > budget {
			t.Errorf("%s took %s under prefetch backlog, want under %s", action.name, elapsed, budget)
		}
	}
}

// TestConcurrentUserActionsAllSucceed is the same contention with several actions
// racing each other as well as the prefetch: two browser tabs and an API client
// acting at once. Every one must finish and none may error — an operation that
// took the wrong lock, or one that returned while another held the connection,
// shows up here as a protocol error rather than as a slow response.
func TestConcurrentUserActionsAllSucceed(t *testing.T) {
	h := backloggedAccount(t, 200)
	ctx := context.Background()

	page, err := h.repo.ListMessages(ctx, ports.MessageFilter{AccountID: &h.account.ID, Limit: 30})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) < 12 {
		t.Fatalf("only %d messages synced", len(page.Items))
	}

	const actors = 12
	var wg sync.WaitGroup
	problems := make(chan string, actors)
	for index := range actors {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			var err error
			switch index % 4 {
			case 0:
				read := index%8 == 0
				err = h.supervisor.SetFlags(ctx, page.Items[index].ID, &read, nil)
			case 1:
				err = h.supervisor.FetchBody(ctx, page.Items[index].ID)
			case 2:
				err = h.supervisor.RequestMailbox(ctx, inboxMailboxID(t, h))
			case 3:
				_, err = h.supervisor.SetSeenBulk(ctx, []int64{page.Items[index].ID})
			}
			if err != nil {
				problems <- fmt.Sprintf("actor %d (kind %d): %v", index, index%4, err)
			}
		}(index)
	}
	wg.Wait()
	close(problems)
	for message := range problems {
		t.Error(message)
	}
	// The account must still be usable afterwards: a mis-sequenced command on a
	// shared connection leaves the client desynchronised, which surfaces as the
	// next operation failing rather than as the racing one failing.
	read := true
	if err := h.supervisor.SetFlags(ctx, page.Items[11].ID, &read, nil); err != nil {
		t.Fatalf("the connection is unusable after concurrent actions: %v", err)
	}
}

// TestPrefetchStillCompletesAfterForegroundBurst is the other direction of the
// invariant. Preemption must not starve the background: a burst of user actions
// may delay the prefetch but must not stop it, or a user who keeps clicking never
// gets the bodies of the messages they have not opened.
func TestPrefetchStillCompletesAfterForegroundBurst(t *testing.T) {
	h := backloggedAccount(t, 60)
	ctx := context.Background()

	page, err := h.repo.ListMessages(ctx, ports.MessageFilter{AccountID: &h.account.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) < 5 {
		t.Fatalf("only %d messages synced", len(page.Items))
	}
	// Thirty foreground operations back to back, which is what scrolling a feed and
	// clicking through it looks like. Each one raises urgent, so a background worker
	// that yielded on the first would never run again if the yield were permanent.
	for round := range 30 {
		read := round%2 == 0
		if err := h.supervisor.SetFlags(ctx, page.Items[round%5].ID, &read, nil); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	ready := bodyCounter(t, h)
	waitFor(t, 180*time.Second, func() bool { return ready() >= 60 })
}

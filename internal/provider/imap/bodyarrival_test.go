//go:build sqlite_fts5

package imap

import (
	"context"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
)

// The HTTP handler waits 3s for a body and then answers 202, telling the browser
// the pane will refresh on its own. That promise is only kept if the fetch that
// outran the budget announces itself, and only reached in reasonable time if the
// foreground is not queued behind the prefetch. Both are covered here, against
// the public FetchBody entry point rather than the internals — the existing
// contention test raises urgent and takes cmdMu directly, which is exactly the
// path that skips the bodySlots semaphore where the inversion lived.

// TestFetchBodyAnnouncesTheBody covers the 202 path: the caller has already been
// answered, so the event is the only thing that can bring the body to the screen.
func TestFetchBodyAnnouncesTheBody(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.supervisor.Stop)
	if _, err := h.user.Append("INBOX", literal{strings.NewReader(rawMessage("body arrival"))}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	messageID := waitForMessage(t, h)

	// Drain what the arrival and any prefetch already published, so the assertion
	// is about this fetch and not about the sync that produced the row.
	waitFor(t, 30*time.Second, func() bool {
		message, _, err := h.repo.GetMessage(ctx, messageID)
		return err == nil && message.BodyState != "metadata"
	})
	if err := h.repo.SetMessageBodyState(ctx, messageID, "metadata"); err != nil {
		t.Fatal(err)
	}
	before := h.events.count("MESSAGE_UPDATED")

	if err := h.supervisor.FetchBody(ctx, messageID); err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	if got := h.events.count("MESSAGE_UPDATED") - before; got < 1 {
		t.Fatalf("FetchBody published %d MESSAGE_UPDATED events, want at least 1: a body that lands after the handler's 3s budget would never reach the browser", got)
	}

	// The browser refreshes one message, so the event has to say which.
	found := false
	for _, event := range h.events.snapshot() {
		if event.Type != "MESSAGE_UPDATED" {
			continue
		}
		data, ok := event.Data.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := data["message_id"].(int64); ok && id == messageID {
			found = true
		}
	}
	if !found {
		t.Errorf("no MESSAGE_UPDATED named message %d; the pane cannot tell the event apart from one about other mail", messageID)
	}
}

// TestPrefetchNeverHoldsAForegroundSlot is the invariant behind the latency. The
// slot pool bounds foreground memory; the prefetch is bounded by its worker count
// and must not consume it, or a foreground fetch blocks on the semaphore before
// rt.lock() has raised urgent and the preemption it relies on never engages.
func TestPrefetchNeverHoldsAForegroundSlot(t *testing.T) {
	h := backloggedAccount(t, 200)
	ready := bodyCounter(t, h)

	// Sample while the workers are demonstrably busy: the backlog has to still be
	// draining at the end, or the samples prove nothing.
	worst := 0
	start := ready()
	for range 200 {
		if held := len(h.supervisor.bodySlots); held > worst {
			worst = held
		}
		time.Sleep(5 * time.Millisecond)
	}
	drained := ready() - start
	if drained < 5 {
		t.Fatalf("only %d bodies fetched while sampling; the prefetch was not running", drained)
	}
	if worst > 0 {
		t.Errorf("the prefetch held up to %d foreground body slots while draining %d bodies, want 0", worst, drained)
	}
}

// TestForegroundBodyFetchDoesNotQueueBehindPrefetch measures the public entry
// point end to end. The count is the assertion, not a duration: from the moment
// FetchBody is called, the only background fetch that may finish first is the one
// already holding the connection. A second means the foreground queued — before
// the split it paid one background fetch for a slot and another for the lock.
func TestForegroundBodyFetchDoesNotQueueBehindPrefetch(t *testing.T) {
	h := backloggedAccount(t, 300)
	ctx := context.Background()
	ready := bodyCounter(t, h)

	page, err := h.repo.ListMessages(ctx, ports.MessageFilter{AccountID: &h.account.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	// Oldest first: the candidate query works newest first, so these are the rows
	// the prefetch has not reached and a user opening one pays the full wait.
	targets := make([]int64, 0, 5)
	for i := len(page.Items) - 1; i >= 0 && len(targets) < 5; i-- {
		if page.Items[i].BodyState != "ready" {
			targets = append(targets, page.Items[i].ID)
		}
	}
	if len(targets) < 5 {
		t.Fatalf("only %d unfetched messages available to open", len(targets))
	}

	worst := 0
	for _, id := range targets {
		before := ready()
		if err := h.supervisor.FetchBody(ctx, id); err != nil {
			t.Fatalf("FetchBody(%d): %v", id, err)
		}
		// This fetch is one of the rows now ready; the rest are the prefetch's.
		during := ready() - before - 1
		if during < 0 {
			during = 0
		}
		if during > worst {
			worst = during
		}
		t.Logf("FetchBody(%d): %d background fetches completed during the call", id, during)
	}
	if worst > 1 {
		t.Errorf("a foreground fetch waited for %d background fetches, want at most 1", worst)
	}
}

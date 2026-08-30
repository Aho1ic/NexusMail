//go:build sqlite_fts5

package imap

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

// TestSetFlagsIssuesStoresInAStableOrder pins the wire order of the two STORE
// commands. It used to range over a map[goimap.Flag]*bool, so \Seen and \Flagged
// were emitted in whatever order the runtime picked that iteration: a provider
// that coalesces or only acknowledges the last STORE behaved differently run to
// run, and the failure was unreproducible by construction.
func TestSetFlagsIssuesStoresInAStableOrder(t *testing.T) {
	var mu sync.Mutex
	var lines []string

	h := newHarness(t)
	inner := h.supervisor.dial
	h.supervisor.dial = func(ctx context.Context, account domain.Account) (net.Conn, error) {
		conn, err := inner(ctx, account)
		if err != nil {
			return nil, err
		}
		return tapConn{Conn: conn, mu: &mu, lines: &lines}, nil
	}

	h.deliver(t, "flag-order")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()
	waitConnected(t, h)
	h.events.await(t, "NEW_EMAIL", 90*time.Second)

	page, err := h.repo.ListMessages(ctx, ports.MessageFilter{AccountID: &h.account.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 {
		t.Fatal("no message was ingested, cannot exercise SetFlags")
	}
	messageID := page.Items[0].ID

	// Both flags in one call is the case the map range made nondeterministic.
	read, starred := true, true
	for range 6 {
		mu.Lock()
		lines = nil
		mu.Unlock()

		if err := h.supervisor.SetFlags(ctx, messageID, &read, &starred); err != nil {
			t.Fatal(err)
		}

		mu.Lock()
		issued := append([]string(nil), lines...)
		mu.Unlock()

		var stores []string
		for _, line := range issued {
			upper := strings.ToUpper(line)
			if !strings.Contains(upper, "UID STORE") {
				continue
			}
			switch {
			case strings.Contains(upper, "\\SEEN"):
				stores = append(stores, "seen")
			case strings.Contains(upper, "\\FLAGGED"):
				stores = append(stores, "flagged")
			}
		}
		if len(stores) != 2 {
			t.Fatalf("SetFlags issued %d recognised stores (%v), want 2; wire: %v", len(stores), stores, issued)
		}
		if stores[0] != "seen" || stores[1] != "flagged" {
			t.Fatalf("store order = %v, want [seen flagged]", stores)
		}
	}
}

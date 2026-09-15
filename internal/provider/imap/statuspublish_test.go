//go:build sqlite_fts5

package imap

import (
	"context"
	"testing"
	"time"
)

// The client's connection dot is driven by the account list alone, and it re-reads
// that list on mount and on ACCOUNT_STATUS — nothing else moves it. Every error
// path published ACCOUNT_STATUS, but the transition back to 'connected' did not,
// so a page that read the list while the session was still connecting or syncing
// kept the offline dot indefinitely. Measured against the running gateway: sixty
// idle seconds produced exactly one account read and zero ACCOUNT_STATUS frames.
//
// This pins the publish, which is what lets the dot return to green without a
// reload. It must stay once per session: the connecting and syncing rewrites at
// the top of each loop iteration are deliberately not published.
func TestConnectedTransitionPublishesAccountStatus(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.deliver(t, "status-publish")
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()

	var accountID any
	waitFor(t, 30*time.Second, func() bool {
		for _, event := range h.events.snapshot() {
			if event.Type != "ACCOUNT_STATUS" {
				continue
			}
			data, ok := event.Data.(map[string]any)
			if !ok {
				continue
			}
			if data["status"] == "connected" {
				accountID = data["account_id"]
				return true
			}
		}
		return false
	})

	if id, ok := accountID.(int64); !ok || id != h.account.ID {
		t.Errorf("ACCOUNT_STATUS carries account_id %v, want %d", accountID, h.account.ID)
	}

	// The event has to describe a row that is actually connected, not a status the
	// publisher invented: the client re-reads the account list when it arrives.
	account, err := h.repo.GetAccount(context.Background(), h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "connected" {
		t.Errorf("account status is %q, want connected", account.Status)
	}
}

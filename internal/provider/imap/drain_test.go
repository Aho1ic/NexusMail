//go:build sqlite_fts5

package imap

import (
	"context"
	"errors"
	"testing"
	"time"

	imapclient "github.com/emersion/go-imap/v2/imapclient"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

// drainPending is reached in production only from inside commandLoop, while that loop
// holds the command lock and is between mailboxes. Its queue-handling branches are
// therefore hard to reach through the loop — a test would have to win a race with the
// loop's own draining — so they are driven directly here. What they decide matters:
// which mailbox a queued request actually syncs, and whether a request naming another
// account's mailbox can be serviced on this account's connection.

// drainSetup builds an unstarted runtime and a connected client, with the mailbox
// catalog populated.
//
// The runtime is built rather than started because a running commandLoop drains the
// same syncReq channel: the loop and the test would race for every queued request and
// the assertions would be decided by that race. Not starting it also means nothing has
// refreshed the catalog, so that is done here — otherwise there is no inbox row for a
// queued request to resolve to, and every test fails on the lookup instead of on what
// it is checking.
func (h *harness) drainSetup(t *testing.T, ctx context.Context) (*runtime, *imapclient.Client) {
	t.Helper()
	rt := &runtime{account: h.account, syncReq: make(chan int64, 8)}
	client := h.connect(t, ctx)
	t.Cleanup(func() { client.Close() })
	if _, err := h.supervisor.refreshMailboxCatalog(ctx, rt, client); err != nil {
		t.Fatalf("refresh mailbox catalog: %v", err)
	}
	return rt, client
}

// hasMessage reports whether a subject has landed in the database for this account.
func (h *harness) hasMessage(t *testing.T, subject string) bool {
	t.Helper()
	page, err := h.repo.ListMessages(context.Background(), ports.MessageFilter{AccountID: &h.account.ID, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range page.Items {
		if message.Subject == subject {
			return true
		}
	}
	return false
}

// TestDrainPendingHandlesAnEmptyQueue pins the default branch. Returning nil promptly
// is what lets commandLoop treat draining as cheap and call it between every mailbox.
func TestDrainPendingHandlesAnEmptyQueue(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, client := h.drainSetup(t, ctx)

	done := make(chan error, 1)
	go func() { done <- h.supervisor.drainPending(ctx, rt, client) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("drainPending on an empty queue returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("drainPending blocked on an empty queue")
	}
}

// TestDrainPendingSyncsTheInboxForAZeroRequest covers the sentinel. A zero mailbox id
// is what idleLoop and the realtime probe send, because they know new mail arrived
// without knowing where; resolving it to anything other than the inbox would mean new
// mail notifications sync the wrong folder.
func TestDrainPendingSyncsTheInboxForAZeroRequest(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A message the supervisor has not seen yet, so a successful drain is observable
	// as that message landing in the database.
	h.deliver(t, "queued by a zero request")

	rt, client := h.drainSetup(t, ctx)

	rt.syncReq <- 0
	if err := h.supervisor.drainPending(ctx, rt, client); err != nil {
		t.Fatalf("drainPending: %v", err)
	}

	if !h.hasMessage(t, "queued by a zero request") {
		t.Error("a zero request did not sync the inbox")
	}
}

// TestDrainPendingSyncsTheRequestedMailbox covers the ordinary path, where the caller
// knows which mailbox changed.
func TestDrainPendingSyncsTheRequestedMailbox(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h.deliver(t, "queued by mailbox id")

	rt, client := h.drainSetup(t, ctx)
	inbox, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}

	rt.syncReq <- inbox.ID
	if err := h.supervisor.drainPending(ctx, rt, client); err != nil {
		t.Fatalf("drainPending: %v", err)
	}

	if !h.hasMessage(t, "queued by mailbox id") {
		t.Error("a mailbox request did not sync that mailbox")
	}
}

// TestDrainPendingIgnoresAnotherAccountsMailbox is the guard that matters most here.
// The queue is a plain channel of ids, so a stale or crossed request can name a
// mailbox belonging to a different account; servicing it would select that mailbox on
// this account's connection, which the provider would refuse at best and, on a shared
// namespace, would sync one user's mail into another's account at worst.
func TestDrainPendingIgnoresAnotherAccountsMailbox(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A second account with its own mailbox row. It is never started, so nothing can
	// sync it legitimately; only the guard decides what happens.
	other := domain.Account{
		Provider: "qq", Email: "other@qq.com", DisplayName: "Other", Username: "other@qq.com",
		IMAPHost: "127.0.0.1", IMAPPort: 993, SMTPHost: "127.0.0.1", SMTPPort: 465,
		AuthType: "password", IMAPTLSMode: "implicit", SMTPTLSMode: "implicit",
		Status: "disconnected", CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
		// Not null in the schema, and never decrypted here: this account exists only
		// to own a mailbox row that the guard has to refuse.
		SecretCiphertext: []byte("not a real ciphertext"),
	}
	if err := h.repo.CreateAccount(ctx, &other); err != nil {
		t.Fatal(err)
	}
	foreign := domain.Mailbox{
		AccountID: other.ID, RemoteName: "INBOX", DisplayName: "INBOX", Role: "inbox",
		SyncMode: "realtime", UIDValidity: 1,
	}
	if err := h.repo.UpsertMailbox(ctx, &foreign); err != nil {
		t.Fatal(err)
	}
	stored, err := h.repo.GetMailboxByRole(ctx, other.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}

	rt, client := h.drainSetup(t, ctx)

	rt.syncReq <- stored.ID
	// The guard skips it and carries on, so this is a clean return rather than an
	// error: a foreign id is a stale request, not a failure of this connection.
	if err := h.supervisor.drainPending(ctx, rt, client); err != nil {
		t.Fatalf("drainPending on a foreign mailbox returned %v, want nil", err)
	}

	// And the foreign mailbox must be untouched: still no UIDNext recorded, because
	// nothing ever selected it.
	after, err := h.repo.GetMailbox(ctx, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UIDNext != nil {
		t.Errorf("the other account's mailbox was synced: UIDNext = %d", *after.UIDNext)
	}
}

// TestDrainPendingReportsAnUnknownMailbox covers the lookup error. A request naming a
// row that no longer exists has to stop the drain rather than be skipped, because the
// same error from a failing database would otherwise be silently swallowed on a path
// that is supposed to surface it and trigger a reconnect.
func TestDrainPendingReportsAnUnknownMailbox(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, client := h.drainSetup(t, ctx)

	rt.syncReq <- 999999
	err := h.supervisor.drainPending(ctx, rt, client)
	if err == nil {
		t.Fatal("drainPending accepted a mailbox id that does not exist")
	}
	if !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("error is %v, want it to wrap ErrNotFound", err)
	}
}

// The two tests below cover the sync failures. Both matter for the same reason: the
// caller treats an error from drainPending as a sign the command connection is no
// longer usable and rebuilds it. Swallowing either would leave the loop running
// against a dead connection, which is the stall this package has been bitten by
// before — so the requirement is that the error propagates, not that it is handled.
//
// A closed client is what makes the sync fail, because it is the failure the
// production path actually meets: the connection dropped underneath a queued request.

func TestDrainPendingReportsAFailedInboxSync(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, client := h.drainSetup(t, ctx)
	client.Close()

	rt.syncReq <- 0
	if err := h.supervisor.drainPending(ctx, rt, client); err == nil {
		t.Error("drainPending reported success after the inbox sync failed on a closed connection")
	}
}

func TestDrainPendingReportsAFailedMailboxSync(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, client := h.drainSetup(t, ctx)
	inbox, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	client.Close()

	rt.syncReq <- inbox.ID
	if err := h.supervisor.drainPending(ctx, rt, client); err == nil {
		t.Error("drainPending reported success after the mailbox sync failed on a closed connection")
	}
}

// TestDrainPendingDrainsEveryQueuedRequest pins that the loop keeps going. Returning
// after one request would leave the rest queued until the next tick, which is the
// minute-scale delay this queue exists to avoid.
func TestDrainPendingDrainsEveryQueuedRequest(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt, client := h.drainSetup(t, ctx)
	inbox, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}

	// Fill the queue to its capacity, mixing both request kinds.
	queued := 0
	for {
		select {
		case rt.syncReq <- inbox.ID:
			queued++
		default:
		}
		if queued == 0 {
			t.Fatal("could not queue any request")
		}
		if len(rt.syncReq) == cap(rt.syncReq) || queued > 1024 {
			break
		}
	}

	if err := h.supervisor.drainPending(ctx, rt, client); err != nil {
		t.Fatalf("drainPending: %v", err)
	}
	if remaining := len(rt.syncReq); remaining != 0 {
		t.Errorf("%d of %d requests were left queued", remaining, queued)
	}
}

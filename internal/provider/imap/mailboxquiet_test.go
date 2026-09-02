//go:build sqlite_fts5

package imap

import (
	"context"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/domain"

	goimap "github.com/emersion/go-imap/v2"
)

// mailboxQuiet is the skip decision in the periodic pass: a non-inbox mailbox that
// STATUS says has not moved, and whose reconciliation is not yet due, is passed
// over instead of costing a UID SEARCH on the connection new mail needs.
//
// Every wrong answer here is silent. A wrong true skips a mailbox that did receive
// mail, and nothing else in the pass will notice until reconciliation falls due —
// minutes to hours later depending on the role. A wrong false only costs a command.
// So the function is written to answer false on every uncertainty, and that
// asymmetry is what these tests pin.

// quietMailbox builds a mailbox row in the state that permits a skip: UIDNext and
// UIDValidity known, matching the server.
func quietMailbox(t *testing.T, h *harness, remote string) domain.Mailbox {
	t.Helper()
	id := localMailbox(t, h, remote, "custom")
	mailbox, err := h.repo.GetMailbox(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return mailbox
}

// serverUIDNext reads what the server reports, so the test agrees with it rather
// than assuming a value.
func serverUIDNext(t *testing.T, h *harness, ctx context.Context, remote string) (uidNext uint32, uidValidity uint32) {
	t.Helper()
	client := h.connect(t, ctx)
	defer func() { _ = client.Close() }()
	status, err := client.Status(remote, &goimap.StatusOptions{UIDNext: true, UIDValidity: true}).Wait()
	if err != nil {
		t.Fatalf("status %s: %v", remote, err)
	}
	return uint32(status.UIDNext), status.UIDValidity
}

// TestMailboxQuietSkipsOnlyWhenTheServerAgrees is the positive case, and the only
// one that may answer true.
func TestMailboxQuietSkipsOnlyWhenTheServerAgrees(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.user.Create("Quiet", nil); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	client := h.connect(t, ctx)
	t.Cleanup(func() { _ = client.Close() })

	mailbox := quietMailbox(t, h, "Quiet")
	uidNext, uidValidity := serverUIDNext(t, h, ctx, "Quiet")
	mailbox.UIDNext = &uidNext
	mailbox.UIDValidity = uidValidity
	// Reconciliation just happened, so it is not yet due.
	h.supervisor.lastReconcile.Store(mailbox.ID, time.Now())

	if !h.supervisor.mailboxQuiet(client, mailbox) {
		t.Error("a mailbox matching the server and recently reconciled was not skipped; every periodic pass pays a UID SEARCH for it")
	}
}

// TestMailboxQuietRefusesWhenNewMailArrived is the failure that matters. After an
// append the server's UIDNEXT has moved past what was stored, and skipping would
// leave that mail undiscovered until reconciliation falls due.
func TestMailboxQuietRefusesWhenNewMailArrived(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.user.Create("Busy", nil); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	client := h.connect(t, ctx)
	t.Cleanup(func() { _ = client.Close() })

	mailbox := quietMailbox(t, h, "Busy")
	uidNext, uidValidity := serverUIDNext(t, h, ctx, "Busy")
	mailbox.UIDNext = &uidNext
	mailbox.UIDValidity = uidValidity
	h.supervisor.lastReconcile.Store(mailbox.ID, time.Now())

	// Sanity: quiet before the delivery, so the assertion below is about the mail.
	if !h.supervisor.mailboxQuiet(client, mailbox) {
		t.Fatal("the mailbox was not quiet before the delivery; the test cannot attribute the change")
	}

	if _, err := h.user.Append("Busy", literal{strings.NewReader(rawMessage("newly arrived"))}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if h.supervisor.mailboxQuiet(client, mailbox) {
		t.Error("a mailbox that received mail was still reported quiet; the message would go unnoticed until reconciliation falls due")
	}
}

// TestMailboxQuietRefusesOnIncompleteLocalState covers the two guards before any
// command is sent. Without a stored UIDNEXT or UIDVALIDITY there is nothing to
// compare against, so a skip would be a guess.
func TestMailboxQuietRefusesOnIncompleteLocalState(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.user.Create("Partial", nil); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	client := h.connect(t, ctx)
	t.Cleanup(func() { _ = client.Close() })

	base := quietMailbox(t, h, "Partial")
	uidNext, uidValidity := serverUIDNext(t, h, ctx, "Partial")
	h.supervisor.lastReconcile.Store(base.ID, time.Now())

	noUIDNext := base
	noUIDNext.UIDNext = nil
	noUIDNext.UIDValidity = uidValidity
	if h.supervisor.mailboxQuiet(client, noUIDNext) {
		t.Error("a mailbox with no stored UIDNEXT was skipped; there was nothing to compare the server against")
	}

	noValidity := base
	noValidity.UIDNext = &uidNext
	noValidity.UIDValidity = 0
	if h.supervisor.mailboxQuiet(client, noValidity) {
		t.Error("a mailbox with no stored UIDVALIDITY was skipped; the UIDs it holds may belong to a previous incarnation")
	}
}

// TestMailboxQuietRefusesWhenReconciliationIsDue pins the time half. UIDNEXT does
// not move when a flag changes or a message is expunged elsewhere, so the interval
// is the only thing that brings those into view — and a skip past it would defer
// them indefinitely.
func TestMailboxQuietRefusesWhenReconciliationIsDue(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.user.Create("Due", nil); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	client := h.connect(t, ctx)
	t.Cleanup(func() { _ = client.Close() })

	mailbox := quietMailbox(t, h, "Due")
	uidNext, uidValidity := serverUIDNext(t, h, ctx, "Due")
	mailbox.UIDNext = &uidNext
	mailbox.UIDValidity = uidValidity

	// Never reconciled: no entry at all.
	if h.supervisor.mailboxQuiet(client, mailbox) {
		t.Error("a mailbox that was never reconciled was skipped")
	}

	// Reconciled longer ago than its role allows.
	h.supervisor.lastReconcile.Store(mailbox.ID, time.Now().Add(-2*reconcileIntervalFor(mailbox.Role)))
	if h.supervisor.mailboxQuiet(client, mailbox) {
		t.Error("a mailbox past its reconcile interval was skipped; flag changes and remote expunges would never be picked up")
	}

	// A wrong-typed entry is treated as absent rather than trusted.
	h.supervisor.lastReconcile.Store(mailbox.ID, "not a time")
	if h.supervisor.mailboxQuiet(client, mailbox) {
		t.Error("a non-time reconcile entry was trusted as recent")
	}
}

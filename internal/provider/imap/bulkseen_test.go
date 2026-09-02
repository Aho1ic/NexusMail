//go:build sqlite_fts5

package imap

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
)

// SetSeenBulk backs mark-all-read. Its return value is not decoration: the caller
// writes local rows only for the IDs the provider accepted, so anything wrongly
// included leaves a message that shows as read locally and unread everywhere else,
// and anything wrongly dropped makes the button look like it did nothing.
//
// The contract with no coverage was the partial failure. One unreachable mailbox
// must not discard the flags another mailbox already stored — the code collects
// errors and continues per mailbox precisely so a single bad folder cannot void
// the whole operation.

// seedInMailbox stores a message in one mailbox and returns its id, using the
// batch path a real sync uses.
func seedInMailbox(t *testing.T, h *harness, mailboxID int64, subject string, uid uint32) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	digest := sha256.Sum256([]byte(subject))
	message := domain.Message{
		AccountID: h.account.ID, Direction: "incoming", DedupeKey: digest[:], Subject: subject,
		Sender: "sender@example.com", Recipients: "mail@example.com",
		FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
		BodyState: "metadata", SizeBytes: 512, ReceivedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	ids, created, err := h.repo.BatchCreateOrUpdateMessages(ctx, []ports.MessageInput{{
		Message: &message, MailboxID: mailboxID, UID: uid, InternalDate: time.UnixMilli(now),
	}})
	if err != nil || !created[0] {
		t.Fatalf("seed %q: created=%v err=%v", subject, created, err)
	}
	return ids[0]
}

// localMailbox creates a mailbox row and returns its real id. UpsertMailbox writes
// raw SQL and leaves the struct id at zero.
func localMailbox(t *testing.T, h *harness, remote, role string) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	mailbox := domain.Mailbox{
		AccountID: h.account.ID, RemoteName: remote, DisplayName: remote, Role: role,
		SyncMode: "lazy", UIDValidity: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := h.repo.UpsertMailbox(ctx, &mailbox); err != nil {
		t.Fatal(err)
	}
	stored, err := h.repo.ListMailboxes(ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range stored {
		if candidate.RemoteName == remote {
			return candidate.ID
		}
	}
	t.Fatalf("mailbox %q was not stored", remote)
	return 0
}

// remoteSeen reports whether the server has \Seen on a UID, which is the only
// evidence that the flag really left this process.
func remoteSeen(t *testing.T, h *harness, ctx context.Context, mailbox string, uid uint32) bool {
	t.Helper()
	client := h.connect(t, ctx)
	defer func() { _ = client.Close() }()
	if _, err := client.Select(mailbox, &goimap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatalf("select %s: %v", mailbox, err)
	}
	items, err := client.Fetch(goimap.UIDSetNum(goimap.UID(uid)), &goimap.FetchOptions{Flags: true}).Collect()
	if err != nil {
		t.Fatalf("fetch flags: %v", err)
	}
	if len(items) == 0 {
		t.Fatalf("uid %d not found in %s", uid, mailbox)
	}
	for _, flag := range items[0].Flags {
		if flag == goimap.FlagSeen {
			return true
		}
	}
	return false
}

// TestSetSeenBulkKeepsGoingPastAnUnreachableMailbox is the partial-failure
// contract. A mailbox row whose remote name does not exist on the server fails at
// SELECT; the reachable mailbox's messages must still be flagged and reported.
func TestSetSeenBulkKeepsGoingPastAnUnreachableMailbox(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.supervisor.Stop)

	h.deliver(t, "real inbox mail")
	good := waitForMessage(t, h)

	// A mailbox that exists locally but not on the server: the folder a provider
	// removed between two syncs.
	ghostID := localMailbox(t, h, "Ghost", "custom")
	ghost := seedInMailbox(t, h, ghostID, "mail in a folder that is gone", 4242)

	done, err := h.supervisor.SetSeenBulk(ctx, []int64{good, ghost})
	if err == nil {
		t.Error("SetSeenBulk hid the unreachable mailbox entirely; the caller cannot tell the flags were partial")
	}
	if len(done) != 1 || done[0] != good {
		t.Fatalf("accepted ids = %v, want exactly [%d]: the reachable mailbox's flag was discarded along with the bad one", done, good)
	}
	if !remoteSeen(t, h, ctx, "INBOX", 1) {
		t.Error("the reachable message was never flagged on the server")
	}
}

// TestSetSeenBulkAcceptsNothingOnADeadConnection pins the answer when the socket
// is gone. An empty accepted list is what stops the caller writing local rows the
// provider never saw; a non-empty one makes every message read locally and unread
// on the server, with nothing left to reconcile them.
//
// Stop cancels the runtime and closes the client but leaves rt.client set, so this
// reaches the failing SELECT rather than the explicit client == nil branch. That
// branch needs a runtime that has never connected and stays uncovered.
func TestSetSeenBulkAcceptsNothingOnADeadConnection(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}

	h.deliver(t, "offline before mark read")
	messageID := waitForMessage(t, h)

	// Stop the supervisor: the runtime survives and its client is closed under it.
	h.supervisor.Stop()

	done, err := h.supervisor.SetSeenBulk(context.Background(), []int64{messageID})
	if err == nil {
		t.Error("SetSeenBulk on a dead connection reported success")
	}
	if len(done) != 0 {
		t.Errorf("accepted ids = %v, want none: the caller would mark rows read that the provider never saw", done)
	}
}

// TestSetSeenBulkFlagsEveryMailboxItWasGiven covers the multi-mailbox loop, which
// releases and re-takes the command lock per mailbox. Both mailboxes must end up
// flagged: taking the lock once per account and holding it would work here too, so
// the assertion is on the result, and the lock discipline itself is pinned by the
// contention tests.
func TestSetSeenBulkFlagsEveryMailboxItWasGiven(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.supervisor.Stop)

	if err := h.user.Create("Later", nil); err != nil {
		t.Fatal(err)
	}
	h.deliver(t, "inbox copy")
	inboxID := waitForMessage(t, h)

	// Put a message in the second mailbox on the server, then mirror it locally so
	// the location lookup resolves there.
	later := localMailbox(t, h, "Later", "custom")
	if _, err := h.user.Append("Later", literal{strings.NewReader(rawMessage("later copy"))}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	laterID := seedInMailbox(t, h, later, "later copy", 1)

	done, err := h.supervisor.SetSeenBulk(ctx, []int64{inboxID, laterID})
	if err != nil {
		t.Fatalf("SetSeenBulk: %v", err)
	}
	if len(done) != 2 {
		t.Fatalf("accepted %d ids, want 2: %v", len(done), done)
	}
	if !remoteSeen(t, h, ctx, "INBOX", 1) {
		t.Error("the inbox message was not flagged on the server")
	}
	if !remoteSeen(t, h, ctx, "Later", 1) {
		t.Error("the second mailbox's message was not flagged; the per-mailbox loop stopped after the first")
	}
}

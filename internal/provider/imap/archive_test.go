//go:build sqlite_fts5

package imap

import (
	"context"
	"testing"
	"time"

	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// waitForMessage blocks until the first synced message is in the repository and
// returns its id, so an archive test can act on a message that really exists on
// both sides.
func waitForMessage(t *testing.T, h *harness) int64 {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		page, err := h.repo.ListMessages(ctx, ports.MessageFilter{AccountID: &h.account.ID, Limit: 10})
		if err == nil && len(page.Items) > 0 {
			return page.Items[0].ID
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("message never synced")
	return 0
}

// remoteUIDs returns the UIDs a second client sees in a mailbox, which is how
// these tests assert what actually happened on the server rather than only what
// the local database concluded.
func remoteUIDs(t *testing.T, client *imapclient.Client, mailbox string) []goimap.UID {
	t.Helper()
	if _, err := client.Select(mailbox, nil).Wait(); err != nil {
		t.Fatalf("select %s: %v", mailbox, err)
	}
	data, err := client.UIDSearch(&goimap.SearchCriteria{}, nil).Wait()
	if err != nil {
		t.Fatalf("search %s: %v", mailbox, err)
	}
	return data.AllUIDs()
}

// TestArchiveCreatesMissingMailbox covers the reported failure: QQ ships no
// archive folder and advertises no \Archive attribute, so the archive role was
// simply absent and every click returned "archive mailbox is unavailable". The
// mailbox has to be created on the provider instead.
func TestArchiveCreatesMissingMailbox(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, h)
	h.deliver(t, "archive-me")
	messageID := waitForMessage(t, h)

	if _, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "archive"); err == nil {
		t.Fatal("account already had an archive mailbox; the test proves nothing")
	}
	if err := h.supervisor.Archive(ctx, messageID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	archive, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "archive")
	if err != nil {
		t.Fatalf("archive mailbox still missing: %v", err)
	}
	if archive.RemoteName != "Archive" {
		t.Fatalf("archive remote name = %q, want %q", archive.RemoteName, "Archive")
	}

	client := h.connect(t, ctx)
	defer client.Close()
	if uids := remoteUIDs(t, client, "Archive"); len(uids) != 1 {
		t.Fatalf("archive mailbox holds %d messages remotely, want 1", len(uids))
	}
	if uids := remoteUIDs(t, client, "INBOX"); len(uids) != 0 {
		t.Fatalf("INBOX still holds %d messages remotely, want 0", len(uids))
	}
}

// TestArchiveWithoutMoveExpungesSource pins the fallback path. Without MOVE the
// message is COPYed and flagged \Deleted, and without UIDPLUS there is no
// UID EXPUNGE — so skipping the expunge left the message in the provider's own
// inbox after the user archived it here. QQ is exactly that provider.
func TestArchiveWithoutMoveExpungesSource(t *testing.T) {
	h := newHarness(t, withoutMoveAndUIDPlus())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, h)
	h.deliver(t, "archive-no-move")
	messageID := waitForMessage(t, h)

	if err := h.supervisor.Archive(ctx, messageID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	client := h.connect(t, ctx)
	defer client.Close()
	if client.Caps().Has(goimap.CapMove) || client.Caps().Has(goimap.CapUIDPlus) {
		t.Fatal("harness still advertises MOVE or UIDPLUS; the fallback path was not exercised")
	}
	if uids := remoteUIDs(t, client, "INBOX"); len(uids) != 0 {
		t.Fatalf("INBOX still holds %d messages remotely after archive, want 0", len(uids))
	}
	if uids := remoteUIDs(t, client, "Archive"); len(uids) != 1 {
		t.Fatalf("archive mailbox holds %d messages remotely, want 1", len(uids))
	}
}

// TestArchiveKeepsOtherPendingDeletes guards the plain EXPUNGE. EXPUNGE removes
// every \Deleted message in the mailbox, so archiving one message must not
// finalise a delete another client left pending. The archived message stays on the
// server in that case, which is the lesser harm.
func TestArchiveKeepsOtherPendingDeletes(t *testing.T) {
	h := newHarness(t, withoutMoveAndUIDPlus())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, h)
	h.deliver(t, "keep-me-pending")
	h.deliver(t, "archive-me-too")
	deadline := time.Now().Add(30 * time.Second)
	var messageID int64
	for time.Now().Before(deadline) {
		page, err := h.repo.ListMessages(ctx, ports.MessageFilter{AccountID: &h.account.ID, Limit: 10})
		if err == nil && len(page.Items) == 2 {
			messageID = page.Items[0].ID
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if messageID == 0 {
		t.Fatal("both messages never synced")
	}
	location, err := h.repo.MessageLocation(ctx, messageID)
	if err != nil {
		t.Fatal(err)
	}

	// Another client marks a different message \Deleted without expunging it.
	other := h.connect(t, ctx)
	defer other.Close()
	if _, err := other.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	all := remoteUIDsSelected(t, other)
	var pending goimap.UID
	for _, uid := range all {
		if uint32(uid) != location.UID {
			pending = uid
			break
		}
	}
	if pending == 0 {
		t.Fatalf("no second UID to leave pending; remote UIDs = %v", all)
	}
	if _, err := other.Store(goimap.UIDSetNum(pending), &goimap.StoreFlags{
		Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted},
	}, nil).Collect(); err != nil {
		t.Fatal(err)
	}

	if err := h.supervisor.Archive(ctx, messageID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	// The pending delete must survive. The archived copy must still have been made.
	uids := remoteUIDs(t, other, "INBOX")
	found := false
	for _, uid := range uids {
		if uid == pending {
			found = true
		}
	}
	if !found {
		t.Fatalf("another client's pending \\Deleted message was expunged; INBOX UIDs = %v", uids)
	}
	if archived := remoteUIDs(t, other, "Archive"); len(archived) != 1 {
		t.Fatalf("archive mailbox holds %d messages remotely, want 1", len(archived))
	}
}

// remoteUIDsSelected lists the UIDs of the already-selected mailbox.
func remoteUIDsSelected(t *testing.T, client *imapclient.Client) []goimap.UID {
	t.Helper()
	data, err := client.UIDSearch(&goimap.SearchCriteria{}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	return data.AllUIDs()
}

//go:build sqlite_fts5

package imap

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// UIDVALIDITY is the provider's statement about which incarnation of a mailbox a
// UID belongs to. QQ and 163 change it whenever the server rebuilds the mailbox
// index, and every stored UID then names a different message — or none.
//
// DeleteRemoteDraft compared it before acting; the four paths that act on a stored
// UID right after SELECT did not. They all route through selectForStoredUID now,
// and the tests below pin what each one does about a refusal, because that differs:
// a body fetch has a retry path, an archive and a single flag change do not, and
// the bulk mark-read has to skip one mailbox without voiding the others.
//
// They run against a manually built runtime rather than a started supervisor. A
// running commandLoop probes the inbox every 5 seconds, and the mismatch these
// tests install is exactly what makes that probe reset the mailbox — so the loop
// would delete the row under the assertion and the failure would look like a
// missing message rather than a missing guard.
func (h *harness) offlineRuntime(t *testing.T, ctx context.Context) (*runtime, *imapclient.Client) {
	t.Helper()
	rt, client := h.drainSetup(t, ctx)
	rt.client.Store(client)
	h.supervisor.mu.Lock()
	h.supervisor.runtimes[h.account.ID] = rt
	h.supervisor.mu.Unlock()
	// Unregistered directly, not via StopAccount: this runtime has no cancel func
	// because nothing started loops for it.
	t.Cleanup(func() {
		h.supervisor.mu.Lock()
		delete(h.supervisor.runtimes, h.account.ID)
		h.supervisor.mu.Unlock()
	})
	return rt, client
}

// rewriteUIDValidity points the stored mailbox row at a different incarnation than
// the one the server reports, which is what the provider's renumbering looks like
// from here. It is written straight to the column: UpdateMailboxCursor is the only
// setter and it would also move the cursor, which is not the state under test.
func rewriteUIDValidity(t *testing.T, h *harness, mailboxID int64, value uint32) {
	t.Helper()
	database, err := sql.Open("sqlite3", "file:"+h.dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec("UPDATE mailboxes SET uid_validity = ? WHERE id = ?", value, mailboxID); err != nil {
		t.Fatalf("rewrite uid_validity: %v", err)
	}
}

// syncInbox runs one ingest pass over the inbox on the caller's connection, which
// is what a started supervisor would have done before the test interferes with the
// stored validity.
func syncInbox(t *testing.T, h *harness, ctx context.Context, client *imapclient.Client) domain.Mailbox {
	t.Helper()
	inbox, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "inbox")
	if err != nil {
		t.Fatalf("inbox role: %v", err)
	}
	if err := h.supervisor.syncMailbox(ctx, client, inbox, true); err != nil {
		t.Fatalf("sync inbox: %v", err)
	}
	inbox, err = h.repo.GetMailboxByRole(ctx, h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	return inbox
}

func onlyMessage(t *testing.T, h *harness, ctx context.Context) domain.Message {
	t.Helper()
	page, err := h.repo.ListMessages(ctx, ports.MessageFilter{AccountID: &h.account.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("the account holds %d messages, want exactly 1", len(page.Items))
	}
	return page.Items[0]
}

// A body fetched under the wrong UIDVALIDITY would store a stranger's message text
// on this row — and hand its verification code to the OTP notification. The fetch
// must fail so the deferred rollback leaves body_state 'error', which is the state
// the next sync of this mailbox repairs by resetting and re-ingesting it.
func TestFetchBodyRefusesAStaleUIDValidity(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h.deliver(t, "stale-body")

	_, client := h.offlineRuntime(t, ctx)
	inbox := syncInbox(t, h, ctx, client)
	message := onlyMessage(t, h, ctx)

	rewriteUIDValidity(t, h, inbox.ID, inbox.UIDValidity+1)

	if err := h.supervisor.FetchBody(ctx, message.ID); !errors.Is(err, ports.ErrUnavailable) {
		t.Fatalf("FetchBody under a stale validity returned %v, want an unavailable error", err)
	}
	stored, _, err := h.repo.GetMessage(ctx, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BodyState != "error" {
		t.Errorf("body_state is %q, want error so the next sync repairs the row", stored.BodyState)
	}
	if stored.BodyText != "" || stored.BodyHTML != "" || stored.RawBlobID != nil {
		t.Errorf("a body was stored despite the UIDVALIDITY change: %q / %q / %v", stored.BodyText, stored.BodyHTML, stored.RawBlobID)
	}
}

// Archive is destructive on the provider: MOVE, or COPY + \Deleted + EXPUNGE. On a
// stale UID that acts on whichever message now holds the number, and reporting
// success would additionally let MoveMessageLocation file the local row — so the
// user sees the mail archived while it is still in their inbox everywhere else, and
// some other mail has quietly moved. It has to fail.
func TestArchiveRefusesAStaleUIDValidity(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h.deliver(t, "stale-archive")

	_, client := h.offlineRuntime(t, ctx)
	inbox := syncInbox(t, h, ctx, client)
	message := onlyMessage(t, h, ctx)

	rewriteUIDValidity(t, h, inbox.ID, inbox.UIDValidity+1)

	if err := h.supervisor.Archive(ctx, message.ID); !errors.Is(err, ports.ErrUnavailable) {
		t.Fatalf("Archive under a stale validity returned %v, want an unavailable error", err)
	}

	// Nothing may have moved on either side. The remote check uses an independent
	// client so the supervisor's own view cannot mask a MOVE that happened.
	inspector := h.connect(t, ctx)
	defer func() { _ = inspector.Close() }()
	remaining := remoteUIDs(t, inspector, "INBOX")
	if len(remaining) != 1 {
		t.Errorf("INBOX holds %d messages after a refused archive, want the untouched 1", len(remaining))
	}
	locations, err := h.repo.MessageLocations(ctx, []int64{message.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || locations[0].Mailbox.ID != inbox.ID {
		t.Errorf("the local row was filed as %+v, want it left in the inbox", locations)
	}
}

// TestUIDValidityRebuildKeepsMailOlderThanTheFirstSyncWindow is the regression for
// the reported loss. A UIDVALIDITY change runs ResetMailbox, which deletes every
// local row for the mailbox, and sets the cursor to 0 — and a zero cursor is also
// how "this mailbox has never been synced" looks, so searchNewUIDs asked only for
// the last 30 days. Everything older was deleted locally, excluded from the
// refetch, and then written off for good: the cursor advances past it on the same
// pass, so no later sync asks again. The mail was still on the provider, visible in
// its own web client and gone from this one.
func TestUIDValidityRebuildKeepsMailOlderThanTheFirstSyncWindow(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, client := h.offlineRuntime(t, ctx)

	// The seeding is done in two passes because the first pass is exactly the
	// 30-day import this test is about: a message dated 60 days back would not be
	// picked up by it. One recent message establishes the cursor, and the older mail
	// then arrives above it — which is what a mailbox synced months ago holds, and
	// also what a COPY or an IMAP move into the folder produces on any provider.
	h.deliver(t, "recent")
	syncInbox(t, h, ctx, client)

	// 60 days old: safely outside the 30-day window, and the age of perfectly
	// ordinary mail in a long-lived mailbox.
	ancient := time.Now().AddDate(0, 0, -60)
	if _, err := h.user.Append("INBOX", literal{strings.NewReader(rawMessage("ancient"))}, &goimap.AppendOptions{Time: ancient}); err != nil {
		t.Fatalf("append ancient: %v", err)
	}
	inbox := syncInbox(t, h, ctx, client)
	if uids, err := h.repo.ListMailboxUIDs(ctx, inbox.ID); err != nil || len(uids) != 2 {
		t.Fatalf("the seeded mailbox holds %v (err %v), want both UIDs", uids, err)
	}

	// The provider rebuilt its index: same messages, new incarnation.
	rewriteUIDValidity(t, h, inbox.ID, inbox.UIDValidity+1)
	stale, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.supervisor.syncMailbox(ctx, client, stale, true); err != nil {
		t.Fatalf("sync after the rebuild: %v", err)
	}

	page, err := h.repo.ListMessages(ctx, ports.MessageFilter{AccountID: &h.account.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	subjects := make(map[string]bool, len(page.Items))
	for _, item := range page.Items {
		subjects[item.Subject] = true
	}
	for _, subject := range []string{"ancient", "recent"} {
		if !subjects[subject] {
			t.Errorf("%q was lost by the UIDVALIDITY rebuild; stored subjects are %v", subject, subjects)
		}
	}
}

// A stale UID here sets \Seen on whichever message now holds that number, which is
// the one failure the user cannot notice: nothing distinguishes mail wrongly marked
// read from mail they read and forgot, so what they actually miss is invisible to
// them. The click has to come back as an error instead.
func TestSetFlagsRefusesAStaleUIDValidity(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h.deliver(t, "stale-flags")

	_, client := h.offlineRuntime(t, ctx)
	inbox := syncInbox(t, h, ctx, client)
	message := onlyMessage(t, h, ctx)

	rewriteUIDValidity(t, h, inbox.ID, inbox.UIDValidity+1)

	read := true
	if err := h.supervisor.SetFlags(ctx, message.ID, &read, nil); !errors.Is(err, ports.ErrUnavailable) {
		t.Fatalf("SetFlags under a stale validity returned %v, want an unavailable error", err)
	}
	// Asserted on the server with an independent client: the local row is written by
	// the service layer only after this call succeeds, so checking it would pass
	// whether or not the STORE went out.
	if remoteSeen(t, h, ctx, "INBOX", 1) {
		t.Error("\\Seen was set on the server despite the UIDVALIDITY change")
	}
}

// SetSeenBulk backs mark-all-read across every account and mailbox at once. A
// renumbered folder must be skipped rather than acted on, and skipping it must not
// void the folders that are fine: the caller writes local rows only for the ids
// this returns, so an id wrongly included hides unopened mail and an id wrongly
// dropped makes the button look broken.
func TestSetSeenBulkSkipsARenumberedMailboxAndKeepsTheRest(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.user.Create("Later", nil); err != nil {
		t.Fatalf("create Later: %v", err)
	}
	h.deliver(t, "in-inbox")
	if _, err := h.user.Append("Later", literal{strings.NewReader(rawMessage("in-later"))}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatalf("append to Later: %v", err)
	}

	_, client := h.offlineRuntime(t, ctx)
	mailboxes, err := h.repo.ListMailboxes(ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]domain.Mailbox, len(mailboxes))
	for _, mailbox := range mailboxes {
		if err := h.supervisor.syncMailbox(ctx, client, mailbox, true); err != nil {
			t.Fatalf("sync %s: %v", mailbox.RemoteName, err)
		}
	}
	mailboxes, err = h.repo.ListMailboxes(ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mailbox := range mailboxes {
		byName[mailbox.RemoteName] = mailbox
	}

	page, err := h.repo.ListMessages(ctx, ports.MessageFilter{AccountID: &h.account.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]int64, len(page.Items))
	for _, item := range page.Items {
		ids[item.Subject] = item.ID
	}
	if len(ids) != 2 {
		t.Fatalf("stored subjects are %v, want both messages", ids)
	}

	// Only the inbox is renumbered. "Later" must still be flagged.
	rewriteUIDValidity(t, h, byName["INBOX"].ID, byName["INBOX"].UIDValidity+1)

	done, err := h.supervisor.SetSeenBulk(ctx, []int64{ids["in-inbox"], ids["in-later"]})
	if !errors.Is(err, ports.ErrUnavailable) {
		t.Fatalf("SetSeenBulk error = %v, want the renumbered mailbox reported as unavailable", err)
	}
	if len(done) != 1 || done[0] != ids["in-later"] {
		t.Fatalf("SetSeenBulk accepted %v, want only the id in the healthy mailbox (%d)", done, ids["in-later"])
	}
	if remoteSeen(t, h, ctx, "INBOX", 1) {
		t.Error("\\Seen was set in the renumbered mailbox")
	}
	if !remoteSeen(t, h, ctx, "Later", 1) {
		t.Error("the healthy mailbox was not flagged, so one renumbered folder voided the whole operation")
	}
}

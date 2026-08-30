package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

// sessionRows counts stored rows for one token digest. ValidateSession cannot answer
// this: it rejects an expired row and a missing row identically, so on its own it
// cannot tell a deleted session from one merely past its deadline — and deletion is
// what the sweep is for.
func sessionRows(t *testing.T, store *Store, tokenHash []byte) int {
	t.Helper()
	var count int
	err := store.sqlDB.QueryRowContext(context.Background(),
		"SELECT count(*) FROM web_sessions WHERE token_hash = ?", tokenHash).Scan(&count)
	if err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return count
}

// TestResetMailboxDropsTheMailboxContents covers the UIDVALIDITY-change path, which
// had no test at all. It is the most destructive unattended path in the repository: it
// deletes rows rather than updating them when a provider renumbers a mailbox. A
// mistake here silently loses mail.
func TestResetMailboxDropsTheMailboxContents(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	ids := seedMessages(t, store, mailbox.ID, 3, "first", "second", "third")
	// The cursor is advanced first so the assertions below are not vacuous: a freshly
	// seeded mailbox already has last_uid 0 and last_sync_at NULL, so checking for
	// those after a reset proves nothing unless they started somewhere else.
	if err := store.UpdateMailboxCursor(ctx, mailbox.ID, 42, 3, ptr32(4), nil); err != nil {
		t.Fatalf("advance cursor: %v", err)
	}

	if err := store.ResetMailbox(ctx, mailbox.ID, 99); err != nil {
		t.Fatalf("reset mailbox: %v", err)
	}

	page, err := store.ListMessages(ctx, ports.MessageFilter{
		AccountID: &account.ID, MailboxID: &mailbox.ID, Limit: 50,
	})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("mailbox still lists %d messages after a reset", len(page.Items))
	}
	// The orphaned incoming messages are collected too, not left as rows nothing can
	// reach and no sync will ever refresh.
	for _, id := range ids {
		if _, _, err := store.GetMessage(ctx, id); err == nil {
			t.Errorf("message %d survived the reset with no mailbox mapping", id)
		}
	}
	// The new UIDVALIDITY is recorded and the cursor rewound. Without the rewind the
	// next sync asks for UIDs above a watermark from the old numbering, so the mail
	// that was just dropped is never fetched again.
	mailboxes, err := store.ListMailboxes(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mailboxes[0].UIDValidity != 99 {
		t.Errorf("uid_validity = %d, want 99", mailboxes[0].UIDValidity)
	}
	if mailboxes[0].LastUID != 0 {
		t.Errorf("last_uid = %d, want the cursor rewound to 0", mailboxes[0].LastUID)
	}
	if mailboxes[0].LastSyncAt != nil {
		t.Errorf("last_sync_at = %v, want nil so a full sync is forced", *mailboxes[0].LastSyncAt)
	}
}

// TestResetMailboxKeepsOtherMailboxesAndSentMail pins the two limits on that
// deletion. The orphan sweep is a global NOT IN over messages, so without these the
// blast radius of one renumbered mailbox is the whole database.
func TestResetMailboxKeepsOtherMailboxesAndSentMail(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, inbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()

	work := domain.Mailbox{
		AccountID: account.ID, RemoteName: "Work", DisplayName: "Work", Role: "custom",
		SyncMode: "periodic", UIDValidity: 42, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.UpsertMailbox(ctx, &work); err != nil {
		t.Fatal(err)
	}
	mailboxes, err := store.ListMailboxes(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var workID int64
	for _, box := range mailboxes {
		if box.RemoteName == "Work" {
			workID = box.ID
		}
	}
	if workID == 0 {
		t.Fatal("the second mailbox was not created")
	}

	keptID := seedMessage(t, store, account.ID, workID, 5001, "kept elsewhere", "incoming", false, now)
	// Outgoing mail has no mailbox mapping at all, which is exactly the shape the
	// orphan sweep looks for. Only the direction predicate spares it; drop that and
	// resetting any mailbox erases the user's sent mail.
	digest := sha256.Sum256([]byte("sent-1"))
	sent := domain.Message{
		AccountID: account.ID, DedupeKey: digest[:], Direction: "outgoing", Subject: "sent mail",
		Sender: "me@example.com", Recipients: "them@example.com",
		FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
		BodyState: "ready", ReceivedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateSentMessage(ctx, &sent, 0); err != nil {
		t.Fatalf("create sent message: %v", err)
	}
	droppedID := seedMessage(t, store, account.ID, inbox.ID, 6001, "dropped", "incoming", false, now)

	if err := store.ResetMailbox(ctx, inbox.ID, 100); err != nil {
		t.Fatalf("reset mailbox: %v", err)
	}

	if _, _, err := store.GetMessage(ctx, keptID); err != nil {
		t.Errorf("a message in another mailbox was deleted: %v", err)
	}
	if _, _, err := store.GetMessage(ctx, sent.ID); err != nil {
		t.Errorf("outgoing mail was swept as an orphan: %v", err)
	}
	if _, _, err := store.GetMessage(ctx, droppedID); err == nil {
		t.Error("the reset mailbox's own message survived")
	}
}

// TestDeleteExpiredSessionsHonoursBothDeadlines covers the maintenance ticker's
// session sweep, which had no test. Both columns matter: expires_at is the idle
// deadline sliding renewal pushes forward, absolute_expires_at is the cap renewal
// cannot move. Checking only the first lets a session renewed often enough outlive
// its absolute cap forever.
func TestDeleteExpiredSessionsHonoursBothDeadlines(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	hour := time.Hour.Milliseconds()

	create := func(token string, expiresAt, absoluteExpiresAt int64) []byte {
		hash := HashBytes([]byte(token))
		if err := store.CreateSession(ctx, hash, HashBytes([]byte(token+"-csrf")), expiresAt, absoluteExpiresAt); err != nil {
			t.Fatalf("create session %s: %v", token, err)
		}
		return hash
	}
	live := create("live", now+hour, now+24*hour)
	idleExpired := create("idle", now-1, now+24*hour)
	capExpired := create("cap", now+hour, now-1)

	if err := store.DeleteExpiredSessions(ctx, now); err != nil {
		t.Fatalf("delete expired sessions: %v", err)
	}

	if rows := sessionRows(t, store, live); rows != 1 {
		t.Errorf("the live session was swept: %d rows remain", rows)
	}
	if _, ok, err := store.ValidateSession(ctx, live, now); err != nil || !ok {
		t.Errorf("the live session stopped validating: ok=%v err=%v", ok, err)
	}
	for name, hash := range map[string][]byte{"idle-expired": idleExpired, "absolute-expired": capExpired} {
		if rows := sessionRows(t, store, hash); rows != 0 {
			t.Errorf("the %s session survived the sweep", name)
		}
	}
}

// TestDeleteExpiredSessionsIsBoundaryInclusive pins the <= in the predicate. A
// session whose deadline is exactly now is expired; with a strict < it lingers until
// the next tick, which is another 15 minutes of a row that should be gone.
func TestDeleteExpiredSessionsIsBoundaryInclusive(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	hash := HashBytes([]byte("exactly-now"))
	if err := store.CreateSession(ctx, hash, HashBytes([]byte("csrf")), now, now+24*time.Hour.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteExpiredSessions(ctx, now); err != nil {
		t.Fatal(err)
	}
	if rows := sessionRows(t, store, hash); rows != 0 {
		t.Error("a session expiring exactly at now survived the sweep")
	}
}

// TestDeleteExpiredSessionsLeavesAnEmptyTableAlone covers the ordinary tick: the
// sweep runs every 15 minutes for the life of the process and almost always has
// nothing to do, so finding no rows must not be an error.
func TestDeleteExpiredSessionsLeavesAnEmptyTableAlone(t *testing.T) {
	store := openTestStore(t)
	if err := store.DeleteExpiredSessions(context.Background(), time.Now().UnixMilli()); err != nil {
		t.Errorf("sweeping an empty table failed: %v", err)
	}
}

// TestHashBytesIsSHA256 pins what the column stores. Session tokens are looked up by
// this digest and never stored in the clear, so the algorithm is part of the on-disk
// format: changing it invalidates every stored session at once.
func TestHashBytesIsSHA256(t *testing.T) {
	value := []byte("token-material")
	want := sha256.Sum256(value)
	got := HashBytes(value)
	if !bytes.Equal(got, want[:]) {
		t.Errorf("HashBytes = %x, want the sha256 digest %x", got, want)
	}
	if len(got) != sha256.Size {
		t.Errorf("digest length = %d, want %d", len(got), sha256.Size)
	}
	if bytes.Equal(HashBytes([]byte("a")), HashBytes([]byte("b"))) {
		t.Error("two different tokens hashed to the same digest")
	}
	// Hashing no bytes still yields a full-width digest, so an empty token cannot
	// match a row stored with a zero-length hash.
	if len(HashBytes(nil)) != sha256.Size {
		t.Error("hashing no bytes did not produce a full-width digest")
	}
}

//go:build sqlite_fts5

package sqlite

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

// TestReconcileMailboxFlagsAppliesRemoteState covers the case that made the app
// disagree with every other client: mail read or starred elsewhere kept its old
// local state forever, because sync only ever appended new UIDs.
func TestReconcileMailboxFlagsAppliesRemoteState(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	_, mailbox := seedAccountMailbox(t, store)
	ids := seedMessages(t, store, mailbox.ID, 3)

	changed, err := store.ReconcileMailboxFlags(ctx, mailbox.ID, []ports.RemoteFlagState{
		{UID: 1, IsRead: true, Flags: []string{"\\Seen"}},
		{UID: 2, IsRead: false, IsStarred: true, Flags: []string{"\\Flagged"}},
		{UID: 3, Flags: []string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The third message matched what was already stored, so it must not be counted.
	if changed != 2 {
		t.Fatalf("changed = %d, want 2", changed)
	}
	first := getMessage(t, store, ids[0])
	if !first.IsRead || first.IsStarred {
		t.Fatalf("uid 1 not marked read: read=%v starred=%v", first.IsRead, first.IsStarred)
	}
	second := getMessage(t, store, ids[1])
	if second.IsRead || !second.IsStarred {
		t.Fatalf("uid 2 not starred: read=%v starred=%v", second.IsRead, second.IsStarred)
	}
	if third := getMessage(t, store, ids[2]); third.IsRead || third.IsStarred {
		t.Fatalf("uid 3 changed: read=%v starred=%v", third.IsRead, third.IsStarred)
	}

	// Unread on the server has to win too, so marking mail unread in another client
	// brings it back here rather than being one-way.
	if changed, err = store.ReconcileMailboxFlags(ctx, mailbox.ID, []ports.RemoteFlagState{
		{UID: 1, IsRead: false, Flags: []string{}},
	}); err != nil || changed != 1 {
		t.Fatalf("unread not applied: changed=%d err=%v", changed, err)
	}
	if getMessage(t, store, ids[0]).IsRead {
		t.Fatal("uid 1 still read after remote unread")
	}
}

func TestReconcileMailboxFlagsStoresFlags(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	_, mailbox := seedAccountMailbox(t, store)
	seedMessages(t, store, mailbox.ID, 1)

	if _, err := store.ReconcileMailboxFlags(ctx, mailbox.ID, []ports.RemoteFlagState{
		{UID: 1, IsRead: true, Flags: []string{"\\Seen", "\\Answered"}},
	}); err != nil {
		t.Fatal(err)
	}
	var flags string
	if err := store.db.Raw("SELECT flags_json FROM mailbox_messages WHERE mailbox_id = ? AND uid = 1", mailbox.ID).Scan(&flags).Error; err != nil {
		t.Fatal(err)
	}
	if flags != `["\\Seen","\\Answered"]` {
		t.Fatalf("flags_json = %s", flags)
	}
}

// TestReconcileMailboxFlagsIgnoresUnknownUIDs asserts reconciliation never invents
// rows: a UID that arrived after the local snapshot is the ingest path's job.
func TestReconcileMailboxFlagsIgnoresUnknownUIDs(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	_, mailbox := seedAccountMailbox(t, store)
	seedMessages(t, store, mailbox.ID, 1)

	changed, err := store.ReconcileMailboxFlags(ctx, mailbox.ID, []ports.RemoteFlagState{
		{UID: 99, IsRead: true, Flags: []string{"\\Seen"}},
	})
	if err != nil || changed != 0 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
	page, err := store.ListMessages(ctx, ports.MessageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("message count changed to %d", len(page.Items))
	}
}

func TestReconcileMailboxFlagsEmptyIsNoop(t *testing.T) {
	store := openTestStore(t)
	_, mailbox := seedAccountMailbox(t, store)
	seedMessages(t, store, mailbox.ID, 1)
	if changed, err := store.ReconcileMailboxFlags(context.Background(), mailbox.ID, nil); err != nil || changed != 0 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
}

// TestDeleteMailboxUIDs covers expunge propagation. Mail deleted in another
// client used to stay in the feed permanently, and opening it produced a body
// fetch that could never succeed.
func TestDeleteMailboxUIDs(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	_, mailbox := seedAccountMailbox(t, store)
	ids := seedMessages(t, store, mailbox.ID, 3)

	removed, err := store.DeleteMailboxUIDs(ctx, mailbox.ID, []uint32{2})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	page, err := store.ListMessages(ctx, ports.MessageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("feed holds %d messages after expunge", len(page.Items))
	}
	for _, item := range page.Items {
		if item.ID == ids[1] {
			t.Fatal("expunged message still in feed")
		}
	}
	if _, _, err := store.GetMessage(ctx, ids[1]); err == nil {
		t.Fatal("expunged message row survived")
	}

	// Idempotent: replaying the same stale set must not remove more.
	if removed, err = store.DeleteMailboxUIDs(ctx, mailbox.ID, []uint32{2}); err != nil || removed != 0 {
		t.Fatalf("second pass removed=%d err=%v", removed, err)
	}

	// A UID that was never stored is not an error and changes nothing.
	if removed, err = store.DeleteMailboxUIDs(ctx, mailbox.ID, []uint32{99}); err != nil || removed != 0 {
		t.Fatalf("unknown uid removed=%d err=%v", removed, err)
	}
}

func TestDeleteMailboxUIDsEmptiesMailbox(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	_, mailbox := seedAccountMailbox(t, store)
	seedMessages(t, store, mailbox.ID, 2)

	// Every UID gone is a real state: the folder was emptied on the server.
	removed, err := store.DeleteMailboxUIDs(ctx, mailbox.ID, []uint32{1, 2})
	if err != nil || removed != 2 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	page, err := store.ListMessages(ctx, ports.MessageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("feed still holds %d messages", len(page.Items))
	}
}

// TestDeleteMailboxUIDsKeepsMessagesInOtherMailboxes protects the second half of
// the delete: a message filed in two mailboxes (Gmail labels) is only
// unreachable once the last mapping is gone.
func TestDeleteMailboxUIDsKeepsMessagesInOtherMailboxes(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, inbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	archive := domain.Mailbox{AccountID: account.ID, RemoteName: "Archive", DisplayName: "Archive", Role: "archive", SyncMode: "periodic", UIDValidity: 42, CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertMailbox(ctx, &archive); err != nil {
		t.Fatal(err)
	}
	// UpsertMailbox writes through raw SQL and does not populate the struct ID, so
	// the real id has to be read back before it can be used as a foreign key.
	stored, err := store.GetMailboxByRole(ctx, account.ID, "archive")
	if err != nil {
		t.Fatal(err)
	}

	ids := seedMessages(t, store, inbox.ID, 1)
	message, _, err := store.GetMessage(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOrUpdateMessage(ctx, &message, stored.ID, 7, nil, time.UnixMilli(now)); err != nil {
		t.Fatal(err)
	}

	if _, err := store.DeleteMailboxUIDs(ctx, inbox.ID, []uint32{1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GetMessage(ctx, ids[0]); err != nil {
		t.Fatalf("message still filed in archive was deleted: %v", err)
	}
	if _, err := store.DeleteMailboxUIDs(ctx, stored.ID, []uint32{7}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GetMessage(ctx, ids[0]); err == nil {
		t.Fatal("message with no mailbox mapping survived")
	}
}

// TestUnreadTotalCountsWholeView pins the count to the view rather than the page:
// the badge and the mark-all-read button both read it, and deriving it from the
// returned items reported zero whenever the unread mail sat past the first page.
func TestUnreadTotalCountsWholeView(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	_, mailbox := seedAccountMailbox(t, store)
	seedMessages(t, store, mailbox.ID, 5)

	page, err := store.ListMessages(ctx, ports.MessageFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("page holds %d items", len(page.Items))
	}
	if page.UnreadTotal != 5 {
		t.Fatalf("UnreadTotal = %d, want 5", page.UnreadTotal)
	}

	if _, err := store.ReconcileMailboxFlags(ctx, mailbox.ID, []ports.RemoteFlagState{
		{UID: 1, IsRead: true}, {UID: 2, IsRead: true},
	}); err != nil {
		t.Fatal(err)
	}
	if page, err = store.ListMessages(ctx, ports.MessageFilter{Limit: 2}); err != nil {
		t.Fatal(err)
	}
	if page.UnreadTotal != 3 {
		t.Fatalf("UnreadTotal after two reads = %d, want 3", page.UnreadTotal)
	}
}

// TestUnreadTotalHonoursSearch keeps the count aligned with mark-all-read, which
// applies the same filter: a count that ignored the search term would offer to mark
// mail the button will not touch.
func TestUnreadTotalHonoursSearch(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	_, mailbox := seedAccountMailbox(t, store)
	seedMessages(t, store, mailbox.ID, 3)
	// Subjects are "Message 1".."Message 3"; give one a distinct term.
	ids := seedMessages(t, store, mailbox.ID, 1, "验证码 890122")

	page, err := store.ListMessages(ctx, ports.MessageFilter{Query: "验证码", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != ids[0] {
		t.Fatalf("search returned %d items", len(page.Items))
	}
	if page.UnreadTotal != 1 {
		t.Fatalf("UnreadTotal under search = %d, want 1", page.UnreadTotal)
	}
}

// TestListMessagesOmitsBodies documents the feed projection: a page of full bodies
// is megabytes the list never renders, so the columns are left out and fetched by
// the detail read instead.
func TestListMessagesOmitsBodies(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	_, mailbox := seedAccountMailbox(t, store)
	ids := seedMessages(t, store, mailbox.ID, 1)
	if err := store.UpdateMessageBody(ctx, ids[0], "full body text", "<p>full body</p>", "snippet", nil); err != nil {
		t.Fatal(err)
	}

	page, err := store.ListMessages(ctx, ports.MessageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Items[0].BodyText != "" || page.Items[0].BodyHTML != "" {
		t.Fatalf("feed carried a body: text=%q html=%q", page.Items[0].BodyText, page.Items[0].BodyHTML)
	}
	if page.Items[0].Snippet != "snippet" {
		t.Fatalf("snippet missing from feed: %q", page.Items[0].Snippet)
	}
	full, _, err := store.GetMessage(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if full.BodyText != "full body text" || full.BodyHTML != "<p>full body</p>" {
		t.Fatalf("detail read lost the body: %q / %q", full.BodyText, full.BodyHTML)
	}
}

// seedMessages inserts unread incoming messages at UIDs 1..count. An optional
// subject overrides the generated one for search tests.
func seedMessages(t *testing.T, store *Store, mailboxID int64, count int, subjects ...string) []int64 {
	t.Helper()
	ctx := context.Background()
	var mailbox domain.Mailbox
	if err := store.db.First(&mailbox, mailboxID).Error; err != nil {
		t.Fatal(err)
	}
	var existing int64
	if err := store.db.Raw("SELECT count(*) FROM mailbox_messages WHERE mailbox_id = ?", mailboxID).Scan(&existing).Error; err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, count)
	for index := 0; index < count; index++ {
		subject := "Message " + string(rune('1'+index))
		if index < len(subjects) {
			subject = subjects[index]
		}
		now := time.Now().UnixMilli() + int64(index)
		digest := sha256.Sum256([]byte(subject + string(rune('a'+int(existing)))))
		message := domain.Message{
			AccountID: mailbox.AccountID, Direction: "incoming", DedupeKey: digest[:], Subject: subject,
			Sender: "Sender <sender@example.com>", Recipients: "receiver@example.com",
			FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
			BodyState: "metadata", ReceivedAt: now, CreatedAt: now, UpdatedAt: now,
		}
		created, err := store.CreateOrUpdateMessage(ctx, &message, mailboxID, uint32(existing)+uint32(index)+1, nil, time.UnixMilli(now))
		if err != nil || !created {
			t.Fatalf("seed message %d: created=%v err=%v", index, created, err)
		}
		ids = append(ids, message.ID)
	}
	return ids
}

func getMessage(t *testing.T, store *Store, id int64) domain.Message {
	t.Helper()
	message, _, err := store.GetMessage(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

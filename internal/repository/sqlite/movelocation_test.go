//go:build sqlite_fts5

package sqlite

import (
	"context"
	"testing"
	"time"

	"nexusmail/internal/domain"
)

// MoveMessageLocation is the local half of archiving: the provider has already
// moved the mail, and this is what makes the local view agree. Its three contracts
// are each a user-visible bug when broken, and none of them fails loudly.
//
// The nil-UID case is the one that matters most. QQ and 163 advertise neither MOVE
// nor UIDPLUS on some connections, so a COPY there reports no destination UID —
// the source mapping still has to go, or the message the user archived stays in
// the inbox and comes back on the next sync.

// mailboxNamed creates a second mailbox and returns its id. UpsertMailbox does not
// fill in the id, so it is read back the way seedAccountMailbox does.
func mailboxNamed(t *testing.T, store *Store, accountID int64, remote, role string) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	mailbox := domain.Mailbox{
		AccountID: accountID, RemoteName: remote, DisplayName: remote,
		Role: role, SyncMode: "lazy", UIDValidity: 42, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.UpsertMailbox(ctx, &mailbox); err != nil {
		t.Fatal(err)
	}
	mailboxes, err := store.ListMailboxes(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range mailboxes {
		if candidate.RemoteName == remote {
			return candidate.ID
		}
	}
	t.Fatalf("mailbox %q was not stored", remote)
	return 0
}

// mappingOf reports the uid and flags a message has in one mailbox, and whether a
// mapping exists there at all.
func mappingOf(t *testing.T, store *Store, mailboxID, messageID int64) (uid uint32, flags string, present bool) {
	t.Helper()
	var row struct {
		UID       uint32 `gorm:"column:uid"`
		FlagsJSON string `gorm:"column:flags_json"`
	}
	result := store.db.WithContext(context.Background()).Raw(
		"SELECT uid, flags_json FROM mailbox_messages WHERE mailbox_id = ? AND message_id = ?",
		mailboxID, messageID).Scan(&row)
	if result.Error != nil {
		t.Fatalf("read mapping: %v", result.Error)
	}
	return row.UID, row.FlagsJSON, result.RowsAffected > 0
}

// TestMoveMessageLocationWithoutADestinationUIDStillDropsTheSource covers the
// provider that reports no UID. Leaving the source mapping keeps the archived mail
// in the inbox feed; the next sync then re-establishes it and the archive looks
// like it silently failed.
func TestMoveMessageLocationWithoutADestinationUIDStillDropsTheSource(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, inbox := seedAccountMailbox(t, store)
	archive := mailboxNamed(t, store, account.ID, "Archive", "archive")
	messageID := seedMessage(t, store, account.ID, inbox.ID, 31, "no dest uid", "incoming", false, time.Now().UnixMilli())

	if err := store.MoveMessageLocation(ctx, messageID, inbox.ID, archive, nil); err != nil {
		t.Fatalf("MoveMessageLocation: %v", err)
	}

	if _, _, present := mappingOf(t, store, inbox.ID, messageID); present {
		t.Error("the source mapping survived: the archived message stays in the inbox feed")
	}
	// No destination mapping is correct here rather than a guess at the UID: the
	// next sync of the archive mailbox discovers the real one.
	if _, _, present := mappingOf(t, store, archive, messageID); present {
		t.Error("a destination mapping was invented without a UID from the provider")
	}
}

// TestMoveMessageLocationCarriesFlagsAndDate pins what travels with the message. A
// read message that comes back unread after archiving, or one that jumps to the top
// of the archive because internal_date was lost, are both silent.
func TestMoveMessageLocationCarriesFlagsAndDate(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, inbox := seedAccountMailbox(t, store)
	archive := mailboxNamed(t, store, account.ID, "Archive", "archive")

	received := time.Now().Add(-72 * time.Hour).UnixMilli()
	message := domain.Message{
		AccountID: account.ID, Direction: "incoming", DedupeKey: []byte("flags-carry-key-32-bytes-padding"),
		Subject: "carries flags", Sender: "s@example.com", Recipients: "r@example.com",
		FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
		BodyState: "metadata", IsRead: true, ReceivedAt: received, CreatedAt: received, UpdatedAt: received,
	}
	created, err := ingestOne(ctx, store, &message, inbox.ID, 44, []string{"\\Seen", "\\Flagged"}, time.UnixMilli(received))
	if err != nil || !created {
		t.Fatalf("ingest: created=%v err=%v", created, err)
	}
	_, sourceFlags, present := mappingOf(t, store, inbox.ID, message.ID)
	if !present {
		t.Fatal("the seeded mapping is missing")
	}

	destinationUID := uint32(908)
	if err := store.MoveMessageLocation(ctx, message.ID, inbox.ID, archive, &destinationUID); err != nil {
		t.Fatalf("MoveMessageLocation: %v", err)
	}

	uid, flags, present := mappingOf(t, store, archive, message.ID)
	if !present {
		t.Fatal("no destination mapping was created")
	}
	if uid != destinationUID {
		t.Errorf("destination uid = %d, want %d", uid, destinationUID)
	}
	if flags != sourceFlags {
		t.Errorf("destination flags = %q, want the source's %q: an archived read message would show as unread", flags, sourceFlags)
	}
	var internalDate int64
	if err := store.db.WithContext(ctx).Raw(
		"SELECT internal_date FROM mailbox_messages WHERE mailbox_id = ? AND message_id = ?",
		archive, message.ID).Scan(&internalDate).Error; err != nil {
		t.Fatal(err)
	}
	if internalDate != received {
		t.Errorf("destination internal_date = %d, want %d: the mail would sort as if it just arrived", internalDate, received)
	}
}

// TestMoveMessageLocationOverwritesAStaleMappingOnTheSameUID pins the ON CONFLICT
// clause. A provider reuses UIDs after a mailbox is recreated, so the destination
// slot can already hold a mapping for a different message. Without the upsert the
// insert violates the (mailbox_id, uid) unique index and the whole archive fails.
func TestMoveMessageLocationOverwritesAStaleMappingOnTheSameUID(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, inbox := seedAccountMailbox(t, store)
	archive := mailboxNamed(t, store, account.ID, "Archive", "archive")

	now := time.Now().UnixMilli()
	// A stale occupant of archive UID 500.
	stale := seedMessage(t, store, account.ID, archive, 500, "stale occupant", "incoming", false, now)
	moving := seedMessage(t, store, account.ID, inbox.ID, 12, "moving in", "incoming", false, now)

	destinationUID := uint32(500)
	if err := store.MoveMessageLocation(ctx, moving, inbox.ID, archive, &destinationUID); err != nil {
		t.Fatalf("MoveMessageLocation onto a reused uid: %v", err)
	}

	if _, _, present := mappingOf(t, store, archive, stale); present {
		t.Error("the stale mapping survived on the same uid; two messages claim archive uid 500")
	}
	uid, _, present := mappingOf(t, store, archive, moving)
	if !present {
		t.Fatal("the moved message has no destination mapping")
	}
	if uid != 500 {
		t.Errorf("destination uid = %d, want 500", uid)
	}
}

//go:build sqlite_fts5

package sqlite

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

// TestBatchCreateOrUpdateMessages checks the batch ingest path that the
// supervisor's syncMailbox uses to commit a chunk of fetched messages in one
// transaction. The behaviour must match the single-row CreateOrUpdateMessage:
// each (account_id, dedupe_key) inserts a new message or updates the existing
// one, and the mailbox_messages row is upserted. FTS5 triggers must fire and
// the messages_fts index must stay in sync with the row counts.
func TestBatchCreateOrUpdateMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	acc := &domain.Account{Email: "x@x", DisplayName: "x", Provider: "qq", AuthType: "password", Username: "x@x", IMAPHost: "h", IMAPPort: 993, IMAPTLSMode: "implicit", SMTPHost: "h", SMTPPort: 465, SMTPTLSMode: "implicit", SecretCiphertext: []byte("k"), Status: "disconnected", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	mb := &domain.Mailbox{AccountID: acc.ID, RemoteName: "INBOX", DisplayName: "INBOX", Role: "inbox", SyncMode: "realtime", UIDValidity: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertMailbox(ctx, mb); err != nil {
		t.Fatal(err)
	}
	// UpsertMailbox uses raw SQL and does not return the row id, so the
	// mailbox id is still 0; refresh it from the store before any further
	// work that depends on a foreign key to mailboxes.id.
	loaded, err := store.GetMailboxByRole(ctx, acc.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	mb = &loaded

	// Build a batch of 100 messages with distinct dedupe keys, plus one
	// collision: item 7 reuses item 0's dedupe_key with a different subject
	// to exercise the update branch.
	const n = 100
	items := make([]ports.MessageInput, n)
	for i := 0; i < n; i++ {
		key := sha256.Sum256([]byte{byte(i), byte(i >> 8)})
		msg := &domain.Message{
			AccountID: acc.ID, Direction: "incoming", DedupeKey: key[:],
			Subject: "subject " + itoa(i), Sender: "s@x", Recipients: "r@x",
			FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
			BodyState: "metadata", ReceivedAt: now + int64(i), CreatedAt: now, UpdatedAt: now,
		}
		items[i] = ports.MessageInput{
			Message:      msg,
			MailboxID:    mb.ID,
			UID:          uint32(i + 1),
			Flags:        []string{"\\Seen"},
			InternalDate: time.UnixMilli(now + int64(i)),
		}
	}
	// Replace item 7's message with a clone of item 0's dedupe_key, but a
	// different subject, so the batch must detect the existing row and
	// update instead of insert.
	items[7].Message.DedupeKey = items[0].Message.DedupeKey
	items[7].Message.Subject = "subject-7-update"

	ids, created, err := store.BatchCreateOrUpdateMessages(ctx, items)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != n || len(created) != n {
		t.Fatalf("result length = (%d, %d), want (%d, %d)", len(ids), len(created), n, n)
	}
	inserted := 0
	for _, c := range created {
		if c {
			inserted++
		}
	}
	if inserted != n-1 {
		t.Errorf("created flags: %d inserts, want %d (item 7 was an update)", inserted, n-1)
	}
	if ids[7] != ids[0] {
		t.Errorf("item 7 id = %d, want %d (same row as item 0)", ids[7], ids[0])
	}
	for i, id := range ids {
		if id == 0 {
			t.Errorf("item %d: id == 0", i)
		}
	}

	// Run the batch again with a fresh subject for item 0 — every row should
	// be an update this time, and the FTS index must still have exactly n-1
	// rows (the dedupe-collision pair shares one row).
	items[0].Message.Subject = "subject-0-update"
	if _, _, err := store.BatchCreateOrUpdateMessages(ctx, items); err != nil {
		t.Fatal(err)
	}
	var msgCount, ftsCount int
	if err := store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE account_id = ?", acc.ID).Scan(&msgCount); err != nil {
		t.Fatal(err)
	}
	if err := store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM message_fts").Scan(&ftsCount); err != nil {
		t.Fatal(err)
	}
	if msgCount != n-1 {
		t.Errorf("messages table = %d rows, want %d", msgCount, n-1)
	}
	if ftsCount != n-1 {
		t.Errorf("message_fts index = %d rows, want %d (FTS5 trigger kept in sync)", ftsCount, n-1)
	}

	// Attachments: the batch should accept a per-row attachment list and
	// patch the message id before the upsert.
	atts := []domain.Attachment{
		{PartID: "1", Filename: "a.txt", ContentType: "text/plain", Disposition: "attachment", FetchState: "metadata", CreatedAt: now, UpdatedAt: now},
		{PartID: "2", Filename: "b.png", ContentType: "image/png", Disposition: "inline", FetchState: "metadata", CreatedAt: now, UpdatedAt: now},
	}
	atts[0].MessageID = ids[0]
	atts[1].MessageID = ids[0]
	if err := store.BatchUpsertAttachments(ctx, atts); err != nil {
		t.Fatal(err)
	}
	var attCount int
	if err := store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM attachments WHERE message_id = ?", ids[0]).Scan(&attCount); err != nil {
		t.Fatal(err)
	}
	if attCount != 2 {
		t.Errorf("attachments for message %d = %d, want 2", ids[0], attCount)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

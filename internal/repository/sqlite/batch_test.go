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
	// be an update this time, and the FTS index must still describe exactly
	// the rows the table holds (the dedupe-collision pair shares one row).
	items[0].Message.Subject = "subject-0-update"
	if _, _, err := store.BatchCreateOrUpdateMessages(ctx, items); err != nil {
		t.Fatal(err)
	}
	var msgCount int
	if err := store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE account_id = ?", acc.ID).Scan(&msgCount); err != nil {
		t.Fatal(err)
	}
	if msgCount != n-1 {
		t.Errorf("messages table = %d rows, want %d", msgCount, n-1)
	}
	// count(*) on message_fts cannot report index health: the table is
	// content='messages', so an unqualified count is answered from the content
	// table and equals msgCount whatever the index holds. Dropping all three
	// triggers, or emptying the index with 'delete-all', leaves that count at the
	// full message total — the exact trap 000003_rebuild_message_fts documents,
	// where search had silently lost the whole backlog while the count read full.
	//
	// integrity-check walks the index against the content table and errors on a
	// mismatch; a MATCH proves the terms the triggers wrote are actually
	// retrievable.
	if _, err := store.sqlDB.ExecContext(ctx, "INSERT INTO message_fts(message_fts) VALUES('integrity-check')"); err != nil {
		t.Fatalf("FTS integrity-check after batch ingest: %v", err)
	}
	var indexed int
	if err := store.sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM messages JOIN message_fts ON message_fts.rowid = messages.id
		 WHERE messages.account_id = ? AND message_fts MATCH ?`, acc.ID, `"subject"*`).Scan(&indexed); err != nil {
		t.Fatalf("FTS match after batch ingest: %v", err)
	}
	if indexed != n-1 {
		t.Errorf("message_fts matched %d of %d ingested subjects; the triggers are not keeping the index in sync", indexed, n-1)
	}
	// The update branch has to reindex, not just leave the old term behind.
	//
	// The surviving subject on the collision row is item 7's: items 0 and 7 share a
	// dedupe_key, so both updates land on the same row and the later one in the
	// batch wins. Item 0's own new subject is therefore expected to be absent, and
	// asserting that too is what proves the delete half of the AFTER UPDATE trigger
	// ran rather than leaving the superseded term retrievable.
	matches := func(term string) int {
		t.Helper()
		var count int
		if err := store.sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM messages JOIN message_fts ON message_fts.rowid = messages.id
			 WHERE messages.account_id = ? AND message_fts MATCH ?`, acc.ID, term).Scan(&count); err != nil {
			t.Fatalf("FTS match %s: %v", term, err)
		}
		return count
	}
	if got := matches(`"subject-7-update"`); got != 1 {
		t.Errorf("updated subject matched %d rows, want 1 (AFTER UPDATE trigger did not reindex)", got)
	}
	if got := matches(`"subject-0-update"`); got != 0 {
		t.Errorf("superseded subject still matched %d rows, want 0 (stale term left in the index)", got)
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

// TestBatchDedupesAcrossCalls covers the case the in-batch collision test cannot
// reach: a message that already exists from an earlier call.
//
// GORM expands a slice bound to a `?` that sits immediately after `(` into one
// placeholder per element, so the first key of the hand-built `IN (?,?…)` was
// compared as 32 separate integers and never matched. Every batch therefore
// missed the existing row for its first item, and the INSERT that followed hit the
// (account_id, dedupe_key) unique index and rolled the whole batch back — which on
// a real account means the mailbox stops ingesting. Items 2..N matched, so nothing
// showed up in a test that only collided inside one batch.
func TestBatchDedupesAcrossCalls(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()

	// Fresh struct each time: a real second sync builds the message from the
	// provider's response and has no id from the earlier pass.
	build := func(subject string) *domain.Message {
		digest := sha256.Sum256([]byte(subject))
		return &domain.Message{
			AccountID: account.ID, Direction: "incoming", DedupeKey: digest[:], Subject: subject,
			Sender: "Sender <sender@example.com>", Recipients: "receiver@example.com",
			FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
			BodyState: "metadata", ReceivedAt: now, CreatedAt: now, UpdatedAt: now,
		}
	}

	first, created, err := store.BatchCreateOrUpdateMessages(ctx, []ports.MessageInput{
		{Message: build("across-calls"), MailboxID: mailbox.ID, UID: 1, InternalDate: time.UnixMilli(now)},
	})
	if err != nil || !created[0] {
		t.Fatalf("first ingest: created=%v err=%v", created, err)
	}

	// A single-item batch is the shape the 5s inbox probe produces, and it is the
	// one where the broken key was also the only key.
	second, created, err := store.BatchCreateOrUpdateMessages(ctx, []ports.MessageInput{
		{Message: build("across-calls"), MailboxID: mailbox.ID, UID: 2, InternalDate: time.UnixMilli(now)},
	})
	if err != nil {
		t.Fatalf("re-ingesting an existing message failed the batch: %v", err)
	}
	if created[0] {
		t.Error("re-ingest reported an insert, want an update of the existing row")
	}
	if second[0] != first[0] {
		t.Errorf("re-ingest used row %d, want the existing row %d", second[0], first[0])
	}

	// The same holds for the first item of a multi-item batch, which is where the
	// expansion bug lived.
	ids, created, err := store.BatchCreateOrUpdateMessages(ctx, []ports.MessageInput{
		{Message: build("across-calls"), MailboxID: mailbox.ID, UID: 3, InternalDate: time.UnixMilli(now)},
		{Message: build("brand-new"), MailboxID: mailbox.ID, UID: 4, InternalDate: time.UnixMilli(now)},
	})
	if err != nil {
		t.Fatalf("mixed batch failed: %v", err)
	}
	if created[0] || !created[1] {
		t.Errorf("created = %v, want [false true]", created)
	}
	if ids[0] != first[0] {
		t.Errorf("first item used row %d, want %d", ids[0], first[0])
	}

	var rows int
	if err := store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE account_id = ?", account.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("messages table = %d rows, want 2", rows)
	}
}

// TestBatchPersistsHasAttachments covers the column the feed reads to draw the
// paperclip. Nothing assigned it, so it sat at the schema default of 0 for every
// message ever synced and the indicator never appeared; the update branch has to
// carry it too, or a re-sync of mail ingested before ingest set the column leaves
// those rows wrong forever.
func TestBatchPersistsHasAttachments(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()

	build := func(subject string, hasAttachments bool) *domain.Message {
		digest := sha256.Sum256([]byte(subject))
		return &domain.Message{
			AccountID: account.ID, Direction: "incoming", DedupeKey: digest[:], Subject: subject,
			Sender: "s@x", Recipients: "r@x",
			FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
			BodyState: "metadata", HasAttachments: hasAttachments,
			ReceivedAt: now, CreatedAt: now, UpdatedAt: now,
		}
	}

	// Insert branch.
	withAttachment := build("carries a file", true)
	plain := build("no files", false)
	if _, _, err := store.BatchCreateOrUpdateMessages(ctx, []ports.MessageInput{
		{Message: withAttachment, MailboxID: mailbox.ID, UID: 1, InternalDate: time.UnixMilli(now)},
		{Message: plain, MailboxID: mailbox.ID, UID: 2, InternalDate: time.UnixMilli(now)},
	}); err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.GetMessage(ctx, withAttachment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.HasAttachments {
		t.Error("ingest stored has_attachments = false for a message that carries one")
	}
	if other, _, err := store.GetMessage(ctx, plain.ID); err != nil || other.HasAttachments {
		t.Errorf("message with no attachments stored has_attachments = %v err=%v", other.HasAttachments, err)
	}

	// Update branch: the row exists with 0, and a re-sync that now knows about the
	// attachment has to correct it. This is the half that repairs history.
	if _, _, err := store.BatchCreateOrUpdateMessages(ctx, []ports.MessageInput{
		{Message: build("no files", true), MailboxID: mailbox.ID, UID: 2, InternalDate: time.UnixMilli(now)},
	}); err != nil {
		t.Fatal(err)
	}
	corrected, _, err := store.GetMessage(ctx, plain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !corrected.HasAttachments {
		t.Error("re-sync did not correct has_attachments on an existing row")
	}

	// And the reverse: a provider that no longer reports an attachment part must be
	// able to clear it, exactly as is_read and is_starred behave on this path.
	if _, _, err := store.BatchCreateOrUpdateMessages(ctx, []ports.MessageInput{
		{Message: build("no files", false), MailboxID: mailbox.ID, UID: 2, InternalDate: time.UnixMilli(now)},
	}); err != nil {
		t.Fatal(err)
	}
	if cleared, _, err := store.GetMessage(ctx, plain.ID); err != nil || cleared.HasAttachments {
		t.Errorf("re-sync left has_attachments = %v err=%v, want false", cleared.HasAttachments, err)
	}

	// The feed projection has to carry the column, or the client never sees it
	// however correct the row is.
	page, err := store.ListMessages(ctx, ports.MessageFilter{AccountID: &account.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range page.Items {
		if item.ID == withAttachment.ID {
			found = true
			if !item.HasAttachments {
				t.Error("feed returned has_attachments = false for a message that has one")
			}
		}
	}
	if !found {
		t.Fatalf("feed did not return message %d", withAttachment.ID)
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

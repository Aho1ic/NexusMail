//go:build sqlite_fts5

package sqlite

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

func TestMigrationFTSAndKeysetPagination(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)

	for index, subject := range []string{"NexusMail quarterly report", "Short note", "Third message"} {
		now := time.Now().UnixMilli() + int64(index)
		digest := sha256.Sum256([]byte(subject))
		message := domain.Message{
			AccountID: account.ID, Direction: "incoming", DedupeKey: digest[:], Subject: subject,
			Sender: "Sender <sender@example.com>", Recipients: "receiver@example.com", FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
			Snippet: "", BodyText: "", BodyHTML: "", BodyState: "metadata", ReceivedAt: now,
			CreatedAt: now, UpdatedAt: now,
		}
		created, err := store.CreateOrUpdateMessage(ctx, &message, mailbox.ID, uint32(index+1), nil, time.UnixMilli(now))
		if err != nil || !created {
			t.Fatalf("create message %d: created=%v err=%v", index, created, err)
		}
		if index == 1 {
			if err := store.UpdateMessageBody(ctx, message.ID, "正文含有红烧牛肉", "", "正文", nil); err != nil {
				t.Fatal(err)
			}
		}
	}

	page, err := store.ListMessages(ctx, ports.MessageFilter{Query: "Nexus", Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Subject != "NexusMail quarterly report" {
		t.Fatalf("FTS result = %#v, err=%v", page, err)
	}
	page, err = store.ListMessages(ctx, ports.MessageFilter{Query: "红烧", Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Subject != "Short note" {
		t.Fatalf("CJK FTS result = %#v, err=%v", page, err)
	}
	page, err = store.ListMessages(ctx, ports.MessageFilter{Query: "红", Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("short LIKE result = %#v, err=%v", page, err)
	}

	first, err := store.ListMessages(ctx, ports.MessageFilter{Limit: 2})
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %#v, err=%v", first, err)
	}
	second, err := store.ListMessages(ctx, ports.MessageFilter{Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID || second.Items[0].ID == first.Items[1].ID {
		t.Fatalf("second page = %#v, err=%v", second, err)
	}
	archive := domain.Mailbox{AccountID: account.ID, RemoteName: "Archive", DisplayName: "Archive", Role: "archive", SyncMode: "periodic", UIDValidity: 43, CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli()}
	if err := store.UpsertMailbox(ctx, &archive); err != nil {
		t.Fatal(err)
	}
	mailboxes, err := store.ListMailboxes(ctx, account.ID)
	if err != nil || len(mailboxes) != 2 {
		t.Fatalf("archive mailbox: %#v err=%v", mailboxes, err)
	}
	for _, item := range mailboxes {
		if item.Role == "archive" {
			archive = item
		}
	}
	destinationUID := uint32(99)
	if err := store.MoveMessageLocation(ctx, first.Items[0].ID, mailbox.ID, archive.ID, &destinationUID); err != nil {
		t.Fatal(err)
	}
	inboxPage, err := store.ListMessages(ctx, ports.MessageFilter{Folder: "inbox", Limit: 10})
	if err != nil || len(inboxPage.Items) != 2 {
		t.Fatalf("inbox after archive = %#v err=%v", inboxPage, err)
	}
	archivePage, err := store.ListMessages(ctx, ports.MessageFilter{Folder: "archive", Limit: 10})
	if err != nil || len(archivePage.Items) != 1 || archivePage.Items[0].ID != first.Items[0].ID {
		t.Fatalf("archive after move = %#v err=%v", archivePage, err)
	}
	if _, err := store.sqlDB.ExecContext(ctx, "INSERT INTO message_fts(message_fts) VALUES('integrity-check')"); err != nil {
		t.Fatalf("FTS integrity-check: %v", err)
	}
}

func TestDraftRevisionConflictAndSessionExpiry(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, _ := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	draft := domain.Draft{
		AccountID: account.ID, RFCMessageID: "<draft@nexusmail.local>", Revision: 1,
		ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", Status: "draft", RemoteSyncState: "dirty",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateDraft(ctx, &draft); err != nil {
		t.Fatal(err)
	}
	draft.Subject = "revision two"
	draft.UpdatedAt++
	if err := store.UpdateDraft(ctx, &draft, 1); err != nil || draft.Revision != 2 {
		t.Fatalf("update draft: revision=%d err=%v", draft.Revision, err)
	}
	if err := store.UpdateDraft(ctx, &draft, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update error = %v", err)
	}

	token := []byte("token hash")
	csrf := []byte("csrf hash")
	if err := store.CreateSession(ctx, token, csrf, now+1000, now+2000); err != nil {
		t.Fatal(err)
	}
	got, valid, err := store.ValidateSession(ctx, token, now)
	if err != nil || !valid || string(got) != string(csrf) {
		t.Fatalf("valid session: got=%q valid=%v err=%v", got, valid, err)
	}
	_, valid, err = store.ValidateSession(ctx, token, now+3000)
	if err != nil || valid {
		t.Fatalf("expired session valid=%v err=%v", valid, err)
	}
}

func TestRemoteDraftReconciliation(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	uidOne, validity := uint32(10), uint32(42)
	remoteTime := now - 30_000
	local := domain.Draft{
		AccountID: account.ID, RFCMessageID: "<shared@example.com>", Revision: 1,
		ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", Subject: "local", BodyText: "local body",
		Status: "draft", RemoteSyncState: "dirty", RemoteMailboxID: &mailbox.ID,
		RemoteUIDValidity: &validity, RemoteUID: &uidOne, RemoteUpdatedAt: &remoteTime,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateDraft(ctx, &local); err != nil {
		t.Fatal(err)
	}
	uidTwo := uint32(11)
	older := local
	older.ID, older.Subject, older.BodyText = 0, "older remote", "remote body"
	older.RemoteUID = &uidTwo
	older.RemoteUpdatedAt = &remoteTime
	older.UpdatedAt = remoteTime
	kept, changed, err := store.ReconcileRemoteDraft(ctx, &older)
	if err != nil || changed || kept.Subject != "local" {
		t.Fatalf("local newer result=%#v changed=%v err=%v", kept, changed, err)
	}

	tieTime := now + 2_000
	tie := older
	tie.RemoteUID = ptr32(12)
	tie.RemoteUpdatedAt, tie.UpdatedAt = &tieTime, tieTime
	tie.Subject = "remote tie"
	conflict, changed, err := store.ReconcileRemoteDraft(ctx, &tie)
	if err != nil || !changed || conflict.ConflictOfID == nil || conflict.RemoteSyncState != "conflict" {
		t.Fatalf("conflict result=%#v changed=%v err=%v", conflict, changed, err)
	}
}

func ptr32(value uint32) *uint32 { return &value }

// TestRemoteDraftRewrittenMessageID covers providers that drop or rewrite
// Message-Id on APPEND: the draft we uploaded comes back under a synthetic RFC
// id, and matching on that alone used to insert a second row holding the same
// remote UID, which the unique index rejected and which failed every later sync
// of the account.
func TestRemoteDraftRewrittenMessageID(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	uid, validity := uint32(9), mailbox.UIDValidity
	local := domain.Draft{
		AccountID: account.ID, RFCMessageID: "<6b2608c5@nexusmail.local>", Revision: 1,
		ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", Subject: "uploaded", BodyText: "body",
		Status: "draft", RemoteSyncState: "synced", RemoteMailboxID: &mailbox.ID,
		RemoteUIDValidity: &validity, RemoteUID: &uid, RemoteUpdatedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateDraft(ctx, &local); err != nil {
		t.Fatal(err)
	}

	remote := local
	remote.ID, remote.RFCMessageID = 0, "<remote-1-4-42-9@nexusmail.local>"
	matched, changed, err := store.ReconcileRemoteDraft(ctx, &remote)
	if err != nil {
		t.Fatalf("reconcile rewritten message id: %v", err)
	}
	if matched.ID != local.ID || changed {
		t.Fatalf("want existing draft %d unchanged, got id=%d changed=%v", local.ID, matched.ID, changed)
	}
	drafts, err := store.ListDrafts(ctx, "")
	if err != nil || len(drafts) != 1 {
		t.Fatalf("want a single draft row, got %d err=%v", len(drafts), err)
	}
}

// TestRemoteDraftConflictCopyReleasesRemoteUID pins that a conflict copy does
// not claim the remote UID still held by the dirty local draft, since the
// unique index maps that UID to exactly one row.
func TestRemoteDraftConflictCopyReleasesRemoteUID(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	uid, validity := uint32(9), mailbox.UIDValidity
	local := domain.Draft{
		AccountID: account.ID, RFCMessageID: "<shared@example.com>", Revision: 1,
		ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", Subject: "local edit", BodyText: "local body",
		Status: "draft", RemoteSyncState: "dirty", RemoteMailboxID: &mailbox.ID,
		RemoteUIDValidity: &validity, RemoteUID: &uid, RemoteUpdatedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateDraft(ctx, &local); err != nil {
		t.Fatal(err)
	}

	// Same UID, a new UIDVALIDITY, and a timestamp inside the clock-skew window.
	remote := local
	remote.ID, remote.Subject = 0, "remote edit"
	remote.RemoteUIDValidity = ptr32(validity + 1)
	copyTime := now + 1_000
	remote.RemoteUpdatedAt, remote.UpdatedAt = &copyTime, copyTime
	conflict, changed, err := store.ReconcileRemoteDraft(ctx, &remote)
	if err != nil {
		t.Fatalf("reconcile conflict copy: %v", err)
	}
	if !changed || conflict.ConflictOfID == nil || *conflict.ConflictOfID != local.ID {
		t.Fatalf("want conflict copy of draft %d, got %#v changed=%v", local.ID, conflict, changed)
	}
	if conflict.RemoteUID != nil || conflict.RemoteMailboxID != nil || conflict.RemoteUIDValidity != nil {
		t.Fatalf("conflict copy must not claim the remote UID, got %#v", conflict)
	}
	kept, _, err := store.GetDraft(ctx, local.ID)
	if err != nil || kept.RemoteUID == nil || *kept.RemoteUID != uid {
		t.Fatalf("local draft must keep the remote UID, got %#v err=%v", kept, err)
	}
}

// TestUnreadMessageIDsScopeAndBulkUpdate pins the two queries behind "mark the
// current view read": the view must resolve to exactly the unread incoming mail
// the feed shows, and one patch must reach every id in a single transaction.
func TestUnreadMessageIDsScopeAndBulkUpdate(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, inbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	archive := domain.Mailbox{AccountID: account.ID, RemoteName: "Archive", DisplayName: "Archive", Role: "archive", SyncMode: "lazy", UIDValidity: 42, CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertMailbox(ctx, &archive); err != nil {
		t.Fatal(err)
	}
	mailboxes, err := store.ListMailboxes(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mailbox := range mailboxes {
		if mailbox.Role == "archive" {
			archive = mailbox
		}
	}

	older := seedMessage(t, store, account.ID, inbox.ID, 1, "older unread", "incoming", false, now)
	newer := seedMessage(t, store, account.ID, inbox.ID, 2, "newer unread", "incoming", false, now+1_000)
	alreadyRead := seedMessage(t, store, account.ID, inbox.ID, 3, "already read", "incoming", true, now+2_000)
	outgoing := seedMessage(t, store, account.ID, inbox.ID, 4, "sent copy", "outgoing", false, now+3_000)
	archived := seedMessage(t, store, account.ID, archive.ID, 1, "archived unread", "incoming", false, now+4_000)
	// The same message filed in two mailboxes must still be counted once.
	linkMessage(t, store, account.ID, archive.ID, 2, "newer unread", "incoming", false, now+1_000)

	all, err := store.UnreadMessageIDs(ctx, ports.MessageFilter{}, 0)
	if err != nil {
		t.Fatalf("unread ids: %v", err)
	}
	// Newest first, read mail and the outgoing copy excluded, no duplicate for the
	// message linked twice.
	if want := []int64{archived, newer, older}; !equalIDs(all, want) {
		t.Fatalf("unscoped unread = %v, want %v (read=%d outgoing=%d)", all, want, alreadyRead, outgoing)
	}

	inboxOnly, err := store.UnreadMessageIDs(ctx, ports.MessageFilter{Folder: "inbox"}, 0)
	if err != nil {
		t.Fatalf("unread ids for inbox: %v", err)
	}
	if want := []int64{newer, older}; !equalIDs(inboxOnly, want) {
		t.Fatalf("inbox unread = %v, want %v", inboxOnly, want)
	}

	byMailbox, err := store.UnreadMessageIDs(ctx, ports.MessageFilter{MailboxID: &archive.ID}, 0)
	if err != nil {
		t.Fatalf("unread ids for mailbox: %v", err)
	}
	if want := []int64{archived, newer}; !equalIDs(byMailbox, want) {
		t.Fatalf("archive unread = %v, want %v", byMailbox, want)
	}

	otherAccount := account.ID + 1_000
	foreign, err := store.UnreadMessageIDs(ctx, ports.MessageFilter{AccountID: &otherAccount}, 0)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("unread ids for another account = %v err=%v, want empty", foreign, err)
	}

	// The search box is part of the view: marking a filtered list read must leave
	// the unread mail the filter hides alone.
	searched, err := store.UnreadMessageIDs(ctx, ports.MessageFilter{Folder: "inbox", Query: "older"}, 0)
	if err != nil {
		t.Fatalf("unread ids for a search: %v", err)
	}
	if want := []int64{older}; !equalIDs(searched, want) {
		t.Fatalf("searched unread = %v, want %v", searched, want)
	}
	// Terms under three runes fall back to LIKE rather than the trigram index.
	short, err := store.UnreadMessageIDs(ctx, ports.MessageFilter{Query: "ed"}, 0)
	if err != nil {
		t.Fatalf("unread ids for a short search: %v", err)
	}
	if want := []int64{archived}; !equalIDs(short, want) {
		t.Fatalf("short-search unread = %v, want %v", short, want)
	}

	// A caller-supplied limit truncates from the newest end; a limit above the
	// hard cap is clamped instead of trusted.
	limited, err := store.UnreadMessageIDs(ctx, ports.MessageFilter{}, 1)
	if err != nil || !equalIDs(limited, []int64{archived}) {
		t.Fatalf("limit 1 = %v err=%v, want [%d]", limited, err, archived)
	}
	oversized, err := store.UnreadMessageIDs(ctx, ports.MessageFilter{}, maxBulkMessageIDs*10)
	if err != nil || len(oversized) != 3 {
		t.Fatalf("oversized limit = %v err=%v, want 3 ids", oversized, err)
	}

	value := true
	if err := store.UpdateMessages(ctx, all, ports.MessagePatch{IsRead: &value}); err != nil {
		t.Fatalf("bulk update: %v", err)
	}
	for _, id := range all {
		message, err := store.messageByID(ctx, id)
		if err != nil || !message.IsRead {
			t.Fatalf("message %d is_read=%v err=%v, want read", id, message.IsRead, err)
		}
	}
	if remaining, err := store.UnreadMessageIDs(ctx, ports.MessageFilter{}, 0); err != nil || len(remaining) != 0 {
		t.Fatalf("unread after bulk update = %v err=%v, want empty", remaining, err)
	}
	// The outgoing message was never in scope, so the bulk patch must not have
	// touched it.
	if message, err := store.messageByID(ctx, outgoing); err != nil || message.IsRead {
		t.Fatalf("outgoing message is_read=%v err=%v, want untouched", message.IsRead, err)
	}
	if err := store.UpdateMessages(ctx, nil, ports.MessagePatch{IsRead: &value}); err != nil {
		t.Fatalf("empty bulk update must be a no-op, got %v", err)
	}
}

// TestMessageLocationsBatch pins that the batch resolver agrees with the single
// lookup, including the inbox-first choice for a message filed twice, and that a
// message with no remote mapping is skipped rather than failing the batch.
func TestMessageLocationsBatch(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, inbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	archive := domain.Mailbox{AccountID: account.ID, RemoteName: "Archive", DisplayName: "Archive", Role: "archive", SyncMode: "lazy", UIDValidity: 42, CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertMailbox(ctx, &archive); err != nil {
		t.Fatal(err)
	}
	mailboxes, err := store.ListMailboxes(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mailbox := range mailboxes {
		if mailbox.Role == "archive" {
			archive = mailbox
		}
	}

	first := seedMessage(t, store, account.ID, inbox.ID, 11, "first", "incoming", false, now)
	second := seedMessage(t, store, account.ID, archive.ID, 12, "second", "incoming", false, now+1_000)
	// Filed in the archive first, then the inbox: the inbox copy must win.
	linkMessage(t, store, account.ID, inbox.ID, 13, "second", "incoming", false, now+1_000)

	locations, err := store.MessageLocations(ctx, []int64{first, second, second + 9_999})
	if err != nil {
		t.Fatalf("message locations: %v", err)
	}
	if len(locations) != 2 {
		t.Fatalf("locations = %#v, want 2 entries (unmapped id skipped)", locations)
	}
	byID := make(map[int64]ports.MessageLocation, len(locations))
	for _, location := range locations {
		byID[location.MessageID] = location
	}
	for _, id := range []int64{first, second} {
		single, err := store.MessageLocation(ctx, id)
		if err != nil {
			t.Fatalf("single location for %d: %v", id, err)
		}
		batch, ok := byID[id]
		if !ok {
			t.Fatalf("message %d missing from batch result %#v", id, locations)
		}
		if batch.Mailbox.ID != single.Mailbox.ID || batch.UID != single.UID || batch.Account.ID != single.Account.ID {
			t.Fatalf("batch location %#v disagrees with single %#v", batch, single)
		}
	}
	if byID[second].Mailbox.Role != "inbox" {
		t.Fatalf("message in two mailboxes resolved to %q, want inbox", byID[second].Mailbox.Role)
	}
	if empty, err := store.MessageLocations(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty batch = %#v err=%v, want empty", empty, err)
	}
}

func seedMessage(t *testing.T, store *Store, accountID, mailboxID int64, uid uint32, subject, direction string, read bool, receivedAt int64) int64 {
	t.Helper()
	id, created := upsertMessage(t, store, accountID, mailboxID, uid, subject, direction, read, receivedAt)
	if !created {
		t.Fatalf("seed message %q already existed", subject)
	}
	return id
}

// linkMessage files an existing message into a second mailbox by reusing its
// dedupe key, which is how a real sync sees the same mail in two folders.
func linkMessage(t *testing.T, store *Store, accountID, mailboxID int64, uid uint32, subject, direction string, read bool, receivedAt int64) {
	t.Helper()
	if _, created := upsertMessage(t, store, accountID, mailboxID, uid, subject, direction, read, receivedAt); created {
		t.Fatalf("link of %q created a second row instead of reusing the dedupe key", subject)
	}
}

func upsertMessage(t *testing.T, store *Store, accountID, mailboxID int64, uid uint32, subject, direction string, read bool, receivedAt int64) (int64, bool) {
	t.Helper()
	digest := sha256.Sum256([]byte(subject))
	message := domain.Message{
		AccountID: accountID, Direction: direction, DedupeKey: digest[:], Subject: subject,
		Sender: "Sender <sender@example.com>", Recipients: "receiver@example.com",
		FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
		BodyState: "metadata", IsRead: read, ReceivedAt: receivedAt, CreatedAt: receivedAt, UpdatedAt: receivedAt,
	}
	created, err := store.CreateOrUpdateMessage(context.Background(), &message, mailboxID, uid, nil, time.UnixMilli(receivedAt))
	if err != nil {
		t.Fatalf("upsert message %q: %v", subject, err)
	}
	return message.ID, created
}

func equalIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedAccountMailbox(t *testing.T, store *Store) (domain.Account, domain.Mailbox) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	account := domain.Account{
		Email: "mail@example.com", Provider: "qq", AuthType: "password", Username: "mail@example.com",
		IMAPHost: "imap.qq.com", IMAPPort: 993, IMAPTLSMode: "implicit", SMTPHost: "smtp.qq.com", SMTPPort: 465, SMTPTLSMode: "implicit",
		SecretCiphertext: []byte("encrypted"), Status: "disconnected", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateAccount(ctx, &account); err != nil {
		t.Fatal(err)
	}
	mailbox := domain.Mailbox{AccountID: account.ID, RemoteName: "INBOX", DisplayName: "Inbox", Role: "inbox", SyncMode: "realtime", UIDValidity: 42, CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertMailbox(ctx, &mailbox); err != nil {
		t.Fatal(err)
	}
	mailboxes, err := store.ListMailboxes(ctx, account.ID)
	if err != nil || len(mailboxes) != 1 {
		t.Fatalf("list mailboxes: %#v err=%v", mailboxes, err)
	}
	return account, mailboxes[0]
}

// TestFTSDeleteTriggerShrinksIndex pins the AFTER DELETE trigger on
// message_fts. The CLAUDE.md warning is that bypassing the trigger paths
// (writing to messages through anything other than the repo) silently
// desyncs the index. The repo-driven path is DeleteMailboxUIDs, which
// removes the mailbox_messages row and then drops any message that is left
// with no mapping. After the delete the FTS index must no longer match the
// dropped subject, otherwise the feed would still show the row.
func TestFTSDeleteTriggerShrinksIndex(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)

	for index, subject := range []string{"fts-delete-anchor", "fts-delete-other"} {
		now := time.Now().UnixMilli() + int64(index)
		digest := sha256.Sum256([]byte(subject))
		message := domain.Message{
			AccountID: account.ID, Direction: "incoming", DedupeKey: digest[:], Subject: subject,
			Sender: "s@x", Recipients: "r@x", FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
			BodyState: "metadata", ReceivedAt: now, CreatedAt: now, UpdatedAt: now,
		}
		created, err := store.CreateOrUpdateMessage(ctx, &message, mailbox.ID, uint32(index+1), nil, time.UnixMilli(now))
		if err != nil || !created {
			t.Fatalf("seed %d: created=%v err=%v", index, created, err)
		}
	}

	// Baseline: searching for the anchor subject must hit the row.
	page, err := store.ListMessages(ctx, ports.MessageFilter{Query: "fts-delete-anchor", Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("baseline FTS = %#v err=%v", page, err)
	}
	anchorID := page.Items[0].ID

	// Drop the only mailbox_messages row for the anchor, then DeleteMailboxUIDs
	// cascades and removes the message. The DELETE trigger on messages must
	// shrink the FTS index; if it does not, this query will still return the
	// row that the user can no longer open.
	stored, err := store.ListMailboxUIDs(ctx, mailbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteMailboxUIDs(ctx, mailbox.ID, []uint32{stored[0]}); err != nil {
		t.Fatal(err)
	}
	page, err = store.ListMessages(ctx, ports.MessageFilter{Query: "fts-delete-anchor", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Errorf("FTS index still matches the deleted subject: %d rows", len(page.Items))
	}
	// The non-deleted message must still be searchable.
	page, err = store.ListMessages(ctx, ports.MessageFilter{Query: "fts-delete-other", Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Errorf("FTS index over-shrank: %#v err=%v", page, err)
	}
	_ = anchorID
}

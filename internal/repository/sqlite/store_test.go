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

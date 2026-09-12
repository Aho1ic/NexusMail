//go:build sqlite_fts5

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
	"nexusmail/internal/repository/sqlite"
)

// putOld stores a blob as Put does but dates it before the grace window, which is
// the state the maintenance pass is written for: an upload is unreferenced for the
// instant between Put and AddDraftAttachment, so the sweep deliberately ignores
// anything younger than orphanGrace and a test that used Put alone would only
// ever observe that exemption.
//
// The layout is duplicated from Put rather than reached through it because Put
// owns the timestamps; this file lives in the same package, so the key derivation
// is not a boundary being crossed.
func putOld(t *testing.T, store *Store, repo *sqlite.Store, content, durability string) domain.BlobObject {
	t.Helper()
	digest := sha256.Sum256([]byte(content))
	hexDigest := hex.EncodeToString(digest[:])
	key := filepath.Join(hexDigest[:2], hexDigest[2:4], hexDigest)
	target := filepath.Join(store.root, key)
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * orphanGrace).UnixMilli()
	blob := domain.BlobObject{
		StorageKey: key, SHA256: digest[:], SizeBytes: int64(len(content)),
		Durability: durability, LastAccessedAt: old, CreatedAt: old,
	}
	if err := repo.CreateBlob(context.Background(), &blob); err != nil {
		t.Fatal(err)
	}
	return blob
}

func blobFileExists(t *testing.T, store *Store, blob domain.BlobObject) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(store.root, blob.StorageKey))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

func seedAccount(t *testing.T, repo *sqlite.Store) domain.Account {
	t.Helper()
	now := time.Now().UnixMilli()
	account := domain.Account{
		Email: "blob@example.com", DisplayName: "Blob", Provider: "qq", AuthType: "password",
		Username: "blob@example.com", IMAPHost: "imap.qq.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp.qq.com", SMTPPort: 465, SMTPTLSMode: "implicit",
		SecretCiphertext: []byte("sealed"), Status: "disconnected", CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.CreateAccount(context.Background(), &account); err != nil {
		t.Fatal(err)
	}
	return account
}

func seedDraft(t *testing.T, repo *sqlite.Store, accountID int64, status string) domain.Draft {
	t.Helper()
	now := time.Now().UnixNano()
	draft := domain.Draft{
		AccountID: accountID, RFCMessageID: "<" + time.Unix(0, now).Format("20060102150405.000000000") + "@nexusmail.local>",
		Revision: 1, ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", Status: status, RemoteSyncState: "dirty",
		CreatedAt: now / 1e6, UpdatedAt: now / 1e6,
	}
	if err := repo.CreateDraft(context.Background(), &draft); err != nil {
		t.Fatal(err)
	}
	return draft
}

func attach(t *testing.T, repo *sqlite.Store, draftID int64, blob domain.BlobObject) {
	t.Helper()
	att := domain.DraftAttachment{
		DraftID: draftID, BlobID: blob.ID, Filename: "report.pdf", ContentType: "application/pdf",
		SizeBytes: blob.SizeBytes, CreatedAt: time.Now().UnixMilli(),
	}
	if err := repo.AddDraftAttachment(context.Background(), &att); err != nil {
		t.Fatal(err)
	}
}

// TestReclaimOrphansFreesADeletedDraftsAttachment is the leak itself: draft
// attachments are stored as 'durable', DeleteDraft only removes rows, and Evict
// only ever considers durability='cache'. Before this pass every attachment a
// user ever uploaded stayed on disk for the life of the deployment.
func TestReclaimOrphansFreesADeletedDraftsAttachment(t *testing.T) {
	store, repo := newTestStore(t, 1<<20)
	ctx := context.Background()
	account := seedAccount(t, repo)
	draft := seedDraft(t, repo, account.ID, "draft")

	blob := putOld(t, store, repo, "attachment payload", "durable")
	attach(t, repo, draft.ID, blob)

	// While the draft holds it, reclamation must be a no-op.
	if err := store.ReclaimOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if !blobFileExists(t, store, blob) {
		t.Fatal("reclaim deleted the file of a blob a live draft still references")
	}

	if err := repo.DeleteDraft(ctx, draft.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReclaimOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if blobFileExists(t, store, blob) {
		t.Fatal("the attachment file of a deleted draft is still on disk")
	}
	if _, err := repo.GetBlob(ctx, blob.ID); err == nil {
		t.Fatal("the blob row survived reclamation")
	}
}

// Content addressing means one blob backs every draft that attaches the same
// bytes. Deleting one draft must leave the other's attachment downloadable.
func TestReclaimOrphansKeepsBlobsSharedByAnotherDraft(t *testing.T) {
	store, repo := newTestStore(t, 1<<20)
	ctx := context.Background()
	account := seedAccount(t, repo)
	first := seedDraft(t, repo, account.ID, "draft")
	second := seedDraft(t, repo, account.ID, "draft")

	blob := putOld(t, store, repo, "shared payload", "durable")
	attach(t, repo, first.ID, blob)
	attach(t, repo, second.ID, blob)

	if err := repo.DeleteDraft(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReclaimOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if !blobFileExists(t, store, blob) {
		t.Fatal("reclaim deleted bytes a second draft still attaches")
	}
	if _, err := repo.GetBlob(ctx, blob.ID); err != nil {
		t.Fatalf("shared blob row was deleted: %v", err)
	}
	// The surviving draft can still read its attachment.
	reader, err := store.Open(ctx, blob)
	if err != nil {
		t.Fatalf("open a shared blob after reclaim: %v", err)
	}
	_ = reader.Close()
}

// An 'unknown' send result may already have been delivered, so its attachment
// bytes have to stay: AGENTS.md makes drafts and 'unknown' results exempt from
// any reclamation, and only a person can retire such a draft.
func TestReclaimOrphansKeepsUnknownSendAttachments(t *testing.T) {
	store, repo := newTestStore(t, 1<<20)
	ctx := context.Background()
	account := seedAccount(t, repo)
	draft := seedDraft(t, repo, account.ID, "unknown")

	blob := putOld(t, store, repo, "possibly delivered", "durable")
	attach(t, repo, draft.ID, blob)

	if err := store.ReclaimOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if !blobFileExists(t, store, blob) {
		t.Fatal("reclaim deleted the attachment of a draft whose delivery result is unknown")
	}
	if _, err := repo.GetBlob(ctx, blob.ID); err != nil {
		t.Fatalf("unknown draft's blob row was deleted: %v", err)
	}
}

// attachments.blob_id is ON DELETE SET NULL, so a reclaim that checked only
// draft_attachments would succeed: the row would be deleted, the database would
// silently null the inbox attachment's blob_id, and the file would be gone. The
// user clicks download and gets nothing, with no log line naming the sweep.
func TestReclaimOrphansKeepsBlobsReferencedByInboxAttachments(t *testing.T) {
	store, repo := newTestStore(t, 1<<20)
	ctx := context.Background()
	account := seedAccount(t, repo)
	messageID := seedIncomingMessage(t, repo, account.ID)

	blob := putOld(t, store, repo, "inbox attachment bytes", "durable")
	blobID := blob.ID
	now := time.Now().UnixMilli()
	if err := repo.BatchUpsertAttachments(ctx, []domain.Attachment{{
		MessageID: messageID, PartID: "2", Filename: "invoice.pdf", ContentType: "application/pdf",
		Disposition: "attachment", SizeBytes: blob.SizeBytes, FetchState: "ready", BlobID: &blobID,
		CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}

	if err := store.ReclaimOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if !blobFileExists(t, store, blob) {
		t.Fatal("reclaim deleted the file of an attachment a stored message still references")
	}
	// The row must still point at the blob. This is the assertion that catches the
	// silent half of the bug: ON DELETE SET NULL means a wrong reclaim leaves the
	// attachment row in place with blob_id nulled, so only reading the column shows
	// that the download the user is about to click has lost its bytes.
	_, attachments, err := repo.GetMessage(ctx, messageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 {
		t.Fatalf("message holds %d attachments, want 1", len(attachments))
	}
	if attachments[0].BlobID == nil {
		t.Fatal("attachments.blob_id was nulled by the reclamation pass")
	}
	if *attachments[0].BlobID != blob.ID {
		t.Fatalf("attachments.blob_id = %d, want %d", *attachments[0].BlobID, blob.ID)
	}
}

// messages.raw_blob_id is the same ON DELETE SET NULL trap for the stored raw
// RFC822 copy.
func TestReclaimOrphansKeepsBlobsReferencedByRawMessages(t *testing.T) {
	store, repo := newTestStore(t, 1<<20)
	ctx := context.Background()
	account := seedAccount(t, repo)
	messageID := seedIncomingMessage(t, repo, account.ID)

	blob := putOld(t, store, repo, "raw rfc822 source", "durable")
	blobID := blob.ID
	if err := repo.UpdateMessageBody(ctx, messageID, "text", "<p>html</p>", "snippet", &blobID); err != nil {
		t.Fatal(err)
	}

	if err := store.ReclaimOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if !blobFileExists(t, store, blob) {
		t.Fatal("reclaim deleted the raw source a stored message still references")
	}
	message, _, err := repo.GetMessage(ctx, messageID)
	if err != nil {
		t.Fatal(err)
	}
	if message.RawBlobID == nil {
		t.Fatal("messages.raw_blob_id was nulled by the reclamation pass")
	}
}

// A freshly uploaded blob is unreferenced for the moment between Put and
// AddDraftAttachment. The grace window is what keeps the sweep from deleting an
// upload that is still being wired up.
func TestReclaimOrphansSparesRecentUploads(t *testing.T) {
	store, repo := newTestStore(t, 1<<20)
	ctx := context.Background()
	blob, err := store.Put(ctx, strings.NewReader("just uploaded"), "durable")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReclaimOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if !blobFileExists(t, store, blob) {
		t.Fatal("reclaim deleted an upload that is younger than the grace window")
	}
	if _, err := repo.GetBlob(ctx, blob.ID); err != nil {
		t.Fatalf("recent upload's row was deleted: %v", err)
	}
}

// The cache tier belongs to Evict, which is budget-driven; the orphan pass must
// not delete cached bodies just because nothing references them yet.
func TestReclaimOrphansLeavesTheCacheTierToEvict(t *testing.T) {
	store, repo := newTestStore(t, 1<<20)
	ctx := context.Background()
	blob := putOld(t, store, repo, "cached body", "cache")
	if err := store.ReclaimOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if !blobFileExists(t, store, blob) {
		t.Fatal("the orphan pass deleted a cache-tier blob that Evict owns")
	}
}

func seedIncomingMessage(t *testing.T, repo *sqlite.Store, accountID int64) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	mailbox := domain.Mailbox{
		AccountID: accountID, RemoteName: "INBOX", DisplayName: "Inbox", Role: "inbox",
		SyncMode: "realtime", UIDValidity: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.UpsertMailbox(ctx, &mailbox); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetMailboxByRole(ctx, accountID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	message := domain.Message{
		AccountID: accountID, Direction: "incoming", DedupeKey: []byte("blob-reclaim-dedupe-key-00000001"),
		Subject: "with attachment", Sender: "s@example.com", Recipients: "r@example.com",
		FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
		BodyState: "metadata", ReceivedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	ids, _, err := repo.BatchCreateOrUpdateMessages(ctx, []ports.MessageInput{{
		Message: &message, MailboxID: stored.ID, UID: 1, InternalDate: time.UnixMilli(now),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return ids[0]
}

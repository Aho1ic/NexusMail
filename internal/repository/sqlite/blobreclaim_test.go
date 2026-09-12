//go:build sqlite_fts5

package sqlite

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"nexusmail/internal/domain"
)

func putBlob(t *testing.T, store *Store, durability string) domain.BlobObject {
	t.Helper()
	now := time.Now().UnixMilli()
	digest := sha256.Sum256([]byte(t.Name() + itoa(int(now)) + itoa(int(time.Now().UnixNano()))))
	blob := domain.BlobObject{
		StorageKey: "ab/" + hex8(digest[:]), SHA256: digest[:], SizeBytes: 8,
		Durability: durability, LastAccessedAt: now, CreatedAt: now,
	}
	if err := store.CreateBlob(context.Background(), &blob); err != nil {
		t.Fatal(err)
	}
	return blob
}

func hex8(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 16)
	for i := range 8 {
		out[i*2] = digits[b[i]>>4]
		out[i*2+1] = digits[b[i]&0xf]
	}
	return string(out)
}

func attachToDraft(t *testing.T, store *Store, draftID, blobID int64) {
	t.Helper()
	att := domain.DraftAttachment{
		DraftID: draftID, BlobID: blobID, Filename: "a.bin", ContentType: "application/octet-stream",
		SizeBytes: 8, CreatedAt: time.Now().UnixMilli(),
	}
	if err := store.AddDraftAttachment(context.Background(), &att); err != nil {
		t.Fatal(err)
	}
}

func listed(blobs []domain.BlobObject, id int64) bool {
	for _, blob := range blobs {
		if blob.ID == id {
			return true
		}
	}
	return false
}

func unreferenced(t *testing.T, store *Store) []domain.BlobObject {
	t.Helper()
	// createdBefore far in the future so the grace period does not hide anything
	// these tests are trying to observe; the grace itself is covered separately.
	blobs, err := store.UnreferencedDurableBlobs(context.Background(), time.Now().Add(time.Hour).UnixMilli(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return blobs
}

// TestUnreferencedDurableBlobsReclaimsALoneDraftAttachment is the leak the
// maintenance pass exists to close: a draft is deleted, its attachment rows
// cascade away, and the durable blob they pointed at is now nobody's.
func TestUnreferencedDurableBlobsReclaimsALoneDraftAttachment(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, _ := seedAccountMailbox(t, store)
	draft := seedDraft(t, store, account.ID, "draft", 0, nil)
	blob := putBlob(t, store, "durable")
	attachToDraft(t, store, draft.ID, blob.ID)

	if listed(unreferenced(t, store), blob.ID) {
		t.Fatal("a blob still referenced by a draft attachment was offered for reclaim")
	}
	if err := store.DeleteDraft(ctx, draft.ID); err != nil {
		t.Fatal(err)
	}
	if !listed(unreferenced(t, store), blob.ID) {
		t.Fatal("deleting the only draft that referenced a blob did not make it reclaimable")
	}
}

// Content-addressed storage means two drafts can share one blob. Deleting one
// of them must not take the other's bytes with it.
func TestUnreferencedDurableBlobsKeepsASharedBlob(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, _ := seedAccountMailbox(t, store)
	first := seedDraft(t, store, account.ID, "draft", 0, nil)
	second := seedDraft(t, store, account.ID, "draft", 0, nil)
	blob := putBlob(t, store, "durable")
	attachToDraft(t, store, first.ID, blob.ID)
	attachToDraft(t, store, second.ID, blob.ID)

	if err := store.DeleteDraft(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if listed(unreferenced(t, store), blob.ID) {
		t.Fatal("a blob still referenced by a second draft was offered for reclaim")
	}
	if err := store.DeleteDraft(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if !listed(unreferenced(t, store), blob.ID) {
		t.Fatal("a blob with no remaining draft attachments was not offered for reclaim")
	}
}

// 'unknown' is terminal and the bytes it needed to send may already have been
// accepted by the server, so they have to stay on disk for a human to inspect
// or resend. The draft_attachments row lives as long as the draft does.
func TestUnreferencedDurableBlobsKeepsUnknownDraftBytes(t *testing.T) {
	store := openTestStore(t)
	account, _ := seedAccountMailbox(t, store)
	draft := seedDraft(t, store, account.ID, "unknown", 0, nil)
	blob := putBlob(t, store, "durable")
	attachToDraft(t, store, draft.ID, blob.ID)

	if listed(unreferenced(t, store), blob.ID) {
		t.Fatal("a blob referenced by an 'unknown' draft was offered for reclaim")
	}
}

// attachments.blob_id is ON DELETE SET NULL. A reclaim that only checked
// draft_attachments would succeed, the database would silently null the
// attachment's blob_id, the file would be gone, and a later download of that
// inbox attachment would fail with nothing in the log pointing at the sweep.
func TestUnreferencedDurableBlobsKeepsInboxAttachmentBytes(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	messageID := seedMessage(t, store, account.ID, mailbox.ID, 1, "with file", "incoming", false, now)
	blob := putBlob(t, store, "durable")
	blobID := blob.ID
	att := domain.Attachment{
		MessageID: messageID, PartID: "1", Filename: "a.bin", ContentType: "application/octet-stream",
		Disposition: "attachment", SizeBytes: 8, FetchState: "ready", BlobID: &blobID,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.BatchUpsertAttachments(ctx, []domain.Attachment{att}); err != nil {
		t.Fatal(err)
	}

	if listed(unreferenced(t, store), blob.ID) {
		t.Fatal("a blob referenced by attachments.blob_id was offered for reclaim")
	}
}

// messages.raw_blob_id is the same SET NULL trap, for the raw RFC822 copy.
func TestUnreferencedDurableBlobsKeepsRawMessageBytes(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	messageID := seedMessage(t, store, account.ID, mailbox.ID, 1, "raw body", "incoming", false, now)
	blob := putBlob(t, store, "durable")
	blobID := blob.ID
	if err := store.UpdateMessageBody(ctx, messageID, "text", "<p>html</p>", "snip", &blobID); err != nil {
		t.Fatal(err)
	}

	if listed(unreferenced(t, store), blob.ID) {
		t.Fatal("a blob referenced by messages.raw_blob_id was offered for reclaim")
	}
}

func TestUnreferencedDurableBlobsIgnoresTheCacheTier(t *testing.T) {
	store := openTestStore(t)
	cached := putBlob(t, store, "cache")
	if listed(unreferenced(t, store), cached.ID) {
		t.Fatal("a cache-tier blob was offered to the durable reclaim pass")
	}
}

func TestUnreferencedDurableBlobsHonoursTheGraceWindow(t *testing.T) {
	store := openTestStore(t)
	blob := putBlob(t, store, "durable")
	// createdBefore in the past of the blob's created_at: the grace window has
	// not elapsed, so even an unreferenced durable blob is not yet a candidate.
	blobs, err := store.UnreferencedDurableBlobs(context.Background(), blob.CreatedAt, 100)
	if err != nil {
		t.Fatal(err)
	}
	if listed(blobs, blob.ID) {
		t.Fatal("a blob created at the cutoff was offered for reclaim; the predicate is created_at < cutoff")
	}
}

func TestUnreferencedDurableBlobsCapsTheBatch(t *testing.T) {
	store := openTestStore(t)
	for range 5 {
		putBlob(t, store, "durable")
	}
	blobs, err := store.UnreferencedDurableBlobs(context.Background(), time.Now().Add(time.Hour).UnixMilli(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 2 {
		t.Fatalf("limit 2 returned %d blobs", len(blobs))
	}
}

//go:build sqlite_fts5

package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/repository/sqlite"
)

// TestPutDeduplicatesByContent asserts the content-addressed layout: two writes of
// the same bytes must share one row and one file, because the same attachment
// forwarded around an inbox is the normal case, not the exception.
func TestPutDeduplicatesByContent(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	ctx := context.Background()

	first, err := store.Put(ctx, strings.NewReader("attachment payload"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(ctx, strings.NewReader("attachment payload"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("same content produced two rows: %d and %d", first.ID, second.ID)
	}
	if first.StorageKey != second.StorageKey {
		t.Fatalf("same content produced two keys: %q and %q", first.StorageKey, second.StorageKey)
	}
	other, err := store.Put(ctx, strings.NewReader("different payload"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == first.ID {
		t.Fatal("different content collapsed onto one row")
	}
}

// TestPutUpgradesDurability covers the case that protects an in-flight send: a
// blob already cached must become durable when a draft claims it, or LRU eviction
// can delete the body of a message whose delivery result is unknown.
func TestPutUpgradesDurability(t *testing.T) {
	store, repo := newTestStore(t, 1<<20)
	ctx := context.Background()

	cached, err := store.Put(ctx, strings.NewReader("shared body"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Put(ctx, strings.NewReader("shared body"), "durable"); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetBlob(ctx, cached.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Durability != "durable" {
		t.Fatalf("durability not upgraded: %q", stored.Durability)
	}

	// The reverse must not happen: a later cache write cannot demote a blob a draft
	// is relying on.
	if _, err = store.Put(ctx, strings.NewReader("shared body"), "cache"); err != nil {
		t.Fatal(err)
	}
	if stored, err = repo.GetBlob(ctx, cached.ID); err != nil {
		t.Fatal(err)
	}
	if stored.Durability != "durable" {
		t.Fatalf("durability demoted to %q", stored.Durability)
	}
}

func TestPutRejectsUnknownDurability(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	if _, err := store.Put(context.Background(), strings.NewReader("x"), "forever"); err == nil {
		t.Fatal("unknown durability accepted")
	}
}

// TestEvictDropsLeastRecentlyUsed pins the two properties eviction has to hold at
// once: the budget is respected, and durable blobs are never candidates however
// old they are.
func TestEvictDropsLeastRecentlyUsed(t *testing.T) {
	// Seeded through a store with room to spare, because Put evicts inline: under a
	// tight budget the third write would evict the first before the access order
	// under test has been established.
	store, repo := newTestStore(t, 1<<20)
	ctx := context.Background()

	durable, err := store.Put(ctx, bytes.NewReader(make([]byte, 10)), "durable")
	if err != nil {
		t.Fatal(err)
	}
	oldest, err := store.Put(ctx, strings.NewReader("aaaaaaaaaa"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	middle, err := store.Put(ctx, strings.NewReader("bbbbbbbbbb"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	newest, err := store.Put(ctx, strings.NewReader("cccccccccc"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	// Touch in the order that should decide survival. last_accessed_at has
	// millisecond resolution, so the touches have to be separated or the ordering
	// the assertion depends on is undefined.
	for _, id := range []int64{oldest.ID, middle.ID, newest.ID} {
		if _, err := repo.GetBlob(ctx, id); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Same files, same rows, budget that fits two of the three cached blobs.
	tight, err := New(store.root, 24, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := tight.Evict(ctx); err != nil {
		t.Fatal(err)
	}
	remaining, err := repo.CachedBlobBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if remaining > 24 {
		t.Fatalf("cache still over budget: %d bytes", remaining)
	}
	if _, err := repo.GetBlob(ctx, oldest.ID); err == nil {
		t.Fatal("least recently used blob survived eviction")
	}
	if _, err := repo.GetBlob(ctx, newest.ID); err != nil {
		t.Fatalf("most recently used blob was evicted: %v", err)
	}
	if _, err := repo.GetBlob(ctx, durable.ID); err != nil {
		t.Fatalf("durable blob was evicted: %v", err)
	}
	// The file must go with the row, otherwise the budget is only enforced on paper.
	if _, err := os.Stat(filepath.Join(store.root, oldest.StorageKey)); !os.IsNotExist(err) {
		t.Fatalf("evicted blob file still present: %v", err)
	}
}

func TestEvictLeavesCacheUnderBudgetAlone(t *testing.T) {
	store, repo := newTestStore(t, 1<<20)
	ctx := context.Background()
	blob, err := store.Put(ctx, strings.NewReader("small"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Evict(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetBlob(ctx, blob.ID); err != nil {
		t.Fatalf("blob under budget was evicted: %v", err)
	}
}

// TestOpenRejectsEscapingKey guards the path join: storage keys reach Open from
// database rows, so a traversal key must be refused rather than resolved.
func TestOpenRejectsEscapingKey(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	ctx := context.Background()
	blob, err := store.Put(ctx, strings.NewReader("payload"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"../../etc/passwd", "/etc/passwd", "", "a/../../b"} {
		blob.StorageKey = key
		if _, err := store.Open(ctx, blob); err == nil {
			t.Errorf("Open accepted storage key %q", key)
		}
		if err := store.Remove(ctx, blob); err == nil {
			t.Errorf("Remove accepted storage key %q", key)
		}
	}
}

func TestOpenReturnsStoredBytes(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	ctx := context.Background()
	blob, err := store.Put(ctx, strings.NewReader("round trip"), "durable")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.Open(ctx, blob)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "round trip" {
		t.Fatalf("read back %q", body)
	}
	if blob.SizeBytes != int64(len("round trip")) {
		t.Fatalf("recorded size %d", blob.SizeBytes)
	}
}

func newTestStore(t *testing.T, maxBytes int64) (*Store, *sqlite.Store) {
	t.Helper()
	root := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(root, "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	store, err := New(filepath.Join(root, "blobs"), maxBytes, repo)
	if err != nil {
		t.Fatal(err)
	}
	return store, repo
}

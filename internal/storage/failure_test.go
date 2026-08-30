//go:build sqlite_fts5

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nexusmail/internal/domain"
)

// failingReader stops partway through, which is what a dropped IMAP connection looks
// like to Put: some bytes arrived, the rest never will.
type failingReader struct {
	prefix    []byte
	delivered bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.delivered && len(r.prefix) > 0 {
		r.delivered = true
		n := copy(p, r.prefix)
		return n, nil
	}
	return 0, errors.New("connection lost mid-attachment")
}

// TestPutReportsAFailedRead covers the partial-write path. It matters beyond the error
// itself: a truncated attachment must not be committed under the hash of the bytes
// that did arrive, because that hash would then satisfy a later dedupe check and the
// real attachment would never be fetched.
func TestPutReportsAFailedRead(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	ctx := context.Background()

	if _, err := store.Put(ctx, &failingReader{prefix: []byte("first half")}, "cache"); err == nil {
		t.Fatal("Put accepted a reader that failed mid-stream")
	}

	// Nothing may be left behind: neither a committed blob nor the temp file.
	entries, err := os.ReadDir(store.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("Put left %q behind after a failed read", entry.Name())
	}
}

// TestPutReportsAnUnwritableRoot covers the temp-file and commit failures together.
// A blob root that cannot be written to is the disk-full or wrong-permissions case,
// and Put has to surface it rather than return a blob whose bytes are not on disk.
func TestPutReportsAnUnwritableRoot(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	ctx := context.Background()

	// Reachable only by taking write permission away after New created the directory.
	if err := os.Chmod(store.root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(store.root, 0o750) })

	blob, err := store.Put(ctx, strings.NewReader("payload"), "cache")
	if err == nil {
		t.Fatalf("Put succeeded against a read-only root and returned key %q", blob.StorageKey)
	}
	if !strings.Contains(err.Error(), "blob") {
		t.Errorf("error is %q, want it to name the blob operation", err)
	}
}

// TestRemoveIgnoresAnAlreadyMissingFile pins the idempotence Evict depends on: the
// row and the file can disagree after a crash between the two, and a Remove that
// failed on the missing file would stall eviction on that blob for good.
func TestRemoveIgnoresAnAlreadyMissingFile(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	ctx := context.Background()

	blob, err := store.Put(ctx, strings.NewReader("payload"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.root, blob.StorageKey)); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(ctx, blob); err != nil {
		t.Errorf("Remove on a missing file returned %v, want nil", err)
	}
	// And again, now that the row is gone too.
	if err := store.Remove(ctx, blob); err != nil {
		t.Errorf("second Remove returned %v, want nil", err)
	}
}

// TestRemoveReportsAnUndeletableFile is the other half: a Remove that genuinely fails
// must be reported, or Evict silently believes it has freed space it has not.
func TestRemoveReportsAnUndeletableFile(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	ctx := context.Background()

	blob, err := store.Put(ctx, strings.NewReader("payload"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	// Unlink permission belongs to the containing directory, not the file.
	parent := filepath.Dir(filepath.Join(store.root, blob.StorageKey))
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o750) })

	if err := store.Remove(ctx, blob); err == nil {
		t.Error("Remove succeeded against an undeletable file")
	}
}

// TestRemoveRejectsEscapingKey guards the same boundary Open does. Remove deletes,
// so an unchecked key here is worse than one in Open: it would take the file with it.
func TestRemoveRejectsEscapingKey(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	ctx := context.Background()

	outside := filepath.Join(filepath.Dir(store.root), "outside.txt")
	if err := os.WriteFile(outside, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{
		"../outside.txt", "../../outside.txt", "a/../../outside.txt",
		"/etc/passwd", "", "..", "./relative", "dir//double",
	} {
		if err := store.Remove(ctx, domain.BlobObject{StorageKey: key}); err == nil {
			t.Errorf("Remove accepted key %q", key)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a file outside the blob root was removed: %v", err)
	}
}

// TestEvictReportsARemoveFailure covers Evict's error path. Evict returning nil after
// a failed delete would report the cache as trimmed while it is still over budget,
// so the next sweep starts from the same place and the budget is never enforced.
func TestEvictReportsARemoveFailure(t *testing.T) {
	// The budget starts generous because Put evicts on its own for cache blobs, so a
	// store built already over budget deletes the blob before the test can touch it.
	store, _ := newTestStore(t, 1<<20)
	ctx := context.Background()

	blob, err := store.Put(ctx, strings.NewReader("a payload larger than the budget below"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(filepath.Join(store.root, blob.StorageKey))
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o750) })
	store.maxBytes = 8

	if err := store.Evict(ctx); err == nil {
		t.Error("Evict reported success while the blob it had to drop is still present")
	}
}

// TestNewReportsAnUncreatableRoot covers construction. main treats this error as
// fatal, so a New that returned a usable Store here would start the process with a
// blob root that no attachment can ever be written to.
func TestNewReportsAnUncreatableRoot(t *testing.T) {
	dir := t.TempDir()
	// A file where the directory has to go: MkdirAll cannot proceed through it.
	blocker := filepath.Join(dir, "blobs")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(blocker, 1<<20, nil); err == nil {
		t.Fatal("New accepted a root that is a file")
	}
	if _, err := New(filepath.Join(blocker, "nested"), 1<<20, nil); err == nil {
		t.Fatal("New accepted a root underneath a file")
	}
}

// TestPutReportsAnUncreatableShard covers the fan-out directory. The layout is
// two levels of hex prefix, so a stray file at either level blocks every blob whose
// digest starts with those bytes while leaving the rest of the store working — a
// partial failure that has to surface rather than be silently retried forever.
func TestPutReportsAnUncreatableShard(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	ctx := context.Background()
	const payload = "payload"

	// The digest prefix is deterministic, so the shard path is known in advance.
	digest := sha256.Sum256([]byte(payload))
	shard := filepath.Join(store.root, hex.EncodeToString(digest[:])[:2])
	if err := os.WriteFile(shard, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Put(ctx, strings.NewReader(payload), "cache"); err == nil {
		t.Fatal("Put succeeded with a file blocking its shard directory")
	}
}

// TestPutReportsARepositoryFailure covers the row-creation error. The file is on disk
// by that point, so returning the blob anyway would hand back an ID of zero and a key
// no row references — a leaked file that eviction can never find.
func TestPutReportsARepositoryFailure(t *testing.T) {
	store, repo := newTestStore(t, 1<<20)
	ctx := context.Background()

	// Closing the repository is the least artificial way to make every query fail;
	// it is what a lost database handle looks like to this layer.
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Put(ctx, strings.NewReader("payload"), "cache"); err == nil {
		t.Fatal("Put succeeded with a closed repository")
	}
}

// TestEvictReportsRepositoryFailures covers both queries Evict depends on. It runs on
// the maintenance ticker, where the only signal is the returned error, so a swallowed
// failure there means the cache silently stops being bounded.
func TestEvictReportsRepositoryFailures(t *testing.T) {
	store, repo := newTestStore(t, 8)
	ctx := context.Background()
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Evict(ctx); err == nil {
		t.Error("Evict reported success with a closed repository")
	}
}

// TestPutSurvivesAnAlreadyCommittedTarget covers the rename branch that tolerates an
// existing target. Two writers racing on the same bytes is normal — the same
// attachment fetched twice — and the loser's rename finds the file already there.
// Treating that as an error would fail a fetch that in fact succeeded.
func TestPutSurvivesAnAlreadyCommittedTarget(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	ctx := context.Background()
	const payload = "shared attachment bytes"

	first, err := store.Put(ctx, strings.NewReader(payload), "cache")
	if err != nil {
		t.Fatal(err)
	}
	// Re-writing the same bytes takes the rename-onto-existing path.
	second, err := store.Put(ctx, strings.NewReader(payload), "cache")
	if err != nil {
		t.Fatalf("Put failed when the target already existed: %v", err)
	}
	if second.StorageKey != first.StorageKey || second.ID != first.ID {
		t.Errorf("second Put produced %d/%s, want the first row %d/%s", second.ID, second.StorageKey, first.ID, first.StorageKey)
	}
	// The bytes must still be intact, not a zero-length file left by a failed rename.
	reader, err := store.Open(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Errorf("stored bytes are %q, want %q", got, payload)
	}
}

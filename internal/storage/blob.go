package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

type Store struct {
	root     string
	maxBytes int64
	repo     blobRepo
}

// blobRepo is ports.BlobRepo plus the orphan query only this package needs, so
// the reclamation pass does not widen the port every consumer of it sees.
type blobRepo interface {
	ports.BlobRepo
	UnreferencedDurableBlobs(ctx context.Context, createdBefore int64, limit int) ([]domain.BlobObject, error)
}

func New(root string, maxBytes int64, repo blobRepo) (*Store, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create blob directory: %w", err)
	}
	return &Store{root: root, maxBytes: maxBytes, repo: repo}, nil
}

func (s *Store) Put(ctx context.Context, reader io.Reader, durability string) (domain.BlobObject, error) {
	if durability != "cache" && durability != "durable" {
		return domain.BlobObject{}, ports.Invalidf("invalid blob durability")
	}
	temp, err := os.CreateTemp(s.root, ".upload-*")
	if err != nil {
		return domain.BlobObject{}, fmt.Errorf("create blob temp file: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	hasher := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(temp, hasher), reader)
	closeErr := temp.Close()
	if copyErr != nil {
		return domain.BlobObject{}, fmt.Errorf("write blob: %w", copyErr)
	}
	if closeErr != nil {
		return domain.BlobObject{}, fmt.Errorf("close blob: %w", closeErr)
	}
	digest := hasher.Sum(nil)
	hexDigest := hex.EncodeToString(digest)
	key := filepath.Join(hexDigest[:2], hexDigest[2:4], hexDigest)
	target := filepath.Join(s.root, key)
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return domain.BlobObject{}, err
	}
	// Track whether this call is the one that put the file at target. The rename
	// consumes tempName, so the deferred cleanup above no longer has anything to
	// remove; if the index write then fails, the file is unreferenced and
	// eviction — which is driven entirely by blob_objects rows — can never find
	// it again.
	//
	// A concurrent writer of the same content is not an error: os.ErrExist, or a
	// failed rename whose target nevertheless exists, means the other caller won
	// the race. The file is theirs, so this call must not remove it on failure.
	committed := false
	if err := os.Rename(tempName, target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			if _, statErr := os.Stat(target); statErr != nil {
				return domain.BlobObject{}, fmt.Errorf("commit blob: %w", err)
			}
		}
	} else {
		committed = true
	}
	now := time.Now().UnixMilli()
	blob := domain.BlobObject{StorageKey: key, SHA256: digest, SizeBytes: size, Durability: durability, LastAccessedAt: now, CreatedAt: now}
	if err := s.repo.CreateBlob(ctx, &blob); err != nil {
		if committed {
			_ = os.Remove(target)
		}
		return domain.BlobObject{}, err
	}
	if durability == "cache" {
		_ = s.Evict(ctx)
	}
	return blob, nil
}

// Open reads a stored blob. ctx is part of the ports.BlobStore contract and is
// unused here because the local filesystem read is not cancellable; a remote
// implementation of the same port would need it.
func (s *Store) Open(_ context.Context, blob domain.BlobObject) (io.ReadCloser, error) {
	if !safeStorageKey(blob.StorageKey) {
		return nil, ports.Invalidf("invalid blob storage key")
	}
	return os.Open(filepath.Join(s.root, blob.StorageKey))
}

func (s *Store) Remove(ctx context.Context, blob domain.BlobObject) error {
	if err := s.removeFile(blob.StorageKey); err != nil {
		return err
	}
	return s.repo.DeleteBlob(ctx, blob.ID)
}

func (s *Store) removeFile(key string) error {
	if !safeStorageKey(key) {
		return ports.Invalidf("invalid blob storage key")
	}
	if err := os.Remove(filepath.Join(s.root, key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Evict trims the cache tier to maxBytes, least recently used first.
//
// The total is read with an aggregate first and the rows are only materialised
// when the budget is actually exceeded: this runs after every cached write, so
// the common case must not scan the whole tier.
func (s *Store) Evict(ctx context.Context) error {
	total, err := s.repo.CachedBlobBytes(ctx)
	if err != nil {
		return err
	}
	if total <= s.maxBytes {
		return nil
	}
	blobs, err := s.repo.CachedBlobs(ctx)
	if err != nil {
		return err
	}
	for _, blob := range blobs {
		if total <= s.maxBytes {
			break
		}
		if err := s.Remove(ctx, blob); err != nil {
			return err
		}
		total -= blob.SizeBytes
	}
	return nil
}

// orphanGrace is how long a durable blob must have existed before the sweep will
// consider it unreferenced. An upload is stored before the draft_attachments row
// that references it is written — Put and AddDraftAttachment are two statements of
// one request — and a client on a slow link can sit between them. An hour is far
// beyond that window and still lets the 15-minute maintenance pass reclaim the
// disk on the same day it was orphaned.
const orphanGrace = time.Hour

// orphanBatch bounds one sweep. Reclaiming is not urgent and the pass repeats
// every maintenance interval, so a database that has accumulated a large backlog
// of orphans drains over several ticks instead of holding writeMu for all of them
// at once.
const orphanBatch = 256

// ReclaimOrphans deletes durable blobs nothing references any more. It is the only
// reclamation path for the durable tier: Evict is by construction limited to
// durability='cache', so without this every draft attachment ever uploaded stays
// on disk forever.
//
// The row goes first and the file second, which is the opposite of Remove. Once
// the row is gone, draft_attachments.blob_id (ON DELETE RESTRICT) can no longer
// be pointed at it, so a draft cannot acquire a reference to bytes that are about
// to be deleted. The residual risk is a leaked file if the process dies between
// the two steps — the next sweep cannot see it, because it has no row — and that
// is the cheaper failure: the reverse order can leave a live row whose file is
// missing, which is an attachment that fails to download.
func (s *Store) ReclaimOrphans(ctx context.Context) error {
	blobs, err := s.repo.UnreferencedDurableBlobs(ctx, time.Now().Add(-orphanGrace).UnixMilli(), orphanBatch)
	if err != nil {
		return err
	}
	for _, blob := range blobs {
		if !safeStorageKey(blob.StorageKey) {
			// A key that cannot be joined to a path is not something to delete blindly,
			// and it is not a reason to abandon the rest of the batch.
			slog.Warn("skip orphan blob with unsafe storage key", "blob_id", blob.ID)
			continue
		}
		if err := s.repo.DeleteBlob(ctx, blob.ID); err != nil {
			return err
		}
		if err := s.removeFile(blob.StorageKey); err != nil {
			return err
		}
	}
	return nil
}

func safeStorageKey(key string) bool {
	clean := filepath.Clean(key)
	return key != "" && clean == key && !filepath.IsAbs(key) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

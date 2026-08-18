package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	repo     ports.Repository
}

func New(root string, maxBytes int64, repo ports.Repository) (*Store, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create blob directory: %w", err)
	}
	return &Store{root: root, maxBytes: maxBytes, repo: repo}, nil
}

func (s *Store) Put(ctx context.Context, reader io.Reader, durability string) (domain.BlobObject, error) {
	if durability != "cache" && durability != "durable" {
		return domain.BlobObject{}, errors.New("invalid blob durability")
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
	if err := os.Rename(tempName, target); err != nil && !errors.Is(err, os.ErrExist) {
		if _, statErr := os.Stat(target); statErr != nil {
			return domain.BlobObject{}, fmt.Errorf("commit blob: %w", err)
		}
	}
	now := time.Now().UnixMilli()
	blob := domain.BlobObject{StorageKey: key, SHA256: digest, SizeBytes: size, Durability: durability, LastAccessedAt: now, CreatedAt: now}
	if err := s.repo.CreateBlob(ctx, &blob); err != nil {
		return domain.BlobObject{}, err
	}
	if durability == "cache" {
		_ = s.Evict(ctx)
	}
	return blob, nil
}

func (s *Store) Open(ctx context.Context, blob domain.BlobObject) (io.ReadCloser, error) {
	if !safeStorageKey(blob.StorageKey) {
		return nil, errors.New("invalid blob storage key")
	}
	return os.Open(filepath.Join(s.root, blob.StorageKey))
}

func (s *Store) Remove(ctx context.Context, blob domain.BlobObject) error {
	if !safeStorageKey(blob.StorageKey) {
		return errors.New("invalid blob storage key")
	}
	if err := os.Remove(filepath.Join(s.root, blob.StorageKey)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.repo.DeleteBlob(ctx, blob.ID)
}

func (s *Store) Evict(ctx context.Context) error {
	blobs, err := s.repo.CachedBlobs(ctx)
	if err != nil {
		return err
	}
	var total int64
	for _, blob := range blobs {
		total += blob.SizeBytes
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

func safeStorageKey(key string) bool {
	clean := filepath.Clean(key)
	return key != "" && clean == key && !filepath.IsAbs(key) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// BlobStore stores and reads opaque blobs (subtitle files) by string key. Each
// saved subtitle records which backend holds it (its Name), so reads route back
// to the right store even after the configured default changes.
//
// The admin is still the PRIMARY writer here — the crawler and the admin UI
// write most of the catalog's subtitles — but this service writes too, into the
// same storage, whenever a signed-in visitor saves a subtitle for everyone from
// the watch page. So SUBTITLE_STORAGE_DIR / S3_BUCKET must match admin's AND be
// writable by this process.
//
// Delete exists for exactly one caller: rolling back a blob whose subtitles row
// failed to insert. Nothing here ever deletes a subtitle a user can see — that
// stays an admin action.
type BlobStore interface {
	Name() string
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// errBlobNotFound is returned by Get when the key does not exist, so callers can
// map it to a 404.
var errBlobNotFound = errors.New("blob not found")

// newBlobStores builds every BlobStore the configuration enables, keyed by Name,
// and returns which one new writes go to. The local store is always available
// (must point at the same directory admin writes to); the S3 store is only built
// when S3_BUCKET is set.
//
// Mirrors admin's newBlobStores, including the startup error when
// SUBTITLE_STORAGE_BACKEND names a store that was not built — failing at boot
// beats discovering it on the first save.
func newBlobStores(cfg Config) (map[string]BlobStore, string, error) {
	stores := map[string]BlobStore{}

	local, err := newLocalBlobStore(cfg.SubtitleStorageDir)
	if err != nil {
		return nil, "", fmt.Errorf("local store: %w", err)
	}
	stores[local.Name()] = local

	if cfg.S3Bucket != "" {
		s3, err := newS3BlobStore(cfg)
		if err != nil {
			return nil, "", fmt.Errorf("s3 store: %w", err)
		}
		stores[s3.Name()] = s3
	}

	primary := cfg.SubtitleStorageBackend
	if primary == "" {
		primary = local.Name()
	}
	if stores[primary] == nil {
		return nil, "", fmt.Errorf("SUBTITLE_STORAGE_BACKEND=%q is not configured (set S3_BUCKET for s3)", primary)
	}
	return stores, primary, nil
}

// --- local filesystem ------------------------------------------------------

type localBlobStore struct {
	dir string
}

func newLocalBlobStore(dir string) (*localBlobStore, error) {
	if dir == "" {
		dir = "./data/subtitles"
	}
	// This service writes here now (a user saving a subtitle for everyone), so
	// the directory has to exist and be writable. In production it is the shared
	// phimtor2_subtitles volume the admin also mounts; both containers run as the
	// same distroless uid, so admin-created directories accept these writes.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &localBlobStore{dir: dir}, nil
}

func (l *localBlobStore) Name() string { return "local" }

// path maps a forward-slash key to an absolute on-disk path, keeping it within
// the base dir (keys are server-generated, but clean defensively anyway).
func (l *localBlobStore) path(key string) string {
	clean := filepath.FromSlash(filepath.ToSlash(filepath.Clean("/" + key)))
	return filepath.Join(l.dir, clean)
}

func (l *localBlobStore) Put(_ context.Context, key string, data []byte, _ string) error {
	p := l.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func (l *localBlobStore) Delete(_ context.Context, key string) error {
	err := os.Remove(l.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (l *localBlobStore) Get(_ context.Context, key string) ([]byte, error) {
	data, err := os.ReadFile(l.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errBlobNotFound
	}
	return data, err
}

// --- S3-compatible object storage ------------------------------------------

type s3BlobStore struct {
	client *minio.Client
	bucket string
}

func newS3BlobStore(cfg Config) (*s3BlobStore, error) {
	endpoint := cfg.S3Endpoint
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	// minio expects a host[:port] without scheme; secure flag carries https.
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: cfg.S3UseSSL,
		Region: cfg.S3Region,
	})
	if err != nil {
		return nil, err
	}
	return &s3BlobStore{client: client, bucket: cfg.S3Bucket}, nil
}

func (s *s3BlobStore) Name() string { return "s3" }

func (s *s3BlobStore) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *s3BlobStore) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func (s *s3BlobStore) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		if resp := minio.ToErrorResponse(err); resp.Code == "NoSuchKey" {
			return nil, errBlobNotFound
		}
		return nil, err
	}
	return data, nil
}

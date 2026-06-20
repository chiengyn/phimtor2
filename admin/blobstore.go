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

// BlobStore persists opaque blobs (subtitle files) under a string key. There are
// two implementations — a local filesystem store and an S3-compatible object
// store — so the admin can keep subtitles next to itself or in shared object
// storage. Each saved subtitle records which backend holds it (its Name), so
// reads/deletes route back to the right store even if the default later changes.
type BlobStore interface {
	Name() string
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// errBlobNotFound is returned by Get when the key does not exist, so callers can
// map it to a 404.
var errBlobNotFound = errors.New("blob not found")

// newBlobStores builds every BlobStore the configuration enables and returns
// them keyed by Name, plus the name of the primary one (where new subtitles are
// written). The local store is always available (with a sensible default dir);
// the S3 store is only built when S3_BUCKET is set. The configured
// SUBTITLE_STORAGE_BACKEND must be among the built stores.
func newBlobStores(cfg Config) (stores map[string]BlobStore, primary string, err error) {
	stores = map[string]BlobStore{}

	local, err := newLocalBlobStore(cfg.SubtitleStorageDir)
	if err != nil {
		return nil, "", err
	}
	stores[local.Name()] = local

	if cfg.S3Bucket != "" {
		s3, err := newS3BlobStore(cfg)
		if err != nil {
			return nil, "", fmt.Errorf("s3 store: %w", err)
		}
		stores[s3.Name()] = s3
	}

	primary = cfg.SubtitleStorageBackend
	if primary == "" {
		primary = "local"
	}
	if _, ok := stores[primary]; !ok {
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create subtitle dir: %w", err)
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

func (l *localBlobStore) Get(_ context.Context, key string) ([]byte, error) {
	data, err := os.ReadFile(l.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errBlobNotFound
	}
	return data, err
}

func (l *localBlobStore) Delete(_ context.Context, key string) error {
	err := os.Remove(l.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
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

func (s *s3BlobStore) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

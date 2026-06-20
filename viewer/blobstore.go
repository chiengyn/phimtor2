package main

import (
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

// BlobStore reads opaque blobs (subtitle files) by string key. This is the
// READ-ONLY counterpart of admin's BlobStore: the admin owns and writes subtitle
// files; the viewer only serves them. Each saved subtitle records which backend
// holds it (its Name), so reads route back to the right store.
type BlobStore interface {
	Name() string
	Get(ctx context.Context, key string) ([]byte, error)
}

// errBlobNotFound is returned by Get when the key does not exist, so callers can
// map it to a 404.
var errBlobNotFound = errors.New("blob not found")

// newReadOnlyBlobStores builds every BlobStore the configuration enables, keyed
// by Name. The local store is always available (must point at the same directory
// admin writes to); the S3 store is only built when S3_BUCKET is set.
func newReadOnlyBlobStores(cfg Config) (map[string]BlobStore, error) {
	stores := map[string]BlobStore{}

	local := newLocalBlobStore(cfg.SubtitleStorageDir)
	stores[local.Name()] = local

	if cfg.S3Bucket != "" {
		s3, err := newS3BlobStore(cfg)
		if err != nil {
			return nil, fmt.Errorf("s3 store: %w", err)
		}
		stores[s3.Name()] = s3
	}
	return stores, nil
}

// --- local filesystem ------------------------------------------------------

type localBlobStore struct {
	dir string
}

func newLocalBlobStore(dir string) *localBlobStore {
	if dir == "" {
		dir = "./data/subtitles"
	}
	// Read-only: don't create the directory (the admin owns it). A missing dir
	// simply yields not-found on Get.
	return &localBlobStore{dir: dir}
}

func (l *localBlobStore) Name() string { return "local" }

// path maps a forward-slash key to an absolute on-disk path, keeping it within
// the base dir (keys are server-generated, but clean defensively anyway).
func (l *localBlobStore) path(key string) string {
	clean := filepath.FromSlash(filepath.ToSlash(filepath.Clean("/" + key)))
	return filepath.Join(l.dir, clean)
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

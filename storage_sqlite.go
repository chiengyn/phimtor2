//go:build cgo

package main

import (
	"fmt"
	"path/filepath"

	"github.com/anacrolix/torrent/storage"
	sqliteStorage "github.com/anacrolix/torrent/storage/sqlite"
)

// newSQLiteStorage builds the built-in capped sqlite storage: a single .db file
// with internal LRU eviction and a reported Capacity (so the client gracefully
// re-downloads evicted pieces). Prefixes are kept warm via piece priority only.
// Requires a cgo build.
func newSQLiteStorage(cfg StorageConfig) (storage.ClientImplCloser, error) {
	opts := sqliteStorage.NewDirectStorageOpts{}
	opts.Path = filepath.Join(cfg.DataDir, "storage.db")
	opts.Capacity = cfg.CacheBytes

	impl, err := sqliteStorage.NewDirectStorage(opts)
	if err != nil {
		return nil, fmt.Errorf("open sqlite storage: %w", err)
	}
	return impl, nil
}

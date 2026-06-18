//go:build !cgo

package main

import (
	"errors"

	"github.com/anacrolix/torrent/storage"
)

// newSQLiteStorage is unavailable without cgo. Build with CGO_ENABLED=1 to use the
// "capped-sqlite" storage mode.
func newSQLiteStorage(cfg StorageConfig) (storage.ClientImplCloser, error) {
	return nil, errors.New(`storage mode "capped-sqlite" requires a cgo build (CGO_ENABLED=1)`)
}

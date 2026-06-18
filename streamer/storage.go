package main

import (
	"fmt"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// Storage backend modes selectable via STORAGE_MODE / -storage.
const (
	// StorageModePrefixCache is the default: a custom two-tier storage that pins
	// the beginning of each video on disk and keeps the bulk in a bounded LRU cache.
	StorageModePrefixCache = "prefix-cache"
	// StorageModeCappedSQLite uses the built-in capped sqlite storage (requires cgo).
	StorageModeCappedSQLite = "capped-sqlite"
)

// StorageConfig holds the settings needed to build a storage backend.
type StorageConfig struct {
	Mode        string
	DataDir     string
	PrefixBytes int64 // bytes pinned at the start of each video file
	CacheBytes  int64 // LRU budget for the bulk (cache / sqlite cap)
}

// newStorage builds the storage backend selected by cfg.Mode.
func newStorage(cfg StorageConfig) (storage.ClientImplCloser, error) {
	switch cfg.Mode {
	case StorageModePrefixCache, "":
		return newPrefixCacheStorage(cfg)
	case StorageModeCappedSQLite:
		return newSQLiteStorage(cfg)
	default:
		return nil, fmt.Errorf("unknown storage mode %q (want %q or %q)",
			cfg.Mode, StorageModePrefixCache, StorageModeCappedSQLite)
	}
}

// prefixPieceIndices returns the set of piece indices that overlap the first
// prefixBytes of every video file in the torrent. It is used both by the manager
// (to raise piece priority so the prefix stays warm) and by the prefix-cache
// storage (to route those pieces to the persistent tier).
func prefixPieceIndices(info *metainfo.Info, prefixBytes int64) map[int]bool {
	out := make(map[int]bool)
	if prefixBytes <= 0 || info.PieceLength <= 0 {
		return out
	}
	for _, fi := range info.UpvertedFiles() {
		name := strings.Join(fi.BestPath(), "/")
		if name == "" {
			// Single-file torrent: the name lives on the info dict.
			name = info.BestName()
		}
		if !isVideoFile(name) {
			continue
		}
		prefixLen := prefixBytes
		if fi.Length < prefixLen {
			prefixLen = fi.Length
		}
		if prefixLen <= 0 {
			continue
		}
		begin := int(fi.TorrentOffset / info.PieceLength)
		end := int((fi.TorrentOffset + prefixLen - 1) / info.PieceLength)
		for i := begin; i <= end; i++ {
			out[i] = true
		}
	}
	return out
}

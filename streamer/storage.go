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
	// StorageModeDownloadAll banks the whole video file a viewer opens to disk
	// (File.Download()) — racing the swarm's decay — and keeps every watched
	// piece; space is reclaimed only when the idle reaper drops the torrent.
	StorageModeDownloadAll = "download-all"
)

// StorageConfig holds the settings needed to build a storage backend.
type StorageConfig struct {
	Mode        string
	DataDir     string
	PrefixBytes int64 // bytes pinned at the start of each video file
	SuffixBytes int64 // bytes pinned at the end of each video file (MP4 moov atom)
	CacheBytes  int64 // LRU budget for the bulk (cache / sqlite cap)
	RetainHot   bool  // keep every piece of a torrent that has an active reader
}

// torrentDropper is the optional contract a storage backend implements to free a
// torrent's on-disk data when it is removed or reaped for being idle, so disk
// isn't held by torrents nobody streams. Backends that don't implement it leave
// their data in place (e.g. capped-sqlite reclaims space on its own).
type torrentDropper interface {
	DropTorrent(ih metainfo.Hash) error
}

// readerTracker is the optional contract a storage backend implements so the
// manager can tell it where each active viewer is reading. The prefix-cache
// backend uses these positions to protect every reader's near-ahead window from
// eviction; backends that don't implement it (capped-sqlite) simply aren't
// tracked.
type readerTracker interface {
	RegisterReader(ih metainfo.Hash) uint64
	NoteReaderPos(ih metainfo.Hash, id uint64, pieceIndex int)
	UnregisterReader(ih metainfo.Hash, id uint64)
}

// newStorage builds the storage backend selected by cfg.Mode.
func newStorage(cfg StorageConfig) (storage.ClientImplCloser, error) {
	switch cfg.Mode {
	case StorageModePrefixCache, "":
		return newPrefixCacheStorage(cfg)
	case StorageModeCappedSQLite:
		return newSQLiteStorage(cfg)
	case StorageModeDownloadAll:
		return newDownloadAllStorage(cfg)
	default:
		return nil, fmt.Errorf("unknown storage mode %q (want %q, %q or %q)",
			cfg.Mode, StorageModePrefixCache, StorageModeCappedSQLite, StorageModeDownloadAll)
	}
}

// prefixPieceIndices returns the set of piece indices that overlap the first
// prefixBytes *and* the last suffixBytes of every video file in the torrent. It
// is used both by the manager (to raise piece priority so these stay warm) and
// by the prefix-cache storage (to route them to the persistent tier).
//
// The suffix matters because many torrent MP4s are not "+faststart": their moov
// atom (the index the player needs before it can render a single frame) sits at
// the *end* of the file, so http.ServeContent's first range request from the
// browser is for the tail. Pinning that tail means it is already downloading (or
// resident) instead of being fetched cold at play time.
func prefixPieceIndices(info *metainfo.Info, prefixBytes, suffixBytes int64) map[int]bool {
	out := make(map[int]bool)
	if info.PieceLength <= 0 {
		return out
	}
	addRange := func(firstByte, lastByte int64) {
		if firstByte < 0 {
			firstByte = 0
		}
		begin := int(firstByte / info.PieceLength)
		end := int(lastByte / info.PieceLength)
		for i := begin; i <= end; i++ {
			out[i] = true
		}
	}
	for _, fi := range info.UpvertedFiles() {
		if !isVideoFile(videoFileName(info, fi)) || fi.Length <= 0 {
			continue
		}
		if prefixBytes > 0 {
			prefixLen := prefixBytes
			if fi.Length < prefixLen {
				prefixLen = fi.Length
			}
			addRange(fi.TorrentOffset, fi.TorrentOffset+prefixLen-1)
		}
		if suffixBytes > 0 {
			suffixLen := suffixBytes
			if fi.Length < suffixLen {
				suffixLen = fi.Length
			}
			lastByte := fi.TorrentOffset + fi.Length - 1
			addRange(lastByte-suffixLen+1, lastByte)
		}
	}
	return out
}

// videoFileStartPieces returns the first piece index of each video file — the
// piece holding the container header the player blocks on first. The manager
// promotes these to PiecePriorityNow so time-to-first-frame isn't gated on the
// rest of the pinned window.
func videoFileStartPieces(info *metainfo.Info) []int {
	var out []int
	if info.PieceLength <= 0 {
		return out
	}
	for _, fi := range info.UpvertedFiles() {
		if !isVideoFile(videoFileName(info, fi)) || fi.Length <= 0 {
			continue
		}
		out = append(out, int(fi.TorrentOffset/info.PieceLength))
	}
	return out
}

// videoFileName resolves a file's path within the torrent, falling back to the
// info dict's name for a single-file torrent (where BestPath is empty).
func videoFileName(info *metainfo.Info, fi metainfo.FileInfo) string {
	name := strings.Join(fi.BestPath(), "/")
	if name == "" {
		return info.BestName()
	}
	return name
}

package main

import (
	"container/list"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// prefixCacheStorage is a two-tier storage backend.
//
//   - Prefix tier: pieces overlapping the first PrefixBytes of each video file are
//     stored persistently and never evicted, so playback starts instantly and
//     survives restarts.
//   - Cache tier: every other piece lives in a bounded LRU cache that evicts the
//     least-recently-read piece once the byte budget is exceeded, keeping disk use
//     bounded. Cache data is treated as ephemeral and wiped on startup.
//
// One blob file is written per piece (<dir>/<infohash>/<index>) so the cache can
// reclaim a single piece by deleting its file. A Capacity is reported so the
// torrent client treats this as capped storage and gracefully re-downloads pieces
// that are read after eviction (see reader.go readAt recovery).
type prefixCacheStorage struct {
	prefixDir   string
	cacheDir    string
	prefixBytes int64
	cacheCap    int64

	prefixCompletion storage.PieceCompletion // persistent (bolt)
	cacheCompletion  storage.PieceCompletion // ephemeral (in-memory)

	// capFunc is shared (by pointer) across all torrents so they share one global
	// request-order budget.
	capFunc func() (int64, bool)

	mu             sync.Mutex
	lru            *list.List // front = most recently used; values are *cacheEntry
	lruIndex       map[metainfo.PieceKey]*list.Element
	cacheBytes     int64
	prefixKeys     map[metainfo.PieceKey]int64 // resident complete prefix pieces -> size
	prefixResident int64
}

type cacheEntry struct {
	key  metainfo.PieceKey
	size int64
}

func newPrefixCacheStorage(cfg StorageConfig) (storage.ClientImplCloser, error) {
	prefixDir := filepath.Join(cfg.DataDir, "prefix")
	cacheDir := filepath.Join(cfg.DataDir, "cache")

	// The cache tier is ephemeral; start each run with an empty cache.
	if err := os.RemoveAll(cacheDir); err != nil {
		return nil, fmt.Errorf("clear cache dir: %w", err)
	}
	for _, d := range []string{prefixDir, cacheDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create storage dir %s: %w", d, err)
		}
	}

	prefixCompletion, err := storage.NewBoltPieceCompletion(prefixDir)
	if err != nil {
		return nil, fmt.Errorf("open prefix completion: %w", err)
	}

	s := &prefixCacheStorage{
		prefixDir:        prefixDir,
		cacheDir:         cacheDir,
		prefixBytes:      cfg.PrefixBytes,
		cacheCap:         cfg.CacheBytes,
		prefixCompletion: prefixCompletion,
		cacheCompletion:  storage.NewMapPieceCompletion(),
		lru:              list.New(),
		lruIndex:         make(map[metainfo.PieceKey]*list.Element),
		prefixKeys:       make(map[metainfo.PieceKey]int64),
	}
	s.capFunc = func() (int64, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		// Prefix pieces are highest priority and scanned first by the request
		// order, so including their resident size guarantees they always fit,
		// leaving cacheCap for the bulk windows of active readers.
		return s.cacheCap + s.prefixResident, true
	}
	return s, nil
}

func (s *prefixCacheStorage) Close() error {
	err := s.prefixCompletion.Close()
	if cerr := s.cacheCompletion.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

func (s *prefixCacheStorage) OpenTorrent(
	_ context.Context,
	info *metainfo.Info,
	infoHash metainfo.Hash,
) (storage.TorrentImpl, error) {
	prefixSet := prefixPieceIndices(info, s.prefixBytes)

	hexHash := infoHash.HexString()
	if err := os.MkdirAll(filepath.Join(s.prefixDir, hexHash), 0o755); err != nil {
		return storage.TorrentImpl{}, err
	}
	if err := os.MkdirAll(filepath.Join(s.cacheDir, hexHash), 0o755); err != nil {
		return storage.TorrentImpl{}, err
	}

	// Reconcile resident prefix size from persisted completion (e.g. after a
	// restart, when the torrent is re-added but its prefix data is still on disk).
	for idx := range prefixSet {
		key := metainfo.PieceKey{InfoHash: infoHash, Index: idx}
		if c, err := s.prefixCompletion.Get(key); err == nil && c.Ok && c.Complete {
			s.addPrefixResident(key, info.Piece(idx).Length())
		}
	}

	t := &prefixCacheTorrent{s: s, ih: infoHash, prefixSet: prefixSet}
	return storage.TorrentImpl{
		Piece:    t.Piece,
		Capacity: &s.capFunc,
		Close:    func() error { return nil },
	}, nil
}

// ---- per-torrent ----

type prefixCacheTorrent struct {
	s         *prefixCacheStorage
	ih        metainfo.Hash
	prefixSet map[int]bool
}

func (t *prefixCacheTorrent) Piece(p metainfo.Piece) storage.PieceImpl {
	idx := p.Index()
	return &prefixCachePiece{
		s:        t.s,
		key:      metainfo.PieceKey{InfoHash: t.ih, Index: idx},
		length:   p.Length(),
		isPrefix: t.prefixSet[idx],
	}
}

// ---- per-piece ----

type prefixCachePiece struct {
	s        *prefixCacheStorage
	key      metainfo.PieceKey
	length   int64
	isPrefix bool
}

func (p *prefixCachePiece) completion() storage.PieceCompletion {
	if p.isPrefix {
		return p.s.prefixCompletion
	}
	return p.s.cacheCompletion
}

func (p *prefixCachePiece) path() string {
	if p.isPrefix {
		return p.s.prefixBlobPath(p.key)
	}
	return p.s.cacheBlobPath(p.key)
}

func (p *prefixCachePiece) WriteAt(b []byte, off int64) (int, error) {
	f, err := os.OpenFile(p.path(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.WriteAt(b, off)
}

func (p *prefixCachePiece) ReadAt(b []byte, off int64) (int, error) {
	f, err := os.Open(p.path())
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := f.ReadAt(b, off)
	if !p.isPrefix && n > 0 {
		p.s.touch(p.key)
	}
	return n, err
}

func (p *prefixCachePiece) Completion() storage.Completion {
	c, err := p.completion().Get(p.key)
	if err != nil {
		return storage.Completion{Err: err}
	}
	return c
}

func (p *prefixCachePiece) MarkComplete() error {
	if err := p.completion().Set(p.key, true); err != nil {
		return err
	}
	if p.isPrefix {
		p.s.addPrefixResident(p.key, p.length)
	} else {
		p.s.addCachePiece(p.key, p.length)
	}
	return nil
}

func (p *prefixCachePiece) MarkNotComplete() error {
	if err := os.Remove(p.path()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if p.isPrefix {
		p.s.removePrefixResident(p.key)
	} else {
		p.s.removeCacheEntry(p.key)
	}
	return p.completion().Set(p.key, false)
}

// ---- storage bookkeeping ----

func (s *prefixCacheStorage) prefixBlobPath(key metainfo.PieceKey) string {
	return filepath.Join(s.prefixDir, key.InfoHash.HexString(), strconv.Itoa(key.Index))
}

func (s *prefixCacheStorage) cacheBlobPath(key metainfo.PieceKey) string {
	return filepath.Join(s.cacheDir, key.InfoHash.HexString(), strconv.Itoa(key.Index))
}

func (s *prefixCacheStorage) addPrefixResident(key metainfo.PieceKey, size int64) {
	s.mu.Lock()
	if _, ok := s.prefixKeys[key]; !ok {
		s.prefixKeys[key] = size
		s.prefixResident += size
	}
	s.mu.Unlock()
}

func (s *prefixCacheStorage) removePrefixResident(key metainfo.PieceKey) {
	s.mu.Lock()
	if size, ok := s.prefixKeys[key]; ok {
		delete(s.prefixKeys, key)
		s.prefixResident -= size
	}
	s.mu.Unlock()
}

func (s *prefixCacheStorage) touch(key metainfo.PieceKey) {
	s.mu.Lock()
	if el, ok := s.lruIndex[key]; ok {
		s.lru.MoveToFront(el)
	}
	s.mu.Unlock()
}

func (s *prefixCacheStorage) addCachePiece(key metainfo.PieceKey, size int64) {
	var evicted []metainfo.PieceKey

	s.mu.Lock()
	if el, ok := s.lruIndex[key]; ok {
		s.lru.MoveToFront(el)
		s.mu.Unlock()
		return
	}
	added := s.lru.PushFront(&cacheEntry{key: key, size: size})
	s.lruIndex[key] = added
	s.cacheBytes += size
	for s.cacheBytes > s.cacheCap {
		back := s.lru.Back()
		if back == nil || back == added {
			break // never evict the piece we just inserted
		}
		ent := back.Value.(*cacheEntry)
		s.lru.Remove(back)
		delete(s.lruIndex, ent.key)
		s.cacheBytes -= ent.size
		evicted = append(evicted, ent.key)
	}
	s.mu.Unlock()

	for _, k := range evicted {
		if err := os.Remove(s.cacheBlobPath(k)); err != nil && !os.IsNotExist(err) {
			// Best effort: the blob may already be gone.
			_ = err
		}
		_ = s.cacheCompletion.Set(k, false)
	}
}

func (s *prefixCacheStorage) removeCacheEntry(key metainfo.PieceKey) {
	s.mu.Lock()
	if el, ok := s.lruIndex[key]; ok {
		ent := el.Value.(*cacheEntry)
		s.lru.Remove(el)
		delete(s.lruIndex, key)
		s.cacheBytes -= ent.size
	}
	s.mu.Unlock()
}

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"go.etcd.io/bbolt"
)

// prefixCacheStorage is a two-tier storage backend.
//
//   - Prefix tier: pieces overlapping the first PrefixBytes of each video file are
//     stored persistently and never evicted, so playback starts instantly and
//     survives restarts.
//   - Cache tier: every other piece lives in a bounded cache that evicts pieces
//     once the byte budget is exceeded, keeping disk use bounded. Cache data is
//     treated as ephemeral and wiped on startup.
//
// Eviction is read-position aware, not plain LRU. For streaming the useful data
// is the window just *ahead* of where each reader is playing, but the torrent
// client downloads ahead opportunistically, so plain LRU would evict
// soon-to-be-played pieces and the reader would hit "no such file" / stall. So we
// track each torrent's latest read index and, when over budget, evict in this
// order: pieces of torrents with no active reader, then pieces furthest *behind*
// the read head (already played), then pieces furthest *ahead* (downloaded too
// eagerly) — always keeping the near-ahead playback window resident. This lets
// CACHE_MB be small without stalling playback.
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
	cache          map[metainfo.PieceKey]int64 // resident cache pieces -> size
	cacheBytes     int64
	readPos        map[metainfo.Hash]int       // latest read piece index per torrent
	prefixKeys     map[metainfo.PieceKey]int64 // resident complete prefix pieces -> size
	prefixResident int64
}

func newPrefixCacheStorage(cfg StorageConfig) (storage.ClientImplCloser, error) {
	prefixDir := filepath.Join(cfg.DataDir, "prefix")
	cacheDir := filepath.Join(cfg.DataDir, "cache")

	if err := os.MkdirAll(prefixDir, 0o755); err != nil {
		return nil, fmt.Errorf("create storage dir %s: %w", prefixDir, err)
	}

	// Open the persistent prefix completion DB first. Bolt takes an exclusive file
	// lock on it, so this is what enforces a single streamer instance per data
	// directory. Acquiring it BEFORE wiping the cache is deliberate: if another
	// instance is still running (e.g. an overlapping deploy), we fail fast and
	// safely here instead of racing its cache writes during the wipe below — that
	// race surfaced as "clear cache dir: ... directory not empty".
	prefixCompletion, err := storage.NewBoltPieceCompletion(prefixDir)
	if err != nil {
		if errors.Is(err, bbolt.ErrTimeout) {
			return nil, fmt.Errorf("open prefix completion: another streamer instance is already using %q (its prefix lock is held) — stop that container before starting a new one: %w", cfg.DataDir, err)
		}
		return nil, fmt.Errorf("open prefix completion: %w", err)
	}

	// The cache tier is ephemeral; start each run with an empty cache. We hold the
	// prefix lock now, so no sibling is concurrently writing here; the short retry
	// only rides out a just-exited sibling still flushing files (transient ENOTEMPTY).
	if err := removeAllWithRetry(cacheDir); err != nil {
		prefixCompletion.Close()
		return nil, fmt.Errorf("clear cache dir: %w", err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		prefixCompletion.Close()
		return nil, fmt.Errorf("create storage dir %s: %w", cacheDir, err)
	}

	s := &prefixCacheStorage{
		prefixDir:        prefixDir,
		cacheDir:         cacheDir,
		prefixBytes:      cfg.PrefixBytes,
		cacheCap:         cfg.CacheBytes,
		prefixCompletion: prefixCompletion,
		cacheCompletion:  storage.NewMapPieceCompletion(),
		cache:            make(map[metainfo.PieceKey]int64),
		readPos:          make(map[metainfo.Hash]int),
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

// removeAllWithRetry deletes dir and everything under it, retrying briefly on
// "directory not empty". os.RemoveAll is already recursive, but it can return
// ENOTEMPTY if files appear under a subdirectory while it is being emptied (e.g.
// a sibling process flushing blobs as it shuts down). A few short retries ride
// that out without masking a genuine, persistent failure.
func removeAllWithRetry(dir string) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = os.RemoveAll(dir); err == nil || !errors.Is(err, syscall.ENOTEMPTY) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
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
	if n > 0 {
		// Track where each reader is (prefix reads count too — the read head
		// starts in the prefix region) so cache eviction can protect the window
		// just ahead of it.
		p.s.noteRead(p.key)
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

// noteRead records that infoHash is currently being read at piece index, so
// eviction knows where the playback head is.
func (s *prefixCacheStorage) noteRead(key metainfo.PieceKey) {
	s.mu.Lock()
	s.readPos[key.InfoHash] = key.Index
	s.mu.Unlock()
}

func (s *prefixCacheStorage) addCachePiece(key metainfo.PieceKey, size int64) {
	var evicted []metainfo.PieceKey

	s.mu.Lock()
	if _, ok := s.cache[key]; ok {
		s.mu.Unlock()
		return
	}
	s.cache[key] = size
	s.cacheBytes += size
	for s.cacheBytes > s.cacheCap {
		victim, ok := s.pickVictim(key)
		if !ok {
			break // nothing else evictable; never evict the piece just inserted
		}
		s.cacheBytes -= s.cache[victim]
		delete(s.cache, victim)
		evicted = append(evicted, victim)
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

// pickVictim chooses the least useful resident cache piece to evict, excluding
// the just-added piece. Higher score = evict sooner: pieces of torrents with no
// active reader first, then pieces furthest behind the read head (already
// played), then pieces furthest ahead (downloaded too eagerly) — so the
// near-ahead playback window is kept. Caller must hold s.mu.
func (s *prefixCacheStorage) pickVictim(exclude metainfo.PieceKey) (metainfo.PieceKey, bool) {
	var best metainfo.PieceKey
	bestScore := -1
	found := false
	for k := range s.cache {
		if k == exclude {
			continue
		}
		if score := s.evictionScore(k); score > bestScore {
			best, bestScore, found = k, score, true
		}
	}
	return best, found
}

const (
	scoreInactiveBase = 1 << 30 // torrent has no active reader
	scoreBehindBase   = 1 << 20 // already played, behind the read head
)

func (s *prefixCacheStorage) evictionScore(k metainfo.PieceKey) int {
	r, ok := s.readPos[k.InfoHash]
	if !ok {
		return scoreInactiveBase + k.Index // no reader: most evictable
	}
	if k.Index < r {
		return scoreBehindBase + (r - k.Index) // behind: furthest-behind first
	}
	return k.Index - r // ahead: furthest-ahead first, kept until behind exhausted
}

func (s *prefixCacheStorage) removeCacheEntry(key metainfo.PieceKey) {
	s.mu.Lock()
	if size, ok := s.cache[key]; ok {
		delete(s.cache, key)
		s.cacheBytes -= size
	}
	s.mu.Unlock()
}

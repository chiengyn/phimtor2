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
// soon-to-be-played pieces and the reader would hit "no such file" / stall.
//
// Crucially this tracks *every* active reader's playhead, not a single one, so
// that many viewers watching the same file at different positions don't evict
// each other's windows — each piece is downloaded from the swarm once and served
// to all of them from disk. When over budget we evict in this order: pieces of
// torrents with no active reader, then pieces behind *every* reader (played by
// all), then pieces ahead of *every* reader (downloaded too eagerly) — always
// keeping the near-ahead window of any reader resident. This lets CACHE_MB be
// small without stalling playback.
//
// With retainHot set, pieces of any torrent that currently has a reader are
// exempt from eviction entirely (the cache may grow past CACHE_MB for hot
// titles), so late joiners never re-hit the swarm. Disk is reclaimed once the
// last reader leaves and normal bounded eviction resumes.
//
// One blob file is written per piece (<dir>/<infohash>/<index>) so the cache can
// reclaim a single piece by deleting its file. A Capacity is reported so the
// torrent client treats this as capped storage and gracefully re-downloads pieces
// that are read after eviction (see reader.go readAt recovery).
type prefixCacheStorage struct {
	prefixDir   string
	cacheDir    string
	prefixBytes int64
	suffixBytes int64
	cacheCap    int64
	retainHot   bool

	prefixCompletion storage.PieceCompletion // persistent (bolt)
	cacheCompletion  storage.PieceCompletion // ephemeral (in-memory)

	// fds caches open read handles to per-piece blobs so repeat reads under heavy
	// concurrency skip the open/close syscalls (see fdCache).
	fds *fdCache

	// capFunc is shared (by pointer) across all torrents so they share one global
	// request-order budget.
	capFunc func() (int64, bool)

	mu           sync.Mutex
	cache        map[metainfo.PieceKey]int64 // resident cache pieces -> size
	cacheBytes   int64
	nextReaderID uint64
	// readers tracks every active reader's latest read piece index per torrent, so
	// eviction can protect the near-ahead window of each (see evictionScore). A
	// torrent with an entry here is "hot": it has at least one viewer.
	readers        map[metainfo.Hash]map[uint64]int
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
		suffixBytes:      cfg.SuffixBytes,
		cacheCap:         cfg.CacheBytes,
		retainHot:        cfg.RetainHot,
		prefixCompletion: prefixCompletion,
		cacheCompletion:  storage.NewMapPieceCompletion(),
		cache:            make(map[metainfo.PieceKey]int64),
		readers:          make(map[metainfo.Hash]map[uint64]int),
		prefixKeys:       make(map[metainfo.PieceKey]int64),
		// Kept well under a typical 1024 RLIMIT_NOFILE, leaving room for peer
		// sockets and the bolt DB.
		fds: newFDCache(256),
	}
	s.capFunc = func() (int64, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		// Capacity is the shared download budget: the client stops requesting once
		// resident + highest-priority pieces exhaust it. Prefix pieces are highest
		// priority and scanned first, so including their resident size guarantees
		// they always fit, leaving cacheCap for the bulk windows of active readers.
		//
		// Under retainHot the cache may hold far more than cacheCap (we refuse to
		// evict hot torrents' pieces), so we must add what we're actually holding —
		// otherwise those retained complete pieces would eat the whole budget and
		// the client could never request the active playback window. cacheCap then
		// remains as headroom for that window on top of everything retained.
		retained := int64(0)
		if s.retainHot {
			retained = s.cacheBytes
		}
		return s.cacheCap + s.prefixResident + retained, true
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
	s.fds.closeAll()
	err := s.prefixCompletion.Close()
	if cerr := s.cacheCompletion.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

// DropTorrent frees all on-disk and in-memory state for a torrent: it forgets
// its resident pieces, clears the persistent prefix-completion entries (so a
// later re-add re-downloads instead of trusting completion for files we delete),
// closes any cached blob handles, and removes the torrent's prefix and cache
// directories. Safe to call after the torrent has been dropped from the client
// (no concurrent reads/writes for it).
func (s *prefixCacheStorage) DropTorrent(ih metainfo.Hash) error {
	hex := ih.HexString()

	s.mu.Lock()
	for key := range s.cache {
		if key.InfoHash == ih {
			s.cacheBytes -= s.cache[key]
			delete(s.cache, key)
		}
	}
	// prefixKeys mirrors the bolt-complete prefix pieces, so collecting from it
	// covers every entry we must clear from the persistent completion DB.
	var prefixPieces []metainfo.PieceKey
	for key := range s.prefixKeys {
		if key.InfoHash == ih {
			s.prefixResident -= s.prefixKeys[key]
			delete(s.prefixKeys, key)
			prefixPieces = append(prefixPieces, key)
		}
	}
	delete(s.readers, ih)
	s.mu.Unlock()

	prefixDir := filepath.Join(s.prefixDir, hex)
	cacheDir := filepath.Join(s.cacheDir, hex)
	s.fds.dropPrefix(prefixDir + string(os.PathSeparator))
	s.fds.dropPrefix(cacheDir + string(os.PathSeparator))

	for _, key := range prefixPieces {
		_ = s.prefixCompletion.Set(key, false)
	}

	var err error
	if rerr := os.RemoveAll(prefixDir); rerr != nil {
		err = rerr
	}
	if rerr := os.RemoveAll(cacheDir); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

func (s *prefixCacheStorage) OpenTorrent(
	_ context.Context,
	info *metainfo.Info,
	infoHash metainfo.Hash,
) (storage.TorrentImpl, error) {
	prefixSet := prefixPieceIndices(info, s.prefixBytes, s.suffixBytes)

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
	return p.s.fds.readAt(p.path(), b, off)
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
	p.s.fds.drop(p.path()) // blob is about to be removed/rewritten
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

// RegisterReader records a new active reader for a torrent and returns its id.
// The manager calls this when it hands out a streaming reader (see
// trackedReader) so eviction can protect that viewer's window. The returned id
// must be passed back to NoteReaderPos / UnregisterReader.
func (s *prefixCacheStorage) RegisterReader(ih metainfo.Hash) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextReaderID++
	id := s.nextReaderID
	m := s.readers[ih]
	if m == nil {
		m = make(map[uint64]int)
		s.readers[ih] = m
	}
	m[id] = 0 // assume playback starts at the file's first piece until first read
	return id
}

// NoteReaderPos records that reader id of torrent ih is now reading pieceIndex,
// so eviction knows where that viewer's playhead is.
func (s *prefixCacheStorage) NoteReaderPos(ih metainfo.Hash, id uint64, pieceIndex int) {
	s.mu.Lock()
	if m := s.readers[ih]; m != nil {
		if _, ok := m[id]; ok {
			m[id] = pieceIndex
		}
	}
	s.mu.Unlock()
}

// UnregisterReader drops a finished reader so eviction stops protecting its
// window (and the torrent goes "cold" once its last reader leaves).
func (s *prefixCacheStorage) UnregisterReader(ih metainfo.Hash, id uint64) {
	s.mu.Lock()
	if m := s.readers[ih]; m != nil {
		delete(m, id)
		if len(m) == 0 {
			delete(s.readers, ih)
		}
	}
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
		path := s.cacheBlobPath(k)
		s.fds.drop(path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			// Best effort: the blob may already be gone.
			_ = err
		}
		_ = s.cacheCompletion.Set(k, false)
	}
}

// pickVictim chooses the least useful resident cache piece to evict, excluding
// the just-added piece. Higher score = evict sooner: pieces of torrents with no
// active reader first, then pieces behind every reader (already played by all),
// then pieces furthest ahead of every reader (downloaded too eagerly) — so the
// near-ahead window of any reader is kept. Under retainHot, pieces of a torrent
// that still has a reader are not evictable at all. Caller must hold s.mu.
func (s *prefixCacheStorage) pickVictim(exclude metainfo.PieceKey) (metainfo.PieceKey, bool) {
	var best metainfo.PieceKey
	bestScore := -1
	found := false
	for k := range s.cache {
		if k == exclude {
			continue
		}
		if s.retainHot && len(s.readers[k.InfoHash]) > 0 {
			continue // hot torrent: pinned while it has a viewer
		}
		if score := s.evictionScore(k); score > bestScore {
			best, bestScore, found = k, score, true
		}
	}
	return best, found
}

const (
	scoreInactiveBase = 1 << 30 // torrent has no active reader
	scoreBehindBase   = 1 << 20 // already played, behind every read head
)

// evictionScore rates how evictable a resident cache piece is across all of its
// torrent's active readers. The piece is scored against each reader's playhead
// and we keep the most protective verdict (the minimum): a piece in the
// near-ahead window of *any* reader is protected even if it is behind or far
// ahead of the others. So many viewers at different positions in the same file
// never evict each other's windows. Caller must hold s.mu.
func (s *prefixCacheStorage) evictionScore(k metainfo.PieceKey) int {
	readers := s.readers[k.InfoHash]
	if len(readers) == 0 {
		return scoreInactiveBase + k.Index // no reader: most evictable
	}
	best := -1
	for _, r := range readers {
		var score int
		if k.Index < r {
			score = scoreBehindBase + (r - k.Index) // behind this reader
		} else {
			score = k.Index - r // ahead: near-ahead is small (most protected)
		}
		if best == -1 || score < best {
			best = score
		}
	}
	return best
}

func (s *prefixCacheStorage) removeCacheEntry(key metainfo.PieceKey) {
	s.mu.Lock()
	if size, ok := s.cache[key]; ok {
		delete(s.cache, key)
		s.cacheBytes -= size
	}
	s.mu.Unlock()
}

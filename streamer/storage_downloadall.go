package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"go.etcd.io/bbolt"
)

// downloadAllStorage is the "bank first, sweep behind" backend
// (STORAGE_MODE=download-all). It exists because torrent swarms decay: the
// prefix-cache backend only ever fetches a window around each playhead, and by
// the time a viewer reaches the later parts of a long movie the seeders that had
// those pieces may be gone. This backend instead lets the manager DownloadAll()
// the moment metadata arrives — racing the whole file onto disk while the swarm
// is still alive — and reclaims space from the *other* end: a background sweep
// (see runWatchedSweeper) deletes pieces that every active viewer has already
// played, since those are the bytes least likely to be needed again.
//
//   - Everything is stored, nothing is budget-evicted: one blob file per piece
//     under <DATA_DIR>/full/<infohash>/<index>, with completion persisted in a
//     bolt DB so banked data survives restarts (the swarm may not).
//   - Space is reclaimed by SweepWatched: pieces behind every reader's playhead
//     (minus a rewind margin, and never the pinned prefix/suffix) are deleted
//     and marked incomplete. The manager cancels those pieces on the torrent
//     first so the client doesn't immediately re-download them.
//   - A (huge) Capacity is still reported: not to throttle — the value is
//     effectively infinite so DownloadAll is never cut off — but because a
//     capped storage is what makes the client treat vanished data as evicted
//     and re-check completion on a failed read (see reader.go readAt recovery).
//     A viewer seeking back into a swept region therefore re-downloads from the
//     swarm instead of killing the stream — the accepted trade-off of sweeping.
type downloadAllStorage struct {
	dir        string
	completion storage.PieceCompletion // persistent (bolt)

	// fds pools open blob read handles, same as the prefix-cache backend.
	fds *fdCache

	capFunc func() (int64, bool)

	mu           sync.Mutex
	resident     map[metainfo.PieceKey]int64 // complete on-disk pieces -> size
	nextReaderID uint64
	// readers tracks every active reader's latest read piece index per torrent —
	// the sweep deletes only behind the *minimum* of these, so no viewer ever has
	// pieces removed ahead of (or near behind) their playhead.
	readers map[metainfo.Hash]map[uint64]int
}

// downloadAllCapacity is the Capacity reported to the torrent client. It only
// needs to be "capped" (so vanished pieces are gracefully re-downloaded) while
// never actually limiting requests — DownloadAll must be free to fetch entire
// torrents. 1 PiB is unreachable in practice and safely far from overflowing
// the client's budget arithmetic.
const downloadAllCapacity int64 = 1 << 50

func newDownloadAllStorage(cfg StorageConfig) (storage.ClientImplCloser, error) {
	dir := filepath.Join(cfg.DataDir, "full")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create storage dir %s: %w", dir, err)
	}

	// Bolt takes an exclusive lock, enforcing a single instance per data dir —
	// and, unlike the prefix-cache's cache tier, nothing here is wiped on
	// startup: the banked pieces are the whole point of this mode.
	completion, err := storage.NewBoltPieceCompletion(dir)
	if err != nil {
		if errors.Is(err, bbolt.ErrTimeout) {
			return nil, fmt.Errorf("open piece completion: another streamer instance is already using %q (its lock is held) — stop that container before starting a new one: %w", cfg.DataDir, err)
		}
		return nil, fmt.Errorf("open piece completion: %w", err)
	}

	s := &downloadAllStorage{
		dir:        dir,
		completion: completion,
		resident:   make(map[metainfo.PieceKey]int64),
		readers:    make(map[metainfo.Hash]map[uint64]int),
		fds:        newFDCache(256),
	}
	s.capFunc = func() (int64, bool) { return downloadAllCapacity, true }
	return s, nil
}

func (s *downloadAllStorage) Close() error {
	s.fds.closeAll()
	return s.completion.Close()
}

// DropTorrent frees all on-disk and in-memory state for a torrent (API delete
// and the idle reaper both land here, same contract as the prefix-cache).
// Completion entries are cleared so a later re-add re-downloads instead of
// trusting completion for blobs we are about to delete.
func (s *downloadAllStorage) DropTorrent(ih metainfo.Hash) error {
	s.mu.Lock()
	var keys []metainfo.PieceKey
	for key := range s.resident {
		if key.InfoHash == ih {
			delete(s.resident, key)
			keys = append(keys, key)
		}
	}
	delete(s.readers, ih)
	s.mu.Unlock()

	dir := filepath.Join(s.dir, ih.HexString())
	s.fds.dropPrefix(dir + string(os.PathSeparator))
	for _, key := range keys {
		_ = s.completion.Set(key, false)
	}
	return os.RemoveAll(dir)
}

func (s *downloadAllStorage) OpenTorrent(
	_ context.Context,
	info *metainfo.Info,
	infoHash metainfo.Hash,
) (storage.TorrentImpl, error) {
	if err := os.MkdirAll(filepath.Join(s.dir, infoHash.HexString()), 0o755); err != nil {
		return storage.TorrentImpl{}, err
	}

	// Reconcile resident bookkeeping from persisted completion (after a restart
	// the blobs are still on disk and must not be re-downloaded or re-counted).
	for idx := 0; idx < info.NumPieces(); idx++ {
		key := metainfo.PieceKey{InfoHash: infoHash, Index: idx}
		if c, err := s.completion.Get(key); err == nil && c.Ok && c.Complete {
			s.addResident(key, info.Piece(idx).Length())
		}
	}

	t := &downloadAllTorrent{s: s, ih: infoHash}
	return storage.TorrentImpl{
		Piece:    t.Piece,
		Capacity: &s.capFunc,
		Close:    func() error { return nil },
	}, nil
}

// ---- per-torrent / per-piece ----

type downloadAllTorrent struct {
	s  *downloadAllStorage
	ih metainfo.Hash
}

func (t *downloadAllTorrent) Piece(p metainfo.Piece) storage.PieceImpl {
	return &downloadAllPiece{
		s:      t.s,
		key:    metainfo.PieceKey{InfoHash: t.ih, Index: p.Index()},
		length: p.Length(),
	}
}

type downloadAllPiece struct {
	s      *downloadAllStorage
	key    metainfo.PieceKey
	length int64
}

func (p *downloadAllPiece) path() string { return p.s.blobPath(p.key) }

func (p *downloadAllPiece) WriteAt(b []byte, off int64) (int, error) {
	f, err := os.OpenFile(p.path(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.WriteAt(b, off)
}

func (p *downloadAllPiece) ReadAt(b []byte, off int64) (int, error) {
	return p.s.fds.readAt(p.path(), b, off)
}

func (p *downloadAllPiece) Completion() storage.Completion {
	c, err := p.s.completion.Get(p.key)
	if err != nil {
		return storage.Completion{Err: err}
	}
	if !c.Ok {
		// A piece bolt has never seen is known-incomplete, not unknown: reporting
		// unknown makes the client queue a doomed startup hash-verification of
		// every piece against an empty disk, and each failure's MarkNotComplete
		// races chunk writes already in flight (deleting their blob mid-piece).
		return storage.Completion{Complete: false, Ok: true}
	}
	return c
}

func (p *downloadAllPiece) MarkComplete() error {
	if err := p.s.completion.Set(p.key, true); err != nil {
		return err
	}
	p.s.addResident(p.key, p.length)
	return nil
}

func (p *downloadAllPiece) MarkNotComplete() error {
	p.s.fds.drop(p.path())
	if err := os.Remove(p.path()); err != nil && !os.IsNotExist(err) {
		return err
	}
	p.s.removeResident(p.key)
	return p.s.completion.Set(p.key, false)
}

// ---- bookkeeping ----

func (s *downloadAllStorage) blobPath(key metainfo.PieceKey) string {
	return filepath.Join(s.dir, key.InfoHash.HexString(), strconv.Itoa(key.Index))
}

func (s *downloadAllStorage) addResident(key metainfo.PieceKey, size int64) {
	s.mu.Lock()
	s.resident[key] = size
	s.mu.Unlock()
}

func (s *downloadAllStorage) removeResident(key metainfo.PieceKey) {
	s.mu.Lock()
	delete(s.resident, key)
	s.mu.Unlock()
}

// ---- readerTracker (same contract as the prefix-cache backend) ----

func (s *downloadAllStorage) RegisterReader(ih metainfo.Hash) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextReaderID++
	id := s.nextReaderID
	m := s.readers[ih]
	if m == nil {
		m = make(map[uint64]int)
		s.readers[ih] = m
	}
	// Assume the file's first piece until the first read: a just-joined viewer
	// must freeze the sweep at position 0, not let it delete under them.
	m[id] = 0
	return id
}

func (s *downloadAllStorage) NoteReaderPos(ih metainfo.Hash, id uint64, pieceIndex int) {
	s.mu.Lock()
	if m := s.readers[ih]; m != nil {
		if _, ok := m[id]; ok {
			m[id] = pieceIndex
		}
	}
	s.mu.Unlock()
}

func (s *downloadAllStorage) UnregisterReader(ih metainfo.Hash, id uint64) {
	s.mu.Lock()
	if m := s.readers[ih]; m != nil {
		delete(m, id)
		if len(m) == 0 {
			delete(s.readers, ih)
		}
	}
	s.mu.Unlock()
}

// ---- watchedSweeper ----

// MinReaderPiece returns the earliest playhead among the torrent's active
// readers, or false when it has none (with no viewers nothing is "watched", so
// the sweep leaves the banked file alone and the idle reaper decides its fate).
func (s *downloadAllStorage) MinReaderPiece(ih metainfo.Hash) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.readers[ih]
	if len(m) == 0 {
		return 0, false
	}
	min := -1
	for _, pos := range m {
		if min == -1 || pos < min {
			min = pos
		}
	}
	return min, true
}

// SweepWatched deletes every resident piece of ih strictly below cutoff, except
// those in keep (the pinned prefix/suffix). The caller must have already
// cancelled the range on the torrent so the client doesn't re-request the
// pieces it is told (on its next completion re-check) are gone. Completion is
// flipped before the blob is removed so a crash between the two can never leave
// a piece recorded complete with its data missing.
func (s *downloadAllStorage) SweepWatched(ih metainfo.Hash, cutoff int, keep map[int]bool) (freedBytes int64, pieces int) {
	s.mu.Lock()
	var victims []metainfo.PieceKey
	for key, size := range s.resident {
		if key.InfoHash != ih || key.Index >= cutoff || keep[key.Index] {
			continue
		}
		victims = append(victims, key)
		freedBytes += size
		delete(s.resident, key)
	}
	s.mu.Unlock()

	for _, k := range victims {
		_ = s.completion.Set(k, false)
		path := s.blobPath(k)
		s.fds.drop(path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			_ = err // best effort: the blob may already be gone
		}
	}
	return freedBytes, len(victims)
}

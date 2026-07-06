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

// downloadAllStorage is the "keep what's watched" backend
// (STORAGE_MODE=download-all). It exists because torrent swarms decay: the
// prefix-cache backend only ever fetches a window around each playhead, and by
// the time a viewer reaches the later parts of a long movie the seeders that had
// those pieces may be gone. This backend instead banks the whole file a viewer
// is watching (the manager calls File.Download() when a stream opens — see
// GetFileReader) while the swarm is still alive, and then simply keeps it all:
//
//   - Everything watched is stored, nothing is evicted: one blob file per piece
//     under <DATA_DIR>/full/<infohash>/<index>, with completion persisted in a
//     bolt DB so banked data survives restarts (the swarm may not). Watched
//     pieces are never deleted behind the playhead — a viewer can seek back with
//     no re-download.
//   - Space is reclaimed only when a torrent goes idle: the idle reaper drops it
//     (DropTorrent) after IDLE_TTL_MIN with no viewers, deleting the whole
//     torrent's blobs at once.
//   - A (huge) Capacity is still reported: not to throttle — the value is
//     effectively infinite so downloads are never cut off — but because a capped
//     storage is what makes the client treat a vanished blob (e.g. lost to a
//     crash mid-write) as evicted and re-check completion on a failed read (see
//     reader.go readAt recovery), re-downloading it instead of killing the
//     stream.
type downloadAllStorage struct {
	dir        string
	completion storage.PieceCompletion // persistent (bolt)

	// fds pools open blob read handles, same as the prefix-cache backend.
	fds *fdCache

	capFunc func() (int64, bool)

	// numPieces records each open torrent's piece count so DropTorrent can clear
	// its bolt completion entries (a re-add must re-download, not trust completion
	// for blobs we are about to delete).
	mu        sync.Mutex
	numPieces map[metainfo.Hash]int
}

// downloadAllCapacity is the Capacity reported to the torrent client. It only
// needs to be "capped" (so a vanished piece is gracefully re-downloaded) while
// never actually limiting requests — a watched file must be free to download in
// full. 1 PiB is unreachable in practice and safely far from overflowing the
// client's budget arithmetic.
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
		numPieces:  make(map[metainfo.Hash]int),
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
	n := s.numPieces[ih]
	delete(s.numPieces, ih)
	s.mu.Unlock()

	dir := filepath.Join(s.dir, ih.HexString())
	s.fds.dropPrefix(dir + string(os.PathSeparator))
	for idx := 0; idx < n; idx++ {
		_ = s.completion.Set(metainfo.PieceKey{InfoHash: ih, Index: idx}, false)
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

	// Record the piece count so DropTorrent can clear this torrent's completion
	// entries. (After a restart the blobs and their completion are still on disk
	// and are trusted as-is — nothing is re-downloaded or re-counted.)
	s.mu.Lock()
	s.numPieces[infoHash] = info.NumPieces()
	s.mu.Unlock()

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
		s:   t.s,
		key: metainfo.PieceKey{InfoHash: t.ih, Index: p.Index()},
	}
}

type downloadAllPiece struct {
	s   *downloadAllStorage
	key metainfo.PieceKey
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
	return p.s.completion.Set(p.key, true)
}

func (p *downloadAllPiece) MarkNotComplete() error {
	p.s.fds.drop(p.path())
	if err := os.Remove(p.path()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return p.s.completion.Set(p.key, false)
}

// ---- bookkeeping ----

func (s *downloadAllStorage) blobPath(key metainfo.PieceKey) string {
	return filepath.Join(s.dir, key.InfoHash.HexString(), strconv.Itoa(key.Index))
}

package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// minReadaheadBytes is the floor for per-reader readahead. Readahead is scaled
// down as the number of concurrent readers rises (see readaheadFor) to bound
// total memory under load, but never below this so playback stays smooth.
const minReadaheadBytes = 4 << 20

type TorrentInfo struct {
	InfoHash       string     `json:"infoHash"`
	Name           string     `json:"name"`
	TotalBytes     int64      `json:"totalBytes"`
	BytesCompleted int64      `json:"bytesCompleted"`
	Files          []FileInfo `json:"files"`
}

// TorrentStats is the live, fast-changing metrics for one torrent, served by
// the dedicated stats endpoint (separate from the relatively static
// list/structure returned by TorrentInfo).
type TorrentStats struct {
	InfoHash       string `json:"infoHash"`
	TotalBytes     int64  `json:"totalBytes"`
	BytesCompleted int64  `json:"bytesCompleted"`
	Seeding        bool   `json:"seeding"`

	// Instantaneous transfer rates in bytes/second, sampled between successive
	// stats calls (see rateSample).
	DownloadSpeed int64 `json:"downloadSpeed"`
	UploadSpeed   int64 `json:"uploadSpeed"`

	// Peer gauges from the torrent client (TorrentGauges).
	TotalPeers       int `json:"totalPeers"`
	PendingPeers     int `json:"pendingPeers"`
	ActivePeers      int `json:"activePeers"`
	ConnectedSeeders int `json:"connectedSeeders"`
	HalfOpenPeers    int `json:"halfOpenPeers"`

	PiecesComplete int `json:"piecesComplete"`
	PiecesTotal    int `json:"piecesTotal"`
}

type FileInfo struct {
	Index          int    `json:"index"`
	Path           string `json:"path"`
	Length         int64  `json:"length"`
	BytesCompleted int64  `json:"bytesCompleted"`
	IsVideo        bool   `json:"isVideo"`
}

var videoExtensions = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".webm": true,
	".mov": true, ".m4v": true, ".wmv": true, ".flv": true,
	".ts": true, ".m2ts": true,
}

func isVideoFile(path string) bool {
	return videoExtensions[strings.ToLower(filepath.Ext(path))]
}

// rateSample is the previous transfer reading for one torrent, used to derive
// instantaneous download/upload speed from the monotonically increasing byte
// counters between two successive ListTorrents calls.
type rateSample struct {
	bytesRead    int64
	bytesWritten int64
	at           time.Time
}

type TorrentManager struct {
	client      *torrent.Client
	storageImpl storage.ClientImplCloser
	tracker     readerTracker // nil if the storage backend doesn't track readers
	mu          sync.RWMutex
	torrents    map[string]*torrent.Torrent
	readahead   int64
	prefixBytes int64

	// activeReaders counts streaming readers currently open across all torrents,
	// used to scale per-reader readahead down under load (readaheadFor).
	activeReaders atomic.Int64

	rateMu  sync.Mutex
	samples map[string]rateSample
}

func NewTorrentManager(storageCfg StorageConfig, readaheadBytes int64, maxConns int) (*TorrentManager, error) {
	storageImpl, err := newStorage(storageCfg)
	if err != nil {
		return nil, fmt.Errorf("create storage: %w", err)
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = storageCfg.DataDir
	cfg.NoUpload = false
	cfg.DefaultStorage = storageImpl

	// Raise peer-connection limits above the library defaults (50/25/100/500) so a
	// hot torrent fills its cache from the swarm fast enough to feed many viewers.
	if maxConns > 0 {
		cfg.EstablishedConnsPerTorrent = maxConns
		cfg.HalfOpenConnsPerTorrent = maxConns / 2
		cfg.TotalHalfOpenConns = maxConns
		if hw := maxConns * 4; cfg.TorrentPeersHighWater < hw {
			cfg.TorrentPeersHighWater = hw
		}
	}

	client, err := torrent.NewClient(cfg)
	if err != nil {
		_ = storageImpl.Close()
		return nil, fmt.Errorf("create torrent client: %w", err)
	}

	m := &TorrentManager{
		client:      client,
		storageImpl: storageImpl,
		torrents:    make(map[string]*torrent.Torrent),
		readahead:   readaheadBytes,
		prefixBytes: storageCfg.PrefixBytes,
		samples:     make(map[string]rateSample),
	}
	// Only the prefix-cache backend tracks reader positions; capped-sqlite does not.
	if rt, ok := storageImpl.(readerTracker); ok {
		m.tracker = rt
	}
	return m, nil
}

// pinPrefixPieces raises the priority of the pieces that hold the first
// prefixBytes of each video file so the client keeps them resident (and
// pre-fetches them while idle), making playback start instantly. Must be called
// after the torrent's metadata is available.
func (m *TorrentManager) pinPrefixPieces(t *torrent.Torrent) {
	info := t.Info()
	if info == nil {
		return
	}
	for idx := range prefixPieceIndices(info, m.prefixBytes) {
		t.Piece(idx).SetPriority(torrent.PiecePriorityHigh)
	}
}

func (m *TorrentManager) AddMagnet(magnetURI string) (string, error) {
	t, err := m.client.AddMagnet(magnetURI)
	if err != nil {
		return "", fmt.Errorf("add magnet: %w", err)
	}

	infoHash := t.InfoHash().HexString()

	m.mu.Lock()
	m.torrents[infoHash] = t
	m.mu.Unlock()

	go func() {
		<-t.GotInfo()
		m.pinPrefixPieces(t)
	}()

	return infoHash, nil
}

func (m *TorrentManager) AddTorrentFile(r io.Reader) (string, error) {
	mi, err := metainfo.Load(r)
	if err != nil {
		return "", fmt.Errorf("parse torrent file: %w", err)
	}

	t, err := m.client.AddTorrent(mi)
	if err != nil {
		return "", fmt.Errorf("add torrent: %w", err)
	}

	infoHash := t.InfoHash().HexString()

	m.mu.Lock()
	m.torrents[infoHash] = t
	m.mu.Unlock()

	go func() {
		<-t.GotInfo()
		m.pinPrefixPieces(t)
	}()

	return infoHash, nil
}

func (m *TorrentManager) GetTorrent(infoHash string) (*torrent.Torrent, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.torrents[infoHash]
	return t, ok
}

func (m *TorrentManager) ListTorrents() []TorrentInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]TorrentInfo, 0, len(m.torrents))
	for hash, t := range m.torrents {
		result = append(result, buildTorrentInfo(hash, t))
	}
	return result
}

// GetTorrentInfo returns the file structure for a single torrent. It is the
// single-torrent counterpart to ListTorrents: callers that already know the
// infoHash (e.g. the admin add-torrent page polling for metadata) use this
// instead of fetching and scanning the whole list. The bool is false when no
// such torrent is tracked.
func (m *TorrentManager) GetTorrentInfo(infoHash string) (TorrentInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	t, ok := m.torrents[infoHash]
	if !ok {
		return TorrentInfo{}, false
	}
	return buildTorrentInfo(infoHash, t), true
}

// buildTorrentInfo snapshots one torrent's structure. Before metadata arrives
// (t.Info() == nil) the name is "Loading..." and Files is empty, so a poller can
// tell "not ready yet" from "ready with files".
func buildTorrentInfo(hash string, t *torrent.Torrent) TorrentInfo {
	info := TorrentInfo{
		InfoHash: hash,
		Name:     "Loading...",
	}

	if t.Info() != nil {
		info.Name = t.Name()
		info.TotalBytes = t.Length()
		info.BytesCompleted = t.BytesCompleted()

		files := t.Files()
		info.Files = make([]FileInfo, len(files))
		for i, f := range files {
			info.Files[i] = FileInfo{
				Index:          i,
				Path:           f.Path(),
				Length:         f.Length(),
				BytesCompleted: f.BytesCompleted(),
				IsVideo:        isVideoFile(f.Path()),
			}
		}
	}

	return info
}

// GetStats returns the live transfer/peer metrics for a single torrent. It is
// intentionally separate from ListTorrents so the UI can poll the fast-changing
// numbers for the torrents it cares about without re-fetching the (mostly
// static) file structure.
func (m *TorrentManager) GetStats(infoHash string) (TorrentStats, bool) {
	t, ok := m.GetTorrent(infoHash)
	if !ok {
		return TorrentStats{}, false
	}

	stats := t.Stats()
	g := stats.TorrentGauges
	st := TorrentStats{
		InfoHash:         infoHash,
		Seeding:          t.Seeding(),
		TotalPeers:       g.TotalPeers,
		PendingPeers:     g.PendingPeers,
		ActivePeers:      g.ActivePeers,
		ConnectedSeeders: g.ConnectedSeeders,
		HalfOpenPeers:    g.HalfOpenPeers,
		PiecesComplete:   g.PiecesComplete,
	}

	st.DownloadSpeed, st.UploadSpeed = m.sampleRates(
		infoHash,
		stats.BytesReadData.Int64(),
		stats.BytesWrittenData.Int64(),
		time.Now(),
	)

	if t.Info() != nil {
		st.TotalBytes = t.Length()
		st.BytesCompleted = t.BytesCompleted()
		st.PiecesTotal = t.NumPieces()
	}

	return st, true
}

// sampleRates derives instantaneous download/upload speeds (bytes/sec) from the
// cumulative read/written byte counters by diffing against the previous sample
// for this torrent, then records the new sample. The first call for a torrent
// (or after a counter reset) reports zero.
func (m *TorrentManager) sampleRates(hash string, read, written int64, now time.Time) (dl, ul int64) {
	m.rateMu.Lock()
	defer m.rateMu.Unlock()

	if prev, ok := m.samples[hash]; ok {
		dt := now.Sub(prev.at).Seconds()
		if dt > 0 {
			if d := read - prev.bytesRead; d > 0 {
				dl = int64(float64(d) / dt)
			}
			if d := written - prev.bytesWritten; d > 0 {
				ul = int64(float64(d) / dt)
			}
		}
	}
	m.samples[hash] = rateSample{bytesRead: read, bytesWritten: written, at: now}
	return dl, ul
}

func (m *TorrentManager) GetFileReader(infoHash string, fileIndex int) (io.ReadSeekCloser, *FileInfo, error) {
	t, ok := m.GetTorrent(infoHash)
	if !ok {
		return nil, nil, fmt.Errorf("torrent not found")
	}

	if t.Info() == nil {
		return nil, nil, fmt.Errorf("torrent metadata not ready")
	}

	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return nil, nil, fmt.Errorf("file index out of range")
	}

	f := files[fileIndex]
	reader := f.NewReader()
	reader.SetResponsive()

	// Scale readahead down as concurrent readers rise so N viewers don't each
	// reserve the full READAHEAD_MB (16 MB × N would be unbounded memory).
	count := m.activeReaders.Add(1)
	reader.SetReadahead(m.readaheadFor(count))

	fi := &FileInfo{
		Index:          fileIndex,
		Path:           f.Path(),
		Length:         f.Length(),
		BytesCompleted: f.BytesCompleted(),
		IsVideo:        isVideoFile(f.Path()),
	}

	tr := &trackedReader{
		ReadSeekCloser: reader,
		onDone:         func() { m.activeReaders.Add(-1) },
		pieceLength:    t.Info().PieceLength,
		fileOffset:     f.Offset(),
		ih:             t.InfoHash(),
	}
	// Register the reader's playhead with the storage so eviction protects this
	// viewer's window (no-op when the backend doesn't track readers).
	if m.tracker != nil {
		tr.tracker = m.tracker
		tr.id = m.tracker.RegisterReader(tr.ih)
	}
	return tr, fi, nil
}

// readaheadFor returns the readahead to give a reader when active is the current
// number of open readers. It divides the configured budget across readers but
// never drops below minReadaheadBytes (or the configured value, if smaller).
func (m *TorrentManager) readaheadFor(active int64) int64 {
	ra := m.readahead
	if active > 1 {
		ra = m.readahead / active
	}
	floor := int64(minReadaheadBytes)
	if floor > m.readahead {
		floor = m.readahead
	}
	if ra < floor {
		ra = floor
	}
	return ra
}

func (m *TorrentManager) RemoveTorrent(infoHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.torrents[infoHash]
	if !ok {
		return fmt.Errorf("torrent not found")
	}

	t.Drop()
	delete(m.torrents, infoHash)

	m.rateMu.Lock()
	delete(m.samples, infoHash)
	m.rateMu.Unlock()
	return nil
}

func (m *TorrentManager) Close() {
	m.client.Close()
	if m.storageImpl != nil {
		_ = m.storageImpl.Close()
	}
}

// trackedReader wraps a torrent file reader so the manager and storage learn
// where the viewer is playing. ReadAt-level reads can't tell readers apart, so
// position is reported here: each Read/Seek maps the file-relative offset to a
// global piece index and notes it; Close releases the reader's slot. tracker may
// be nil (storage backend that doesn't track) — onDone always runs to keep the
// active-reader count accurate.
type trackedReader struct {
	io.ReadSeekCloser
	tracker     readerTracker // may be nil
	onDone      func()        // decrements the manager's active-reader count
	ih          metainfo.Hash
	id          uint64
	fileOffset  int64 // torrent-wide byte offset of this file's first byte
	pieceLength int64
	pos         int64 // current read offset within the file
	closed      bool
}

func (tr *trackedReader) note() {
	if tr.tracker == nil || tr.pieceLength <= 0 {
		return
	}
	tr.tracker.NoteReaderPos(tr.ih, tr.id, int((tr.fileOffset+tr.pos)/tr.pieceLength))
}

func (tr *trackedReader) Read(p []byte) (int, error) {
	tr.note() // note the piece we're about to consume before advancing
	n, err := tr.ReadSeekCloser.Read(p)
	tr.pos += int64(n)
	return n, err
}

func (tr *trackedReader) Seek(offset int64, whence int) (int64, error) {
	newPos, err := tr.ReadSeekCloser.Seek(offset, whence)
	if err == nil {
		tr.pos = newPos
		tr.note()
	}
	return newPos, err
}

func (tr *trackedReader) Close() error {
	if !tr.closed {
		tr.closed = true
		if tr.tracker != nil {
			tr.tracker.UnregisterReader(tr.ih, tr.id)
		}
		if tr.onDone != nil {
			tr.onDone()
		}
	}
	return tr.ReadSeekCloser.Close()
}

package main

import (
	"fmt"
	"io"
	"log"
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

// FilePieces is the per-piece download map for one file within a torrent. Pieces
// is one character per piece spanning the file, in order: '0' missing, '1'
// partial (some but not all bytes obtained), '2' complete. It stays compact
// (one byte per piece) so the admin watch page can poll it and render a piece
// map without shipping an object per piece. The first/last entries may cover
// only part of their piece — a shared piece straddling an adjacent file — but
// their state is still that whole piece's.
type FilePieces struct {
	InfoHash       string `json:"infoHash"`
	FileIndex      int    `json:"fileIndex"`
	NumPieces      int    `json:"numPieces"`
	PiecesComplete int    `json:"piecesComplete"`
	PieceLength    int64  `json:"pieceLength"`
	Length         int64  `json:"length"`
	Pieces         string `json:"pieces"`
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
	suffixBytes int64

	// eager (download-all mode) banks the whole video file a viewer opens to disk
	// (File.Download() in GetFileReader) while the swarm is still alive, and keeps
	// it — only the watched file is fetched, never the whole (possibly multi-file)
	// torrent, and watched pieces are never swept. Idle torrents are reclaimed
	// wholesale by the idle reaper.
	eager bool

	// activeReaders counts streaming readers currently open across all torrents,
	// used to scale per-reader readahead down under load (readaheadFor).
	activeReaders atomic.Int64

	// bytesServed is the cumulative count of HTTP bytes written to viewers across
	// all streams (direct + transcoded), sampled by AggregateStats into an egress
	// rate the manager uses for bandwidth-aware placement.
	bytesServed atomic.Int64

	// activity tracks per-torrent streaming usage so idle torrents can be reaped
	// (dropped) to free disk and peer connections. See reaper.go.
	idleTTL    time.Duration
	activityMu sync.Mutex
	activity   map[string]*torrentActivity

	// stallTimeout, if >0, drops a watched torrent that can't make download
	// progress (dead swarm); see runStallChecker.
	stallTimeout time.Duration

	// bgDone stops the background reaper/stall goroutines on Close.
	bgDone chan struct{}

	rateMu  sync.Mutex
	samples map[string]rateSample
}

func NewTorrentManager(storageCfg StorageConfig, readaheadBytes int64, maxConns int, idleTTL time.Duration, maxUnverifiedBytes int64, stallTimeout time.Duration) (*TorrentManager, error) {
	storageImpl, err := newStorage(storageCfg)
	if err != nil {
		return nil, fmt.Errorf("create storage: %w", err)
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = storageCfg.DataDir
	cfg.NoUpload = false
	cfg.DefaultStorage = storageImpl

	// MaxUnverifiedBytes is a single budget the request engine shares across ALL
	// torrents: it walks pieces in global priority order and stops requesting the
	// moment the cumulative in-flight/unverified bytes reach this cap (see
	// internal/request-strategy/order.go). The library default (64 MiB) is small,
	// and a stalled torrent (dead/slow swarm) keeps a few high-priority pieces —
	// its reader window plus our pinned-High prefix — permanently "requested but
	// never verified". Partial pieces sort to the front of the global order, so
	// that one torrent pins the whole budget and the scan breaks before reaching
	// healthy torrents' pieces: they get zero requests and can't play until the
	// stalled torrent is removed. We bound in-flight per torrent via the peer/conn
	// limits instead, so disable this cross-torrent cap (0 = unlimited). Storage is
	// still bounded by the cache budget (capFunc). Set MAX_UNVERIFIED_MB > 0 to
	// re-impose a global cap.
	cfg.MaxUnverifiedBytes = maxUnverifiedBytes

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
		client:          client,
		storageImpl:     storageImpl,
		torrents:        make(map[string]*torrent.Torrent),
		readahead:       readaheadBytes,
		prefixBytes:     storageCfg.PrefixBytes,
		suffixBytes:     storageCfg.SuffixBytes,
		eager:           storageCfg.Mode == StorageModeDownloadAll,
		idleTTL:         idleTTL,
		stallTimeout:    stallTimeout,
		activity:        make(map[string]*torrentActivity),
		samples:         make(map[string]rateSample),
	}
	// Only the prefix-cache backend tracks reader positions (to protect each
	// viewer's window from eviction); download-all and capped-sqlite do not.
	if rt, ok := storageImpl.(readerTracker); ok {
		m.tracker = rt
	}
	// Start the background reaper (idle torrents) and stall checker (dead-swarm
	// torrents a viewer is waiting on). They share one stop channel.
	if idleTTL > 0 || stallTimeout > 0 {
		m.bgDone = make(chan struct{})
	}
	if idleTTL > 0 {
		go m.runReaper()
	}
	if stallTimeout > 0 {
		go m.runStallChecker()
	}
	return m, nil
}

// pinPrefixPieces raises the priority of the pieces that hold the first
// prefixBytes and last suffixBytes of each video file so the client keeps them
// resident (and pre-fetches them while idle), making playback start instantly.
// The suffix covers the MP4 moov atom of non-faststart files, which the browser
// range-requests before it can render a frame. The first piece of each video —
// the container header — is promoted further to PiecePriorityNow so it arrives
// ahead of the rest of the pinned window. Must be called after the torrent's
// metadata is available.
func (m *TorrentManager) pinPrefixPieces(t *torrent.Torrent) {
	info := t.Info()
	if info == nil {
		return
	}
	for idx := range prefixPieceIndices(info, m.prefixBytes, m.suffixBytes) {
		t.Piece(idx).SetPriority(torrent.PiecePriorityHigh)
	}
	for _, idx := range videoFileStartPieces(info) {
		t.Piece(idx).SetPriority(torrent.PiecePriorityNow)
	}
}

// watchOnInfo waits for a just-added torrent's metadata, then pins the pieces
// playback blocks on first so time-to-first-frame isn't gated on the rest of the
// download. In download-all mode this pre-warm is skipped: nothing is fetched
// until a viewer opens a file (then that whole file is banked on demand via
// File.Download() in GetFileReader, and its header/moov are prioritized by the
// responsive reader), so a multi-file torrent never pulls a byte of a file
// nobody is watching.
func (m *TorrentManager) watchOnInfo(t *torrent.Torrent) {
	if m.eager {
		return
	}
	go func() {
		<-t.GotInfo()
		m.pinPrefixPieces(t)
	}()
}

// PrioritizeSeek raises a window of pieces at byteOffset within a file to High so
// that after a viewer seeks (a Range request to an un-downloaded region) the
// post-seek playback buffer fills ahead of background work — the pinned
// prefix/suffix of this torrent and the pieces of any other torrent. The
// responsive reader already requests the exact seek position at top priority;
// this just widens the high-priority window so playback doesn't immediately
// re-buffer once the seek lands. The dominant seek latency is still the swarm
// fetching those cold pieces, which no priority change removes. Pieces already
// complete cost nothing, so a no-op when the target is already downloaded.
func (m *TorrentManager) PrioritizeSeek(infoHash string, fileIndex int, byteOffset int64) {
	t, ok := m.GetTorrent(infoHash)
	if !ok || t.Info() == nil {
		return
	}
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return
	}
	pieceLen := t.Info().PieceLength
	if pieceLen <= 0 {
		return
	}
	start := int((files[fileIndex].Offset() + byteOffset) / pieceLen)
	// Cover roughly one readahead's worth of pieces ahead of the seek point,
	// capped so tiny pieces can't fan out into hundreds of High requests.
	window := int(m.readahead/pieceLen) + 1
	if window > 32 {
		window = 32
	}
	for i := start; i < start+window && i < t.NumPieces(); i++ {
		t.Piece(i).SetPriority(torrent.PiecePriorityHigh)
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
	m.touchActivity(infoHash) // start the idle clock from when it was added

	m.watchOnInfo(t)

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
	m.touchActivity(infoHash) // start the idle clock from when it was added

	m.watchOnInfo(t)

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

// GetFilePieces snapshots the per-piece download state of a single file within a
// torrent (see FilePieces). The bool is false when no such torrent/file is
// tracked, or when the torrent's metadata hasn't arrived yet (piece layout
// unknown). File.State() already returns exactly the pieces spanning this file,
// with the byte-length each contributes, so a multi-file torrent reports only
// the watched file's pieces — not the whole torrent's.
func (m *TorrentManager) GetFilePieces(infoHash string, fileIndex int) (FilePieces, bool) {
	t, ok := m.GetTorrent(infoHash)
	if !ok || t.Info() == nil {
		return FilePieces{}, false
	}
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return FilePieces{}, false
	}
	f := files[fileIndex]

	states := f.State()
	var b strings.Builder
	b.Grow(len(states))
	complete := 0
	for _, ps := range states {
		switch {
		case ps.Complete:
			b.WriteByte('2')
			complete++
		case ps.Partial:
			b.WriteByte('1')
		default:
			b.WriteByte('0')
		}
	}

	return FilePieces{
		InfoHash:       infoHash,
		FileIndex:      fileIndex,
		NumPieces:      len(states),
		PiecesComplete: complete,
		PieceLength:    t.Info().PieceLength,
		Length:         f.Length(),
		Pieces:         b.String(),
	}, true
}

// LoadStats summarizes an instance's current load for the manager's placement
// decision: the live viewer egress rate plus the torrent count. The manager
// polls this to balance new torrents by the bandwidth actually being served to
// viewers — a truer load signal than the raw number of torrents, since a few
// actively-watched torrents serve far more than many idle ones.
type LoadStats struct {
	TorrentCount int   `json:"torrentCount"`
	EgressSpeed  int64 `json:"egressSpeed"` // HTTP bytes/sec served to viewers
}

// aggregateRateKey is the sampleRates key for the cumulative egress counter.
// A real infohash is 40 hex chars, so "*" can never collide with one.
const aggregateRateKey = "*"

// AddBytesServed records HTTP bytes written to a viewer; the stream handler feeds
// it through its response-writer wrapper for every byte served (direct or
// transcoded).
func (m *TorrentManager) AddBytesServed(n int64) { m.bytesServed.Add(n) }

// AggregateStats returns the instance-wide load: the viewer egress rate (HTTP
// bytes/sec served to browsers, derived by diffing the cumulative bytesServed
// counter the same way per-torrent rates are) and the current torrent count.
// Like per-torrent stats, the first call after start reports a zero rate; the
// manager polls it on a fixed cadence, so each reading is the average over one
// poll interval.
func (m *TorrentManager) AggregateStats() LoadStats {
	egress, _ := m.sampleRates(aggregateRateKey, m.bytesServed.Load(), 0, time.Now())
	m.mu.RLock()
	n := len(m.torrents)
	m.mu.RUnlock()
	return LoadStats{TorrentCount: n, EgressSpeed: egress}
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

	// download-all mode: bank the whole file this viewer is watching. Only the
	// opened file is fetched (never the rest of a multi-file torrent), and its
	// pieces are kept until the torrent goes idle and the reaper drops it. Cheap
	// to call again if another reader opens the same file — priority is idempotent.
	if m.eager {
		f.Download()
	}

	reader := f.NewReader()
	reader.SetResponsive()

	// Scale readahead down as concurrent readers rise so N viewers don't each
	// reserve the full READAHEAD_MB (16 MB × N would be unbounded memory).
	count := m.activeReaders.Add(1)
	reader.SetReadahead(m.readaheadFor(count))

	// Mark the torrent in use so the idle reaper won't drop it while it streams.
	m.markReaderOpened(infoHash)

	fi := &FileInfo{
		Index:          fileIndex,
		Path:           f.Path(),
		Length:         f.Length(),
		BytesCompleted: f.BytesCompleted(),
		IsVideo:        isVideoFile(f.Path()),
	}

	tr := &trackedReader{
		ReadSeekCloser: reader,
		onDone: func() {
			m.activeReaders.Add(-1)
			m.markReaderClosed(infoHash)
		},
		pieceLength: t.Info().PieceLength,
		fileOffset:  f.Offset(),
		ih:          t.InfoHash(),
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
	t, ok := m.torrents[infoHash]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("torrent not found")
	}
	ih := t.InfoHash()
	t.Drop() // stops the torrent and closes its peer connections
	delete(m.torrents, infoHash)
	m.mu.Unlock()

	m.rateMu.Lock()
	delete(m.samples, infoHash)
	m.rateMu.Unlock()

	m.activityMu.Lock()
	delete(m.activity, infoHash)
	m.activityMu.Unlock()

	// Free the torrent's on-disk blobs once it's detached from the client. Done
	// outside m.mu since it touches the filesystem.
	if d, ok := m.storageImpl.(torrentDropper); ok {
		if err := d.DropTorrent(ih); err != nil {
			log.Printf("free storage for %s: %v", infoHash, err)
		}
	}
	return nil
}

func (m *TorrentManager) Close() {
	if m.bgDone != nil {
		close(m.bgDone)
	}
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

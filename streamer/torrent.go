package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type TorrentInfo struct {
	InfoHash       string `json:"infoHash"`
	Name           string `json:"name"`
	TotalBytes     int64  `json:"totalBytes"`
	BytesCompleted int64  `json:"bytesCompleted"`
	Seeding        bool   `json:"seeding"`

	// Instantaneous transfer rates in bytes/second, sampled between successive
	// list calls (see rateSample).
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

	Files []FileInfo `json:"files"`
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
	mu          sync.RWMutex
	torrents    map[string]*torrent.Torrent
	readahead   int64
	prefixBytes int64

	rateMu  sync.Mutex
	samples map[string]rateSample
}

func NewTorrentManager(storageCfg StorageConfig, readaheadBytes int64) (*TorrentManager, error) {
	storageImpl, err := newStorage(storageCfg)
	if err != nil {
		return nil, fmt.Errorf("create storage: %w", err)
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = storageCfg.DataDir
	cfg.NoUpload = false
	cfg.DefaultStorage = storageImpl

	client, err := torrent.NewClient(cfg)
	if err != nil {
		_ = storageImpl.Close()
		return nil, fmt.Errorf("create torrent client: %w", err)
	}

	return &TorrentManager{
		client:      client,
		storageImpl: storageImpl,
		torrents:    make(map[string]*torrent.Torrent),
		readahead:   readaheadBytes,
		prefixBytes: storageCfg.PrefixBytes,
		samples:     make(map[string]rateSample),
	}, nil
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

	now := time.Now()
	result := make([]TorrentInfo, 0, len(m.torrents))
	for hash, t := range m.torrents {
		info := TorrentInfo{
			InfoHash: hash,
			Name:     "Loading...",
		}

		stats := t.Stats()
		g := stats.TorrentGauges
		info.TotalPeers = g.TotalPeers
		info.PendingPeers = g.PendingPeers
		info.ActivePeers = g.ActivePeers
		info.ConnectedSeeders = g.ConnectedSeeders
		info.HalfOpenPeers = g.HalfOpenPeers
		info.PiecesComplete = g.PiecesComplete
		info.Seeding = t.Seeding()

		info.DownloadSpeed, info.UploadSpeed = m.sampleRates(
			hash,
			stats.BytesReadData.Int64(),
			stats.BytesWrittenData.Int64(),
			now,
		)

		if t.Info() != nil {
			info.Name = t.Name()
			info.TotalBytes = t.Length()
			info.BytesCompleted = t.BytesCompleted()
			info.PiecesTotal = t.NumPieces()

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

		result = append(result, info)
	}
	return result
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
	reader.SetReadahead(m.readahead)

	fi := &FileInfo{
		Index:          fileIndex,
		Path:           f.Path(),
		Length:         f.Length(),
		BytesCompleted: f.BytesCompleted(),
		IsVideo:        isVideoFile(f.Path()),
	}

	return reader, fi, nil
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

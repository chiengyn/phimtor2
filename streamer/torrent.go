package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type TorrentInfo struct {
	InfoHash       string     `json:"infoHash"`
	Name           string     `json:"name"`
	TotalBytes     int64      `json:"totalBytes"`
	BytesCompleted int64      `json:"bytesCompleted"`
	Files          []FileInfo `json:"files"`
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

type TorrentManager struct {
	client      *torrent.Client
	storageImpl storage.ClientImplCloser
	mu          sync.RWMutex
	torrents    map[string]*torrent.Torrent
	readahead   int64
	prefixBytes int64
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

	result := make([]TorrentInfo, 0, len(m.torrents))
	for hash, t := range m.torrents {
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

		result = append(result, info)
	}
	return result
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

// GetFileInfo returns metadata for a file without opening a reader.
func (m *TorrentManager) GetFileInfo(infoHash string, fileIndex int) (*FileInfo, error) {
	t, ok := m.GetTorrent(infoHash)
	if !ok {
		return nil, fmt.Errorf("torrent not found")
	}
	if t.Info() == nil {
		return nil, fmt.Errorf("torrent metadata not ready")
	}
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return nil, fmt.Errorf("file index out of range")
	}
	f := files[fileIndex]
	return &FileInfo{
		Index:          fileIndex,
		Path:           f.Path(),
		Length:         f.Length(),
		BytesCompleted: f.BytesCompleted(),
		IsVideo:        isVideoFile(f.Path()),
	}, nil
}

// MovieHash computes the OpenSubtitles (OSDb) hash for a file: the file size
// plus the 64-bit little-endian sum of its first and last 64 KiB. Because the
// tail of a streaming torrent is often not on disk yet, the read is bounded by
// ctx — on timeout the reader is closed to unblock the pending read and an
// error is returned, letting the caller fall back to a text query.
func (m *TorrentManager) MovieHash(ctx context.Context, infoHash string, fileIndex int) (string, error) {
	t, ok := m.GetTorrent(infoHash)
	if !ok {
		return "", fmt.Errorf("torrent not found")
	}
	if t.Info() == nil {
		return "", fmt.Errorf("torrent metadata not ready")
	}
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return "", fmt.Errorf("file index out of range")
	}

	f := files[fileIndex]
	const chunk = 65536
	size := f.Length()
	if size < chunk {
		return "", fmt.Errorf("file too small for moviehash")
	}

	reader := f.NewReader()
	reader.SetResponsive()
	reader.SetReadahead(chunk)

	type result struct {
		hash string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		hash := uint64(size)
		buf := make([]byte, chunk)

		if _, err := io.ReadFull(reader, buf); err != nil {
			ch <- result{err: err}
			return
		}
		hash += sumLE64(buf)

		if _, err := reader.Seek(-chunk, io.SeekEnd); err != nil {
			ch <- result{err: err}
			return
		}
		if _, err := io.ReadFull(reader, buf); err != nil {
			ch <- result{err: err}
			return
		}
		hash += sumLE64(buf)

		ch <- result{hash: fmt.Sprintf("%016x", hash)}
	}()

	select {
	case <-ctx.Done():
		reader.Close() // unblock the pending Read in the goroutine
		return "", ctx.Err()
	case r := <-ch:
		reader.Close()
		return r.hash, r.err
	}
}

func sumLE64(b []byte) uint64 {
	var s uint64
	for i := 0; i+8 <= len(b); i += 8 {
		s += binary.LittleEndian.Uint64(b[i : i+8])
	}
	return s
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
	return nil
}

func (m *TorrentManager) Close() {
	m.client.Close()
	if m.storageImpl != nil {
		_ = m.storageImpl.Close()
	}
}

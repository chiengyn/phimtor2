package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
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
	client    *torrent.Client
	mu        sync.RWMutex
	torrents  map[string]*torrent.Torrent
	readahead int64
}

func NewTorrentManager(dataDir string, readaheadBytes int64) (*TorrentManager, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	cfg.NoUpload = false

	client, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("create torrent client: %w", err)
	}

	return &TorrentManager{
		client:    client,
		torrents:  make(map[string]*torrent.Torrent),
		readahead: readaheadBytes,
	}, nil
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

	go func() { <-t.GotInfo() }()

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

	go func() { <-t.GotInfo() }()

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
}

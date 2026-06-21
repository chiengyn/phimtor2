package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// torrentfile.go decodes just enough of the bencode .torrent format to pull
// out the info hash and file list, so the crawl jobs (crawl.go) can turn a
// YTS torrent URL into a Video without registering it with the live
// streamer (which would mean waiting on DHT/swarm metadata for every
// discovered movie).

var torrentFileHTTPClient = &http.Client{Timeout: 30 * time.Second}

// fetchTorrentFile downloads a .torrent file from the given URL.
func fetchTorrentFile(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := torrentFileHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch torrent file: status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// torrentFileEntry is one file inside a parsed .torrent's info dict.
type torrentFileEntry struct {
	Index  int
	Path   string
	Length int64
}

// parseTorrentFile decodes a raw .torrent file and returns its info hash
// (lowercase hex, matching torrent_sources.info_hash) plus its file list.
func parseTorrentFile(data []byte) (infoHash string, files []torrentFileEntry, err error) {
	d := &bencodeDecoder{data: data}
	top, err := d.decodeValue()
	if err != nil {
		return "", nil, fmt.Errorf("decode torrent: %w", err)
	}
	topDict, ok := top.(map[string]interface{})
	if !ok {
		return "", nil, errors.New("decode torrent: top-level value is not a dict")
	}
	if d.infoRaw == nil {
		return "", nil, errors.New("decode torrent: missing info dict")
	}
	infoDict, ok := topDict["info"].(map[string]interface{})
	if !ok {
		return "", nil, errors.New("decode torrent: info is not a dict")
	}

	sum := sha1.Sum(d.infoRaw)
	infoHash = hex.EncodeToString(sum[:])

	if filesVal, ok := infoDict["files"].([]interface{}); ok {
		for i, fv := range filesVal {
			fd, ok := fv.(map[string]interface{})
			if !ok {
				continue
			}
			length, _ := fd["length"].(int64)
			pathParts, _ := fd["path"].([]interface{})
			parts := make([]string, 0, len(pathParts))
			for _, p := range pathParts {
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
			}
			files = append(files, torrentFileEntry{Index: i, Path: strings.Join(parts, "/"), Length: length})
		}
	} else {
		// Single-file torrent: one implicit file at index 0.
		name, _ := infoDict["name"].(string)
		length, _ := infoDict["length"].(int64)
		files = append(files, torrentFileEntry{Index: 0, Path: name, Length: length})
	}

	return infoHash, files, nil
}

// videoFileExtensions mirrors streamer/torrent.go's videoExtensions. Not
// shared as a package — admin and streamer duplicate small lookup tables
// like this rather than share code, same as their domain models.
var videoFileExtensions = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".webm": true,
	".mov": true, ".m4v": true, ".wmv": true, ".flv": true,
	".ts": true, ".m2ts": true,
}

// pickVideoFile returns the largest video file in a parsed torrent's file
// list, ignoring samples/subtitles/nfo files that often ride along.
func pickVideoFile(files []torrentFileEntry) (torrentFileEntry, bool) {
	var best torrentFileEntry
	found := false
	for _, f := range files {
		if !videoFileExtensions[strings.ToLower(filepath.Ext(f.Path))] {
			continue
		}
		if !found || f.Length > best.Length {
			best = f
			found = true
		}
	}
	return best, found
}

// --- minimal bencode decoder ---

// bencodeDecoder decodes a bencoded byte slice into plain Go values
// (string, int64, []interface{}, map[string]interface{}). It additionally
// records the raw bytes of the dict value under the "info" key, wherever it
// occurs, since the BitTorrent info hash is the SHA-1 of those exact bytes
// (not a re-encoding of the decoded value).
type bencodeDecoder struct {
	data    []byte
	pos     int
	infoRaw []byte
}

func (d *bencodeDecoder) decodeValue() (interface{}, error) {
	if d.pos >= len(d.data) {
		return nil, io.ErrUnexpectedEOF
	}
	switch d.data[d.pos] {
	case 'i':
		return d.decodeInt()
	case 'l':
		return d.decodeList()
	case 'd':
		return d.decodeDict()
	default:
		return d.decodeString()
	}
}

func (d *bencodeDecoder) decodeInt() (int64, error) {
	end := bytes.IndexByte(d.data[d.pos+1:], 'e')
	if end < 0 {
		return 0, errors.New("bencode: unterminated integer")
	}
	end += d.pos + 1
	n, err := strconv.ParseInt(string(d.data[d.pos+1:end]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bencode: invalid integer: %w", err)
	}
	d.pos = end + 1
	return n, nil
}

func (d *bencodeDecoder) decodeString() (string, error) {
	colon := bytes.IndexByte(d.data[d.pos:], ':')
	if colon < 0 {
		return "", errors.New("bencode: malformed string length")
	}
	colon += d.pos
	n, err := strconv.Atoi(string(d.data[d.pos:colon]))
	if err != nil || n < 0 {
		return "", errors.New("bencode: malformed string length")
	}
	start := colon + 1
	end := start + n
	if end > len(d.data) {
		return "", io.ErrUnexpectedEOF
	}
	d.pos = end
	return string(d.data[start:end]), nil
}

func (d *bencodeDecoder) decodeList() ([]interface{}, error) {
	d.pos++ // 'l'
	var list []interface{}
	for {
		if d.pos >= len(d.data) {
			return nil, io.ErrUnexpectedEOF
		}
		if d.data[d.pos] == 'e' {
			d.pos++
			return list, nil
		}
		v, err := d.decodeValue()
		if err != nil {
			return nil, err
		}
		list = append(list, v)
	}
}

func (d *bencodeDecoder) decodeDict() (map[string]interface{}, error) {
	d.pos++ // 'd'
	m := map[string]interface{}{}
	for {
		if d.pos >= len(d.data) {
			return nil, io.ErrUnexpectedEOF
		}
		if d.data[d.pos] == 'e' {
			d.pos++
			return m, nil
		}
		key, err := d.decodeString()
		if err != nil {
			return nil, fmt.Errorf("bencode: dict key: %w", err)
		}
		valStart := d.pos
		val, err := d.decodeValue()
		if err != nil {
			return nil, fmt.Errorf("bencode: dict value for %q: %w", key, err)
		}
		if key == "info" {
			d.infoRaw = d.data[valStart:d.pos]
		}
		m[key] = val
	}
}

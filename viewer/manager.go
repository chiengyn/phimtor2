package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// managerClient is the viewer's server-to-server client for the streamer manager.
// The browser never calls this; the viewer mediates the add so only the chosen
// streamer's stats + stream endpoints need to be browser-reachable. The manager
// returns the owning streamer's public URL, which the viewer hands to the browser
// per prepare.
type managerClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newManagerClient(internalURL, token string) *managerClient {
	return &managerClient{
		baseURL: strings.TrimRight(internalURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// addTorrent registers a source via the manager and returns its info hash plus
// the owning streamer's public base URL. When torrentFile is non-nil it is sent
// as a multipart .torrent upload (with the magnet alongside so the manager can
// still dedupe by infohash), letting the streamer load the metainfo directly and
// skip the DHT metadata fetch; otherwise a plain magnet is sent. The manager is
// idempotent — re-adding a tracked source returns the existing placement.
func (c *managerClient) addTorrent(ctx context.Context, magnet string, torrentFile []byte) (infoHash, streamerPublicURL string, err error) {
	var body []byte
	var contentType string
	if len(torrentFile) > 0 {
		body, contentType, err = buildTorrentMultipart(magnet, torrentFile)
	} else {
		contentType = "application/json"
		body, err = json.Marshal(map[string]string{"magnet": magnet})
	}
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/torrents", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", contentType)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("manager add torrent: %s: %s", resp.Status, managerErrMsg(resp.Body))
	}

	var out struct {
		InfoHash          string `json:"infoHash"`
		StreamerPublicURL string `json:"streamerPublicURL"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("decode manager response: %w", err)
	}
	if out.InfoHash == "" {
		return "", "", fmt.Errorf("manager returned empty info hash")
	}
	return out.InfoHash, strings.TrimRight(out.StreamerPublicURL, "/"), nil
}

// deleteTorrent asks the manager to drop a torrent, which routes the delete to
// its owning streamer. It is idempotent on the manager side — deleting a torrent
// no streamer owns still succeeds — so a duplicate leave/sweep is harmless.
func (c *managerClient) deleteTorrent(ctx context.Context, infoHash string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/api/torrents/"+url.PathEscape(infoHash), nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("manager delete torrent: %s", resp.Status)
	}
	return nil
}

// buildTorrentMultipart builds a multipart/form-data body carrying the raw
// .torrent bytes in a "torrent" file part plus the magnet in a "magnet" field.
// The streamer prefers the file part (skipping DHT metadata resolution); the
// magnet rides along so the manager can still read the infohash off it to dedupe
// placement onto the streamer that already owns the torrent.
func buildTorrentMultipart(magnet string, torrentFile []byte) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if magnet != "" {
		if err = mw.WriteField("magnet", magnet); err != nil {
			return nil, "", err
		}
	}
	fw, err := mw.CreateFormFile("torrent", "source.torrent")
	if err != nil {
		return nil, "", err
	}
	if _, err = fw.Write(torrentFile); err != nil {
		return nil, "", err
	}
	if err = mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// managerErrMsg best-effort extracts the {"error":...} message from a failed
// manager response, falling back to the raw body.
func managerErrMsg(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 4<<10))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(data))
}

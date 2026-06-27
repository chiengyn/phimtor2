package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// addTorrent registers a magnet via the manager and returns its info hash plus
// the owning streamer's public base URL. The manager is idempotent — re-adding a
// tracked magnet returns the existing placement.
func (c *managerClient) addTorrent(ctx context.Context, magnet string) (infoHash, streamerPublicURL string, err error) {
	body, err := json.Marshal(map[string]string{"magnet": magnet})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/torrents", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
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

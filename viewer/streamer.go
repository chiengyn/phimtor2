package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// streamerClient is the viewer's server-to-server client for the streamer's
// add-torrent API. The browser never calls this endpoint directly; the viewer
// mediates it so that only the streamer's stats + stream endpoints need to be
// publicly reachable.
type streamerClient struct {
	baseURL string
	http    *http.Client
}

func newStreamerClient(internalURL string) *streamerClient {
	return &streamerClient{
		baseURL: strings.TrimRight(internalURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// addTorrent registers a magnet with the streamer and returns its info hash. The
// streamer treats this idempotently — re-adding a tracked magnet just returns the
// existing hash.
func (c *streamerClient) addTorrent(ctx context.Context, magnet string) (string, error) {
	body, err := json.Marshal(map[string]string{"magnet": magnet})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/torrents", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg := streamerErrMsg(resp.Body)
		return "", fmt.Errorf("streamer add torrent: %s: %s", resp.Status, msg)
	}

	var out struct {
		InfoHash string `json:"infoHash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode streamer response: %w", err)
	}
	if out.InfoHash == "" {
		return "", fmt.Errorf("streamer returned empty info hash")
	}
	return out.InfoHash, nil
}

// streamerErrMsg best-effort extracts the {"error":...} message from a failed
// streamer response, falling back to the raw body.
func streamerErrMsg(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 4<<10))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(data))
}

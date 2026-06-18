package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const osBaseURL = "https://api.opensubtitles.com/api/v1"

// OpenSubtitlesClient is a thin proxy over the OpenSubtitles REST API. The
// browser never talks to OpenSubtitles directly: the Api-Key would leak and
// the API does not allow authenticated cross-origin calls, so all requests are
// funneled through the admin server. Unlike the streamer (which had the torrent
// file on hand), the admin matches by text query + season/episode only — there
// is no moviehash, since the admin holds no torrent data.
type OpenSubtitlesClient struct {
	apiKey    string
	userAgent string
	username  string
	password  string
	http      *http.Client

	mu    sync.Mutex
	token string // cached login JWT; raises the download quota
}

func NewOpenSubtitlesClient(apiKey, userAgent, username, password string) *OpenSubtitlesClient {
	return &OpenSubtitlesClient{
		apiKey:    apiKey,
		userAgent: userAgent,
		username:  username,
		password:  password,
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *OpenSubtitlesClient) Enabled() bool { return c != nil && c.apiKey != "" }

// Subtitle is the trimmed shape returned to the UI.
type Subtitle struct {
	FileID        int    `json:"fileId"`
	Language      string `json:"language"`
	Release       string `json:"release"`
	DownloadCount int    `json:"downloadCount"`
}

type SearchParams struct {
	Query     string
	Languages string
	Season    int
	Episode   int
}

func (c *OpenSubtitlesClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, osBaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Api-Key", c.apiKey)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// Search returns subtitle candidates matched by text query (and optional
// season/episode for TV).
func (c *OpenSubtitlesClient) Search(ctx context.Context, p SearchParams) ([]Subtitle, error) {
	q := url.Values{}
	if p.Query != "" {
		q.Set("query", p.Query)
	}
	if p.Languages != "" {
		q.Set("languages", p.Languages)
	}
	if p.Season > 0 {
		q.Set("season_number", strconv.Itoa(p.Season))
	}
	if p.Episode > 0 {
		q.Set("episode_number", strconv.Itoa(p.Episode))
	}

	req, err := c.newRequest(ctx, http.MethodGet, "/subtitles?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search: %s", osErr(resp))
	}

	var out struct {
		Data []struct {
			Attributes struct {
				Language      string `json:"language"`
				DownloadCount int    `json:"download_count"`
				Release       string `json:"release"`
				Files         []struct {
					FileID int `json:"file_id"`
				} `json:"files"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	subs := make([]Subtitle, 0, len(out.Data))
	for _, d := range out.Data {
		a := d.Attributes
		if len(a.Files) == 0 {
			continue
		}
		subs = append(subs, Subtitle{
			FileID:        a.Files[0].FileID,
			Language:      a.Language,
			Release:       a.Release,
			DownloadCount: a.DownloadCount,
		})
	}
	return subs, nil
}

// Download resolves a file_id to subtitle text in WebVTT form. The API can emit
// WebVTT directly via sub_format; we convert from SubRip as a safety net.
func (c *OpenSubtitlesClient) Download(ctx context.Context, fileID int) (string, error) {
	body, _ := json.Marshal(map[string]any{"file_id": fileID, "sub_format": "webvtt"})
	req, err := c.newRequest(ctx, http.MethodPost, "/download", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	if tok := c.ensureToken(ctx); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: %s", osErr(resp))
	}

	var out struct {
		Link    string `json:"link"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode download response: %w", err)
	}
	if out.Link == "" {
		msg := out.Message
		if msg == "" {
			msg = "no download link returned (quota may be exhausted)"
		}
		return "", fmt.Errorf("download: %s", msg)
	}

	freq, err := http.NewRequestWithContext(ctx, http.MethodGet, out.Link, nil)
	if err != nil {
		return "", err
	}
	freq.Header.Set("User-Agent", c.userAgent)
	fresp, err := c.http.Do(freq)
	if err != nil {
		return "", err
	}
	defer fresp.Body.Close()
	if fresp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch subtitle: %s", fresp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(fresp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	text := string(data)
	if !strings.HasPrefix(strings.TrimSpace(text), "WEBVTT") {
		text = srtToVTT(text)
	}
	return text, nil
}

// ensureToken logs in once (if credentials are set) and caches the JWT. Failures
// are non-fatal: downloads still work anonymously, just with a lower quota.
func (c *OpenSubtitlesClient) ensureToken(ctx context.Context) string {
	if c.username == "" || c.password == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" {
		return c.token
	}

	body, _ := json.Marshal(map[string]string{"username": c.username, "password": c.password})
	req, err := c.newRequest(ctx, http.MethodPost, "/login", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var out struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) == nil {
		c.token = out.Token
	}
	return c.token
}

func osErr(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if msg := strings.TrimSpace(string(b)); msg != "" {
		return resp.Status + ": " + msg
	}
	return resp.Status
}

var srtTimestampRe = regexp.MustCompile(`(\d{2}:\d{2}:\d{2}),(\d{3})`)

// srtToVTT mirrors the browser-side conversion (see templates/watch.html) as a
// fallback for when the API hands back SubRip instead of WebVTT.
func srtToVTT(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = srtTimestampRe.ReplaceAllString(s, "$1.$2")
	return "WEBVTT\n\n" + s
}

var episodeRe = regexp.MustCompile(`(?i)\bS(\d{1,2})E(\d{1,2})\b`)

// parseSubtitleQuery turns a video file name into a search title plus optional
// season/episode, e.g. "Show.Name.S01E02.1080p.mkv" -> ("Show Name", 1, 2).
func parseSubtitleQuery(fileName string) (title string, season, episode int) {
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	base = strings.NewReplacer(".", " ", "_", " ").Replace(base)
	base = strings.TrimSpace(base)

	if m := episodeRe.FindStringSubmatchIndex(base); m != nil {
		season, _ = strconv.Atoi(base[m[2]:m[3]])
		episode, _ = strconv.Atoi(base[m[4]:m[5]])
		if t := strings.TrimSpace(base[:m[0]]); t != "" {
			return t, season, episode
		}
	}
	return base, season, episode
}

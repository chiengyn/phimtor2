package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// yts.go is a minimal client for YTS's public movie API
// (https://yts.mx/api/v2), used by the crawl jobs (crawl.go) to discover
// movies and their torrents. Unlike the streamer, this client never adds a
// torrent to a swarm — it only reads YTS's catalog metadata.

// YTSClient talks to YTS's list_movies.json endpoint. The base URL is
// mutable at runtime (settable from the admin UI, see crawl.html) since
// YTS's official domain has gone down before and mirrors rotate — not
// persisted, so a restart falls back to YTS_BASE_URL.
type YTSClient struct {
	mu      sync.RWMutex
	baseURL string
	http    *http.Client
}

// NewYTSClient builds a client against baseURL (e.g. https://yts.mx/api/v2).
func NewYTSClient(baseURL string) *YTSClient {
	return &YTSClient{baseURL: strings.TrimSuffix(baseURL, "/"), http: &http.Client{Timeout: 30 * time.Second}}
}

// BaseURL returns the current base URL.
func (c *YTSClient) BaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL
}

// SetBaseURL changes the base URL used by subsequent ListMovies calls.
func (c *YTSClient) SetBaseURL(baseURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseURL = strings.TrimSuffix(baseURL, "/")
}

// YTSMovie is one result from list_movies.json, with its torrents already
// filtered to qualities this admin's resolution enum understands.
type YTSMovie struct {
	ID       int64
	ImdbCode string
	Title    string
	Language string // ISO 639-1, e.g. "en"
	Torrents []YTSTorrent
}

// YTSTorrent is one torrent for a YTSMovie. Resolution is one of this
// admin's resolution enum values ("720p"/"1080p"/"2160p") — torrents with an
// unrecognized quality (3D, etc.) are dropped before this type is built.
type YTSTorrent struct {
	Hash       string
	Resolution string
	SizeBytes  int64
	URL        string // direct .torrent download link
}

// YTSListParams configures a list_movies.json query.
type YTSListParams struct {
	QueryTerm string // e.g. an IMDb id, for an exact lookup
	SortBy    string // "date_added" | "" (relevance)
	OrderBy   string // "desc" | "asc"
	Limit     int
	Page      int
}

// ListMovies calls list_movies.json and maps results into YTSMovie/YTSTorrent.
func (c *YTSClient) ListMovies(ctx context.Context, params YTSListParams) ([]YTSMovie, error) {
	u, err := url.Parse(c.BaseURL() + "/list_movies.json")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	if params.QueryTerm != "" {
		q.Set("query_term", params.QueryTerm)
	}
	if params.SortBy != "" {
		q.Set("sort_by", params.SortBy)
	}
	if params.OrderBy != "" {
		q.Set("order_by", params.OrderBy)
	}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Page > 0 {
		q.Set("page", strconv.Itoa(params.Page))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("yts list_movies: status %d", resp.StatusCode)
	}

	var body struct {
		Status string `json:"status"`
		Data   struct {
			Movies []struct {
				ID       int64  `json:"id"`
				ImdbCode string `json:"imdb_code"`
				Title    string `json:"title"`
				Language string `json:"language"`
				Torrents []struct {
					URL       string `json:"url"`
					Hash      string `json:"hash"`
					Quality   string `json:"quality"`
					SizeBytes int64  `json:"size_bytes"`
				} `json:"torrents"`
			} `json:"movies"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode yts response: %w", err)
	}
	if body.Status != "ok" {
		return nil, fmt.Errorf("yts list_movies: status %q", body.Status)
	}

	movies := make([]YTSMovie, 0, len(body.Data.Movies))
	for _, m := range body.Data.Movies {
		movie := YTSMovie{ID: m.ID, ImdbCode: m.ImdbCode, Title: m.Title, Language: m.Language}
		for _, t := range m.Torrents {
			res := ytsQualityToResolution(t.Quality)
			if res == "" {
				continue
			}
			movie.Torrents = append(movie.Torrents, YTSTorrent{
				Hash:       strings.ToLower(t.Hash),
				Resolution: res,
				SizeBytes:  t.SizeBytes,
				URL:        t.URL,
			})
		}
		movies = append(movies, movie)
	}
	return movies, nil
}

// ytsQualityToResolution maps a YTS torrent "quality" string (e.g. "1080p",
// "1080p.x265", "3D") to this admin's videos.resolution enum, dropping
// qualities it doesn't store.
func ytsQualityToResolution(quality string) string {
	switch {
	case strings.HasPrefix(quality, "2160p"):
		return "2160p"
	case strings.HasPrefix(quality, "1080p"):
		return "1080p"
	case strings.HasPrefix(quality, "720p"):
		return "720p"
	default:
		return ""
	}
}

package main

import (
	"archive/zip"
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
	"time"
)

const subsourceBaseURL = "https://api.subsource.net/api/v1"

// SubSourceClient is a server-side proxy over the subsource.net public API
// (https://subsource.net/api-docs). Like the OpenSubtitles client it sits behind
// the SubtitleProvider interface so the admin can offer it as an alternative
// source; the browser never talks to subsource directly (the API key is sent in
// the X-API-Key header and must not leak).
//
// subsource models a "movie" (a film or one season of a series) that owns many
// subtitle entries, and serves each subtitle as a ZIP archive. So Search is two
// hops — resolve the query to a movie id, then list that movie's subtitles — and
// Download fetches the ZIP and extracts the subtitle file from it (picking the
// right episode for season packs; see the fileID encoding below).
type SubSourceClient struct {
	apiKey    string
	userAgent string
	baseURL   string
	http      *http.Client
}

func NewSubSourceClient(apiKey, userAgent string) *SubSourceClient {
	if userAgent == "" {
		userAgent = "phimtor2 v1.0"
	}
	return &SubSourceClient{
		apiKey:    apiKey,
		userAgent: userAgent,
		baseURL:   subsourceBaseURL,
		http:      &http.Client{Timeout: 45 * time.Second},
	}
}

func (c *SubSourceClient) Name() string { return "subsource" }

func (c *SubSourceClient) Enabled() bool { return c != nil && c.apiKey != "" }

func (c *SubSourceClient) newRequest(ctx context.Context, path string, q url.Values) (*http.Request, error) {
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// Search resolves the text query to a subsource movie id and returns that
// movie's subtitle entries, filtered by language. For TV (season set) the movie
// lookup is scoped to the matching season; the episode (when known) is folded
// into each result's FileID so Download can pull the right file out of a season
// pack ZIP.
func (c *SubSourceClient) Search(ctx context.Context, p SearchParams) ([]SubtitleResult, error) {
	movieID, err := c.findMovie(ctx, p)
	if err != nil {
		return nil, err
	}
	if movieID == 0 {
		return nil, nil
	}

	q := url.Values{}
	q.Set("movieId", strconv.Itoa(movieID))
	if lang := toSubsourceLang(p.Languages); lang != "" {
		q.Set("language", lang)
	}
	q.Set("sort", "popular")
	q.Set("limit", "100")

	req, err := c.newRequest(ctx, "/subtitles", q)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subsource subtitles: %s", subsourceErr(resp))
	}

	var out struct {
		Data []struct {
			SubtitleID  int      `json:"subtitleId"`
			Language    string   `json:"language"`
			ReleaseInfo []string `json:"releaseInfo"`
			Downloads   int      `json:"downloads"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode subsource subtitles: %w", err)
	}

	subs := make([]SubtitleResult, 0, len(out.Data))
	for _, d := range out.Data {
		if d.SubtitleID == 0 {
			continue
		}
		subs = append(subs, SubtitleResult{
			Provider:      c.Name(),
			FileID:        subsourceFileID(d.SubtitleID, p.Episode),
			Language:      fromSubsourceLang(d.Language),
			Release:       strings.Join(d.ReleaseInfo, " "),
			DownloadCount: d.Downloads,
		})
	}
	return subs, nil
}

// findMovie resolves a text query to a subsource movie id. subsource returns one
// entry per film and per series-season, so when a season is known we prefer the
// entry that matches it. A type-filtered search that comes back empty is retried
// without the filter, since our movie/series guess can be wrong.
func (c *SubSourceClient) findMovie(ctx context.Context, p SearchParams) (int, error) {
	wantSeries := p.Season > 0 || p.Episode > 0

	typeFilter := "movie"
	if wantSeries {
		typeFilter = "series"
	}
	movies, err := c.searchMovies(ctx, p, typeFilter)
	if err != nil {
		return 0, err
	}
	if len(movies) == 0 {
		// Our movie/series guess may be wrong; retry across all types.
		if movies, err = c.searchMovies(ctx, p, ""); err != nil {
			return 0, err
		}
	}
	if len(movies) == 0 {
		return 0, nil
	}

	if p.Season > 0 {
		for _, m := range movies {
			if m.Season == p.Season {
				return m.MovieID, nil
			}
		}
	}
	return movies[0].MovieID, nil
}

type subsourceMovie struct {
	MovieID int    `json:"movieId"`
	Type    string `json:"type"`
	Season  int    `json:"season"`
}

func (c *SubSourceClient) searchMovies(ctx context.Context, p SearchParams, typeFilter string) ([]subsourceMovie, error) {
	q := url.Values{}
	q.Set("searchType", "text")
	q.Set("q", p.Query)
	if typeFilter != "" {
		q.Set("type", typeFilter)
	}
	if p.Season > 0 {
		q.Set("season", strconv.Itoa(p.Season))
	}

	req, err := c.newRequest(ctx, "/movies/search", q)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subsource search: %s", subsourceErr(resp))
	}

	var out struct {
		Data []subsourceMovie `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode subsource search: %w", err)
	}
	return out.Data, nil
}

// maxSubsourceZip caps the downloaded ZIP archive.
const maxSubsourceZip = 16 << 20

// Download fetches a subtitle's ZIP archive and extracts a WebVTT track from it.
// fileID is "<subtitleId>" for a movie or "<subtitleId>:<episode>" for a series
// season pack, where the episode picks the matching file out of the archive.
func (c *SubSourceClient) Download(ctx context.Context, fileID string) ([]byte, string, error) {
	subID, episode := parseSubsourceFileID(fileID)
	if subID <= 0 {
		return nil, "", fmt.Errorf("invalid subtitle id %q", fileID)
	}

	req, err := c.newRequest(ctx, "/subtitles/"+strconv.Itoa(subID)+"/download", nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("subsource download: %s", subsourceErr(resp))
	}

	zipData, err := io.ReadAll(io.LimitReader(resp.Body, maxSubsourceZip+1))
	if err != nil {
		return nil, "", err
	}
	if len(zipData) > maxSubsourceZip {
		return nil, "", fmt.Errorf("subsource archive too large")
	}

	text, err := extractSubtitleFromZip(zipData, episode)
	if err != nil {
		return nil, "", err
	}
	if !strings.HasPrefix(strings.TrimSpace(text), "WEBVTT") {
		text = srtToVTT(text)
	}
	return []byte(text), "vtt", nil
}

// extractSubtitleFromZip pulls a single .srt/.vtt track out of a subsource ZIP.
// When episode > 0 it prefers the file whose name encodes that episode (season
// packs bundle one file per episode); otherwise it returns the first track.
func extractSubtitleFromZip(zipData []byte, episode int) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return "", fmt.Errorf("open subsource archive: %w", err)
	}

	var candidates []*zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(f.Name)) {
		case ".srt", ".vtt":
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no .srt/.vtt file in subsource archive")
	}

	chosen := candidates[0]
	if episode > 0 {
		for _, f := range candidates {
			if fileNameMatchesEpisode(f.Name, episode) {
				chosen = f
				break
			}
		}
	}

	rc, err := chosen.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxSubsourceZip))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// subsourceErr renders a non-2xx subsource response, surfacing the JSON
// {"error","message"} body when present.
func subsourceErr(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil {
		if msg := strings.TrimSpace(e.Message); msg != "" {
			return resp.Status + ": " + msg
		}
		if msg := strings.TrimSpace(e.Error); msg != "" {
			return resp.Status + ": " + msg
		}
	}
	if msg := strings.TrimSpace(string(b)); msg != "" {
		return resp.Status + ": " + msg
	}
	return resp.Status
}

// subsourceFileID encodes the subtitle id (and episode, for season packs) into
// the opaque FileID the UI round-trips back to Download.
func subsourceFileID(subID, episode int) string {
	if episode > 0 {
		return fmt.Sprintf("%d:%d", subID, episode)
	}
	return strconv.Itoa(subID)
}

func parseSubsourceFileID(fileID string) (subID, episode int) {
	if id, ep, ok := strings.Cut(fileID, ":"); ok {
		subID, _ = strconv.Atoi(strings.TrimSpace(id))
		episode, _ = strconv.Atoi(strings.TrimSpace(ep))
		return subID, episode
	}
	subID, _ = strconv.Atoi(strings.TrimSpace(fileID))
	return subID, 0
}

// fileNameMatchesEpisode reports whether a subtitle filename looks like it
// belongs to the given episode, matching "E07"/"e7"/"x07"/"episode 7" style
// markers with optional leading zeros. The trailing boundary keeps E1 from
// matching E10.
func fileNameMatchesEpisode(name string, episode int) bool {
	re := regexp.MustCompile(fmt.Sprintf(`(?i)(?:e|x|ep|episode)[ ._-]*0*%d(\b|[^0-9])`, episode))
	return re.MatchString(name)
}

// subsourceLangNames maps the ISO-ish codes the UI sends to the full language
// names subsource expects. Unmapped values are passed through (lowercased), so a
// caller can also type a full subsource name directly.
var subsourceLangNames = map[string]string{
	"en": "english",
	"vi": "vietnamese",
	"es": "spanish",
	"fr": "french",
	"de": "german",
	"it": "italian",
	"pt": "portuguese",
	"ru": "russian",
	"ja": "japanese",
	"ko": "korean",
	"zh": "chinese",
	"ar": "arabic",
	"hi": "hindi",
	"th": "thai",
	"id": "indonesian",
	"ms": "malay",
	"nl": "dutch",
	"pl": "polish",
	"tr": "turkish",
	"sv": "swedish",
	"fa": "farsi_persian",
}

// subsourceLangCodes is the reverse of subsourceLangNames, used to normalize the
// language subsource reports back into the short code the rest of the app stores.
var subsourceLangCodes = func() map[string]string {
	m := make(map[string]string, len(subsourceLangNames))
	for code, name := range subsourceLangNames {
		m[name] = code
	}
	return m
}()

func toSubsourceLang(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "" {
		return ""
	}
	// The UI may send a comma/space separated list; subsource filters on one.
	if i := strings.IndexAny(code, ", "); i >= 0 {
		code = code[:i]
	}
	if name, ok := subsourceLangNames[code]; ok {
		return name
	}
	return code
}

func fromSubsourceLang(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if code, ok := subsourceLangCodes[n]; ok {
		return code
	}
	return n
}

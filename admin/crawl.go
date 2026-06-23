package main

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"sync"
	"time"
)

// crawl.go runs the two catalog-discovery jobs the admin UI can trigger
// manually (server.go's /api/crawl/* routes): pulling new releases from
// YTS, and backfilling TMDB's top-rated list. Both end up importing through
// importMovie, the shared core that upserts a title and adds any of its YTS
// torrents not already stored.

// CrawlStatus is the in-memory state of one crawl job, polled by the admin
// UI while a run is in progress and after it finishes. Not persisted — a
// restart simply forgets the last run.
type CrawlStatus struct {
	Running    bool
	StartedAt  time.Time
	FinishedAt time.Time
	Summary    string
	Err        string
}

// crawler holds the dependencies and run state for both crawl jobs. Only
// one instance of each job may run at a time.
type crawler struct {
	store           *Store
	tmdb            *TMDBClient
	yts             *YTSClient
	subtitles       SubtitleProvider     // may be nil/disabled; subtitle fetch is then a no-op
	blobs           map[string]BlobStore // keyed by backend name, mirrors Server.blobs
	subtitleStorage string               // backend new subtitles are written to

	mu        sync.Mutex
	newMovies CrawlStatus
	topRated  CrawlStatus
}

func newCrawler(store *Store, tmdb *TMDBClient, yts *YTSClient, subtitles SubtitleProvider, blobs map[string]BlobStore, subtitleStorage string) *crawler {
	return &crawler{store: store, tmdb: tmdb, yts: yts, subtitles: subtitles, blobs: blobs, subtitleStorage: subtitleStorage}
}

// subtitleOptions configures whether and how many subtitles to fetch per
// movie imported by a crawl run, set from the crawl page's form.
type subtitleOptions struct {
	Enabled  bool
	MaxCount int // top N by download count; ignored when Enabled is false
}

func (c *crawler) NewMoviesStatus() CrawlStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.newMovies
}

func (c *crawler) TopRatedStatus() CrawlStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.topRated
}

// YTSBaseURL returns the base URL the YTS client currently queries.
func (c *crawler) YTSBaseURL() string {
	return c.yts.BaseURL()
}

// SetYTSBaseURL changes the base URL the YTS client queries, effective for
// any crawl started after this call. Not persisted — a restart falls back
// to the YTS_BASE_URL env/flag.
func (c *crawler) SetYTSBaseURL(baseURL string) {
	c.yts.SetBaseURL(baseURL)
}

// StartNewMoviesCrawl launches runYTSNewMoviesCrawl in the background.
// Returns false without starting anything if one is already running.
func (c *crawler) StartNewMoviesCrawl(limit int, subOpts subtitleOptions) bool {
	c.mu.Lock()
	if c.newMovies.Running {
		c.mu.Unlock()
		return false
	}
	c.newMovies = CrawlStatus{Running: true, StartedAt: time.Now()}
	c.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		summary, err := c.runYTSNewMoviesCrawl(ctx, limit, subOpts)
		c.finish(&c.newMovies, summary, err)
	}()
	return true
}

// StartTopRatedCrawl launches runTopRatedCrawl in the background. Returns
// false without starting anything if one is already running.
func (c *crawler) StartTopRatedCrawl(startPage, endPage int, subOpts subtitleOptions) bool {
	c.mu.Lock()
	if c.topRated.Running {
		c.mu.Unlock()
		return false
	}
	c.topRated = CrawlStatus{Running: true, StartedAt: time.Now()}
	c.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		summary, err := c.runTopRatedCrawl(ctx, startPage, endPage, subOpts)
		c.finish(&c.topRated, summary, err)
	}()
	return true
}

func (c *crawler) finish(status *CrawlStatus, summary string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status.Running = false
	status.FinishedAt = time.Now()
	status.Summary = summary
	if err != nil {
		status.Err = err.Error()
	} else {
		status.Err = ""
	}
}

// importStats accumulates counts across a crawl run for its summary line.
type importStats struct {
	titlesImported int
	videosAdded    int
	subtitlesAdded int
	skipped        int
	errors         int
}

func (s importStats) String() string {
	return fmt.Sprintf("%d phim, %d video mới, %d phụ đề mới, %d bỏ qua, %d lỗi",
		s.titlesImported, s.videosAdded, s.subtitlesAdded, s.skipped, s.errors)
}

// runYTSNewMoviesCrawl fetches the most recently added movies from YTS and
// imports any English-language ones with a torrent not already in the
// catalog (YTS carries other languages too, but this admin only targets
// English releases).
func (c *crawler) runYTSNewMoviesCrawl(ctx context.Context, limit int, subOpts subtitleOptions) (string, error) {
	movies, err := c.yts.ListMovies(ctx, YTSListParams{SortBy: "date_added", OrderBy: "desc", Limit: limit})
	if err != nil {
		return "", fmt.Errorf("list yts movies: %w", err)
	}

	var stats importStats
	for _, movie := range movies {
		if movie.ImdbCode == "" || len(movie.Torrents) == 0 {
			stats.skipped++
			continue
		}
		if movie.Language != "en" {
			stats.skipped++
			continue
		}

		// Cheap check before spending a TMDB lookup: skip movies whose
		// torrents we've already imported on a previous run.
		hasNew, err := c.hasNewTorrent(ctx, movie.Torrents)
		if err != nil {
			stats.errors++
			continue
		}
		if !hasNew {
			stats.skipped++
			continue
		}

		tmdbID, err := c.tmdb.FindMovieByIMDbID(ctx, movie.ImdbCode)
		if err != nil {
			stats.errors++
			continue
		}

		videos, subs, err := c.importMovie(ctx, tmdbID, movie, subOpts)
		if err != nil {
			stats.errors++
			continue
		}
		stats.titlesImported++
		stats.videosAdded += videos
		stats.subtitlesAdded += subs
	}
	return stats.String(), nil
}

// runTopRatedCrawl backfills TMDB's top-rated movie list, importing only
// movies that also have a YTS torrent (a title with no playable source
// isn't created, mirroring the old project's rule). Movies already in the
// catalog are re-imported when YTS exposes a torrent we don't have yet, so
// importMovie can add the new sources and refresh subtitles.
func (c *crawler) runTopRatedCrawl(ctx context.Context, startPage, endPage int, subOpts subtitleOptions) (string, error) {
	var stats importStats
	for page := startPage; page <= endPage; page++ {
		ids, err := c.tmdb.ListTopRatedMovieIDs(ctx, page)
		if err != nil {
			return stats.String(), fmt.Errorf("list top rated page %d: %w", page, err)
		}

		for _, tmdbID := range ids {
			exists, err := c.store.TitleExistsByTMDBID(ctx, tmdbID, "movie")
			if err != nil {
				stats.errors++
				continue
			}

			imdbID, err := c.tmdb.movieImdbID(ctx, tmdbID)
			if err != nil || imdbID == "" {
				stats.skipped++
				continue
			}

			ytsMovies, err := c.yts.ListMovies(ctx, YTSListParams{QueryTerm: imdbID, Limit: 1})
			if err != nil || len(ytsMovies) == 0 {
				stats.skipped++
				continue
			}

			// A movie already in the catalog is only re-imported when YTS has
			// a torrent we don't have yet, so importMovie can add the new
			// sources (and top up subtitles); otherwise there is nothing to do.
			// Brand-new movies are always imported.
			if exists {
				hasNew, err := c.hasNewTorrent(ctx, ytsMovies[0].Torrents)
				if err != nil {
					stats.errors++
					continue
				}
				if !hasNew {
					stats.skipped++
					continue
				}
			}

			videos, subs, err := c.importMovie(ctx, tmdbID, ytsMovies[0], subOpts)
			if err != nil {
				stats.errors++
				continue
			}
			stats.titlesImported++
			stats.videosAdded += videos
			stats.subtitlesAdded += subs
		}
	}
	return stats.String(), nil
}

// ImportTorrentsForTitle fetches a single existing movie's torrents from YTS
// (matched by its IMDb id) and imports any not already stored, attaching them
// to the title. Unlike the crawl jobs it touches only torrent sources — the
// title's metadata is left untouched. It is the one-click counterpart used by
// the detail page. Only movies are supported — YTS carries no TV. Runs
// synchronously since it touches a single title; the caller blocks on it.
func (c *crawler) ImportTorrentsForTitle(ctx context.Context, title *Title) (int, error) {
	if title.Type != "movie" {
		return 0, fmt.Errorf("YTS chỉ hỗ trợ phim lẻ")
	}
	imdbID, err := c.tmdb.movieImdbID(ctx, title.TMDBID)
	if err != nil || imdbID == "" {
		return 0, fmt.Errorf("không tìm thấy IMDb id trên TMDB cho phim này")
	}
	movies, err := c.yts.ListMovies(ctx, YTSListParams{QueryTerm: imdbID, Limit: 1})
	if err != nil {
		return 0, fmt.Errorf("truy vấn YTS thất bại: %w", err)
	}
	if len(movies) == 0 || len(movies[0].Torrents) == 0 {
		return 0, fmt.Errorf("YTS không có torrent cho phim này")
	}
	videos, _, err := c.addMovieTorrents(ctx, title, movies[0])
	return videos, err
}

// hasNewTorrent reports whether any torrent in the list is not yet stored.
func (c *crawler) hasNewTorrent(ctx context.Context, torrents []YTSTorrent) (bool, error) {
	for _, t := range torrents {
		exists, err := c.store.TorrentSourceExists(ctx, t.Hash)
		if err != nil {
			return false, err
		}
		if !exists {
			return true, nil
		}
	}
	return false, nil
}

// importMovie upserts the TMDB title for tmdbID and imports every torrent in
// ytsMovie that isn't already stored, as a video on that title. It is the
// shared core used by both crawl jobs. When subOpts.Enabled and at least one
// video was added, it also fetches subtitles for the title (see
// downloadSubtitlesForMovie).
func (c *crawler) importMovie(ctx context.Context, tmdbID int, ytsMovie YTSMovie, subOpts subtitleOptions) (videosAdded, subtitlesAdded int, err error) {
	title, err := c.tmdb.FetchTitle(ctx, "movie", tmdbID)
	if err != nil {
		return 0, 0, fmt.Errorf("fetch tmdb title %d: %w", tmdbID, err)
	}
	if err := c.store.UpsertTitle(ctx, title); err != nil {
		return 0, 0, fmt.Errorf("upsert title: %w", err)
	}

	videosAdded, lastFilePath, err := c.addMovieTorrents(ctx, title, ytsMovie)
	if err != nil {
		return videosAdded, 0, err
	}

	if videosAdded > 0 {
		subtitlesAdded = c.downloadSubtitlesForMovie(ctx, title, lastFilePath, subOpts)
	}

	return videosAdded, subtitlesAdded, nil
}

// addMovieTorrents imports every torrent in ytsMovie not already stored as a
// video on the given (already-persisted) title, touching only the torrent
// sources — never the title's metadata. Returns the number of videos added and
// the file path of the last one (used to seed a subtitle query). Shared by
// importMovie and ImportTorrentsForTitle.
func (c *crawler) addMovieTorrents(ctx context.Context, title *Title, ytsMovie YTSMovie) (videosAdded int, lastFilePath string, err error) {
	for _, t := range ytsMovie.Torrents {
		exists, err := c.store.TorrentSourceExists(ctx, t.Hash)
		if err != nil {
			return videosAdded, lastFilePath, fmt.Errorf("check torrent source: %w", err)
		}
		if exists {
			continue
		}

		data, err := fetchTorrentFile(ctx, t.URL)
		if err != nil {
			continue
		}
		infoHash, files, err := parseTorrentFile(data)
		if err != nil {
			continue
		}
		videoFile, ok := pickVideoFile(files)
		if !ok {
			continue
		}

		titleID := title.ID
		v := Video{
			TitleID:    &titleID,
			Name:       ytsMovie.Title,
			Resolution: t.Resolution,
			InfoHash:   infoHash,
			Magnet:     fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=%s", infoHash, url.QueryEscape(ytsMovie.Title)),
			FileIndex:  videoFile.Index,
			FilePath:   videoFile.Path,
			FileSize:   videoFile.Length,
		}
		if err := c.store.AddVideo(ctx, &v, data); err != nil {
			continue
		}
		videosAdded++
		lastFilePath = videoFile.Path
	}
	return videosAdded, lastFilePath, nil
}

// downloadSubtitlesForMovie tops up a title's subtitles to subOpts.MaxCount
// from OpenSubtitles (the highest download-count results first), skipping any
// provider file already saved so a re-crawl adds only new subtitles instead of
// duplicating ones from a previous run. It searches by the title's original
// name first, falling back to a query parsed from the imported video's file
// name if that finds nothing. Failures are non-fatal — a movie still counts as
// imported without its subtitles, so any error here is swallowed (returns 0).
func (c *crawler) downloadSubtitlesForMovie(ctx context.Context, title *Title, fileName string, subOpts subtitleOptions) int {
	if !subOpts.Enabled || c.subtitles == nil || !c.subtitles.Enabled() {
		return 0
	}
	blobStore := c.blobs[c.subtitleStorage]
	if blobStore == nil {
		return 0
	}

	existing, err := c.store.SubtitlesForTitle(ctx, title.ID)
	if err != nil {
		return 0
	}
	// Provider files already saved for this title — skip them so a re-crawl
	// adds only subtitles we don't have yet.
	have := make(map[string]bool, len(existing))
	for _, s := range existing {
		if s.Provider == c.subtitles.Name() {
			have[s.ProviderFileID] = true
		}
	}
	max := subOpts.MaxCount
	if max <= 0 {
		max = 1
	}
	remaining := max - len(have)
	if remaining <= 0 {
		return 0
	}

	query := title.OriginalTitle
	if query == "" {
		query = title.Title
	}
	results, err := c.subtitles.Search(ctx, SearchParams{Query: query, Languages: "vi"})
	if (err != nil || len(results) == 0) && fileName != "" {
		if altQuery, _, _ := parseSubtitleQuery(fileName); altQuery != "" {
			results, err = c.subtitles.Search(ctx, SearchParams{Query: altQuery, Languages: "vi"})
		}
	}
	if err != nil || len(results) == 0 {
		return 0
	}

	sort.Slice(results, func(i, j int) bool { return results[i].DownloadCount > results[j].DownloadCount })

	titleID := title.ID
	added := 0
	for _, r := range results {
		if added >= remaining {
			break
		}
		fileID := r.FileID
		if have[fileID] {
			continue
		}
		data, format, err := c.subtitles.Download(ctx, fileID)
		if err != nil {
			continue
		}
		lang := r.Language
		if lang == "" {
			lang = "sub"
		}
		key := subtitleStorageKey(&titleID, nil, c.subtitles.Name(), fileID, lang, format)
		if err := blobStore.Put(ctx, key, data, subtitleContentType(format)); err != nil {
			continue
		}
		sub := &Subtitle{
			TitleID: &titleID, Provider: c.subtitles.Name(), ProviderFileID: fileID,
			Language: lang, Name: r.Release, DownloadCount: r.DownloadCount, Format: format,
			StorageBackend: blobStore.Name(), StorageKey: key,
		}
		if err := c.store.AddSubtitle(ctx, sub); err != nil {
			_ = blobStore.Delete(ctx, key)
			continue
		}
		added++
	}
	return added
}

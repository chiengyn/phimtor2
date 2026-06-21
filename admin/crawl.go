package main

import (
	"context"
	"fmt"
	"net/url"
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
	store *Store
	tmdb  *TMDBClient
	yts   *YTSClient

	mu        sync.Mutex
	newMovies CrawlStatus
	topRated  CrawlStatus
}

func newCrawler(store *Store, tmdb *TMDBClient, yts *YTSClient) *crawler {
	return &crawler{store: store, tmdb: tmdb, yts: yts}
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
func (c *crawler) StartNewMoviesCrawl(limit int) bool {
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
		summary, err := c.runYTSNewMoviesCrawl(ctx, limit)
		c.finish(&c.newMovies, summary, err)
	}()
	return true
}

// StartTopRatedCrawl launches runTopRatedCrawl in the background. Returns
// false without starting anything if one is already running.
func (c *crawler) StartTopRatedCrawl(startPage, endPage int) bool {
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
		summary, err := c.runTopRatedCrawl(ctx, startPage, endPage)
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
	skipped        int
	errors         int
}

func (s importStats) String() string {
	return fmt.Sprintf("%d phim, %d video mới, %d bỏ qua, %d lỗi",
		s.titlesImported, s.videosAdded, s.skipped, s.errors)
}

// runYTSNewMoviesCrawl fetches the most recently added movies from YTS and
// imports any English-language ones with a torrent not already in the
// catalog (YTS carries other languages too, but this admin only targets
// English releases).
func (c *crawler) runYTSNewMoviesCrawl(ctx context.Context, limit int) (string, error) {
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

		videos, err := c.importMovie(ctx, tmdbID, movie)
		if err != nil {
			stats.errors++
			continue
		}
		stats.titlesImported++
		stats.videosAdded += videos
	}
	return stats.String(), nil
}

// runTopRatedCrawl backfills TMDB's top-rated movie list, importing only
// movies that also have a YTS torrent (a title with no playable source
// isn't created, mirroring the old project's rule).
func (c *crawler) runTopRatedCrawl(ctx context.Context, startPage, endPage int) (string, error) {
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
			if exists {
				stats.skipped++
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

			videos, err := c.importMovie(ctx, tmdbID, ytsMovies[0])
			if err != nil {
				stats.errors++
				continue
			}
			stats.titlesImported++
			stats.videosAdded += videos
		}
	}
	return stats.String(), nil
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
// shared core used by both crawl jobs.
func (c *crawler) importMovie(ctx context.Context, tmdbID int, ytsMovie YTSMovie) (videosAdded int, err error) {
	title, err := c.tmdb.FetchTitle(ctx, "movie", tmdbID)
	if err != nil {
		return 0, fmt.Errorf("fetch tmdb title %d: %w", tmdbID, err)
	}
	if err := c.store.UpsertTitle(ctx, title); err != nil {
		return 0, fmt.Errorf("upsert title: %w", err)
	}

	for _, t := range ytsMovie.Torrents {
		exists, err := c.store.TorrentSourceExists(ctx, t.Hash)
		if err != nil {
			return videosAdded, fmt.Errorf("check torrent source: %w", err)
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
	}

	return videosAdded, nil
}

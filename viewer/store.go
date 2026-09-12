package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	// Imported by name, not blank: the billing amount-reservation loop needs to
	// tell a duplicate-key rejection (expected, retry) from a real failure. A
	// named import still runs the driver's init, so registration is unchanged.
	mysqldrv "github.com/go-sql-driver/mysql"
)

// isDuplicateKey reports whether err is MySQL's "duplicate entry" (1062). For
// the invoice amount reservation this is an ordinary outcome under concurrency,
// not an error condition.
func isDuplicateKey(err error) bool {
	var me *mysqldrv.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

type Store struct {
	db *sql.DB
}

func NewStore(dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetConnMaxLifetime(3 * time.Minute)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	// MySQL may not be ready yet when running under compose; retry briefly.
	var pingErr error
	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		pingErr = db.PingContext(ctx)
		cancel()
		if pingErr == nil {
			return &Store{db: db}, nil
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("ping mysql: %w", pingErr)
}

func (s *Store) Close() error { return s.db.Close() }

// TitleSummary is the lightweight row rendered in the discovery grid.
type TitleSummary struct {
	ID            int64
	TMDBID        int
	Type          string
	Title         string
	OriginalTitle string
	AirDate       string
	PosterPath    string
	VoteAverage   float64
	// HasVietsub marks a movie with a saved Vietnamese subtitle (the card's
	// "Vietsub" chip). Movie-only by design: the SELECT gates on type so TV
	// cards never carry it.
	HasVietsub bool
}

// TitleFilter narrows the discovery list. Zero values mean "no constraint".
type TitleFilter struct {
	Query   string // free-text match against the (localized) title or original title
	GenreID int    // TMDB genre id; 0 = any
	Type    string // "movie" | "tv"; anything else = any
	Vietsub bool   // when set, only movies carrying a Vietnamese subtitle
}

// titleFilterClause builds the shared WHERE clause (and its args) for the
// discovery filter, used by both ListTitles and CountTitles so the page of rows
// and the total count always agree. It returns "" when the filter is empty.
func titleFilterClause(f TitleFilter) (string, []any) {
	var where []string
	var args []any

	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, "(title LIKE ? OR original_title LIKE ?)")
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	if f.GenreID > 0 {
		where = append(where, "id IN (SELECT title_id FROM title_genres WHERE genre_id = ?)")
		args = append(args, f.GenreID)
	}
	if f.Type == "movie" || f.Type == "tv" {
		where = append(where, "type = ?")
		args = append(args, f.Type)
	}
	if f.Vietsub {
		// Vietsub is a movie-only flag (has_vietsub is set only on movies), so
		// gating on it also implies the movie type.
		where = append(where, "type = 'movie' AND has_vietsub")
	}
	if len(where) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// titleSummaryColumns is the SELECT list every card query shares, so the
// discovery grid, the browse rows and the saved list can never drift apart in
// which columns they load. scanTitleSummary is its matching scanner, taking the
// Scan method both *sql.Row and *sql.Rows satisfy (same idiom as scanVideo).
//
// The columns are qualified with the alias "t", so every caller must select
// `FROM titles t` — otherwise a query that joins another table carrying an `id`
// column (SavedTitles does) would be ambiguous.
const titleSummaryColumns = `t.id, t.tmdb_id, t.type, t.title, t.original_title, t.air_date, t.poster_path, t.vote_average, (t.type = 'movie' AND t.has_vietsub)`

func scanTitleSummary(sc interface{ Scan(...any) error }) (TitleSummary, error) {
	var t TitleSummary
	var origTitle, poster sql.NullString
	var air sql.NullTime
	var vote sql.NullFloat64
	if err := sc.Scan(&t.ID, &t.TMDBID, &t.Type, &t.Title, &origTitle, &air, &poster, &vote, &t.HasVietsub); err != nil {
		return TitleSummary{}, err
	}
	t.OriginalTitle = origTitle.String
	t.AirDate = dateStr(air)
	t.PosterPath = poster.String
	t.VoteAverage = vote.Float64
	return t, nil
}

// CountTitles returns how many titles match the filter, for computing the total
// number of discovery-grid pages.
func (s *Store) CountTitles(ctx context.Context, f TitleFilter) (int, error) {
	clause, args := titleFilterClause(f)
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM titles`+clause, args...).Scan(&n)
	return n, err
}

// ListTitles returns one page of title summaries matching the filter, newest
// first; offset skips earlier pages.
func (s *Store) ListTitles(ctx context.Context, f TitleFilter, limit, offset int) ([]TitleSummary, error) {
	clause, args := titleFilterClause(f)
	query := `SELECT ` + titleSummaryColumns + ` FROM titles t` +
		clause + " ORDER BY updated_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TitleSummary
	for rows.Next() {
		t, err := scanTitleSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SitemapEntry is one indexable title for the XML sitemap. Title/OriginalTitle
// feed the SEO slug in the emitted <loc>.
type SitemapEntry struct {
	ID            int64
	Title         string
	OriginalTitle string
	UpdatedAt     time.Time
}

// SitemapTitles lists every title with its last-modified time, newest first,
// for sitemap.xml. Missing timestamps fall back to "now" so the entry is still
// emitted with a valid <lastmod>.
func (s *Store) SitemapTitles(ctx context.Context) ([]SitemapEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, original_title, updated_at FROM titles ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SitemapEntry
	for rows.Next() {
		var e SitemapEntry
		var origTitle sql.NullString
		var updated sql.NullTime
		if err := rows.Scan(&e.ID, &e.Title, &origTitle, &updated); err != nil {
			return nil, err
		}
		e.OriginalTitle = origTitle.String
		if updated.Valid {
			e.UpdatedAt = updated.Time
		} else {
			e.UpdatedAt = time.Now()
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// rowLimit caps how many titles each browse row carries. Rows are horizontal
// carousels, so there is no point loading the whole catalog into one; the row
// heading links to the filtered grid (which paginates) for the full list.
const rowLimit = 10

// Row is a labelled horizontal strip of titles on the browse home page.
type Row struct {
	Key    string // query string that re-filters to this row, e.g. "type=movie"
	Label  string
	Titles []TitleSummary
	// Ranked marks the "Top 10" strip: a true rating-ordered ranking the browse
	// page renders with oversized rank numerals (and no "see all" link, since the
	// ranking — not a filter — is what the row is about).
	Ranked bool
}

// Href is the browse link for this row. It returns a template.URL so
// html/template does not query-escape the "=" in Key (which would turn
// "/?type=movie" into "/?type%3dmovie" and break the filter).
func (r Row) Href() template.URL {
	return template.URL("/?" + r.Key)
}

// ListRows groups every title into Netflix-style browse rows: one row per type
// (movies, then TV) followed by one row per genre, newest titles first within
// each row. Empty rows are omitted.
func (s *Store) ListRows(ctx context.Context) ([]Row, error) {
	// Load every title once, newest first.
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+titleSummaryColumns+` FROM titles t ORDER BY t.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var order []int64 // title ids in newest-first order
	byID := map[int64]TitleSummary{}
	var all, movies, tv, vietsub []TitleSummary
	for rows.Next() {
		t, err := scanTitleSummary(rows)
		if err != nil {
			return nil, err
		}
		byID[t.ID] = t
		order = append(order, t.ID)
		all = append(all, t)
		switch t.Type {
		case "movie":
			movies = append(movies, t)
		case "tv":
			tv = append(tv, t)
		}
		if t.HasVietsub {
			vietsub = append(vietsub, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load the title→genre edges. ORDER BY g.name makes first-seen genre order
	// alphabetical; titles are bucketed by walking `order` so each genre row
	// stays newest-first.
	grows, err := s.db.QueryContext(ctx, `
		SELECT tg.title_id, g.id, g.name FROM title_genres tg
		JOIN genres g ON g.id = tg.genre_id
		ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	defer grows.Close()

	genreName := map[int]string{}
	titleGenres := map[int64][]int{}
	var genreOrder []int
	for grows.Next() {
		var titleID int64
		var gid int
		var gname string
		if err := grows.Scan(&titleID, &gid, &gname); err != nil {
			return nil, err
		}
		if _, seen := genreName[gid]; !seen {
			genreName[gid] = gname
			genreOrder = append(genreOrder, gid)
		}
		titleGenres[titleID] = append(titleGenres[titleID], gid)
	}
	if err := grows.Err(); err != nil {
		return nil, err
	}

	genreTitles := map[int][]TitleSummary{}
	for _, id := range order {
		for _, gid := range titleGenres[id] {
			genreTitles[gid] = append(genreTitles[gid], byID[id])
		}
	}

	var out []Row
	// Top 10: prefer the admin's hand-picked featured titles, in their curated
	// order (the same list that drives the hero billboard). When nothing is
	// featured, fall back to a genuine rating ranking so the numbered strip still
	// means something — only titles that carry a vote qualify, and the row is
	// dropped unless enough of them do (a "Top 10" of unrated titles would be
	// numbering noise).
	featIDs, err := s.FeaturedTitleIDs(ctx, 10)
	if err != nil {
		return nil, err
	}
	if len(featIDs) > 0 {
		var top []TitleSummary
		for _, id := range featIDs {
			if t, ok := byID[id]; ok {
				top = append(top, t)
			}
		}
		out = append(out, Row{Label: "Top 10 nổi bật hôm nay", Titles: top, Ranked: true})
	} else {
		top := make([]TitleSummary, 0, len(all))
		for _, t := range all {
			if t.VoteAverage > 0 {
				top = append(top, t)
			}
		}
		sort.SliceStable(top, func(i, j int) bool { return top[i].VoteAverage > top[j].VoteAverage })
		if len(top) >= 3 {
			if len(top) > 10 {
				top = top[:10]
			}
			out = append(out, Row{Label: "Top 10 nổi bật hôm nay", Titles: top, Ranked: true})
		}
	}
	// Latest movies that carry a Vietnamese subtitle (has_vietsub), placed right
	// below the Top 10 featured row; its heading links to the vietsub=1 grid for
	// the full list.
	if len(vietsub) > 0 {
		out = append(out, Row{Key: "vietsub=1", Label: "Phim lẻ Vietsub mới cập nhật", Titles: capRow(vietsub)})
	}
	if len(movies) > 0 {
		out = append(out, Row{Key: "type=movie", Label: "Phim lẻ", Titles: capRow(movies)})
	}
	if len(tv) > 0 {
		out = append(out, Row{Key: "type=tv", Label: "Phim bộ", Titles: capRow(tv)})
	}
	for _, gid := range genreOrder {
		if ts := genreTitles[gid]; len(ts) > 0 {
			out = append(out, Row{Key: fmt.Sprintf("genre=%d", gid), Label: genreName[gid], Titles: capRow(ts)})
		}
	}
	return out, nil
}

// capRow trims a row's titles to rowLimit (carousels never show more).
func capRow(ts []TitleSummary) []TitleSummary {
	if len(ts) > rowLimit {
		return ts[:rowLimit]
	}
	return ts
}

// FeaturedTitleIDs returns the ids of titles an admin hand-picked for the browse
// hero billboard, in curated display order (featured_titles.position ascending),
// capped at limit. Empty when nothing is featured, letting the caller fall back
// to a score-based pick. Joined to titles so a since-deleted pick drops out.
func (s *Store) FeaturedTitleIDs(ctx context.Context, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.title_id FROM featured_titles f
		JOIN titles t ON t.id = f.title_id
		ORDER BY f.position ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListGenres returns only the genres actually attached to at least one title,
// for the discovery filter dropdown.
func (s *Store) ListGenres(ctx context.Context) ([]Genre, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT g.id, g.name FROM genres g
		JOIN title_genres tg ON tg.genre_id = g.id
		ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Genre
	for rows.Next() {
		var g Genre
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetTitle loads a full title including genres and (for TV) seasons + episodes.
// Returns (nil, nil) when no such title exists.
func (s *Store) GetTitle(ctx context.Context, id int64) (*Title, error) {
	var t Title
	var overview, poster, backdrop, lang, status sql.NullString
	var air sql.NullTime
	var runtime sql.NullInt64
	var vote sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, tmdb_id, type, title, original_title, overview, air_date, runtime,
		       poster_path, backdrop_path, vote_average, original_language, status,
		       created_at, updated_at
		FROM titles WHERE id = ?`, id).Scan(
		&t.ID, &t.TMDBID, &t.Type, &t.Title, &t.OriginalTitle, &overview, &air, &runtime,
		&poster, &backdrop, &vote, &lang, &status, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Overview = overview.String
	t.AirDate = dateStr(air)
	if runtime.Valid {
		r := int(runtime.Int64)
		t.Runtime = &r
	}
	t.PosterPath = poster.String
	t.BackdropPath = backdrop.String
	t.VoteAverage = vote.Float64
	t.OriginalLanguage = lang.String
	t.Status = status.String

	if t.Genres, err = s.loadGenres(ctx, id); err != nil {
		return nil, err
	}
	if t.Type == "tv" {
		if t.Seasons, err = s.loadSeasons(ctx, id); err != nil {
			return nil, err
		}
	}
	return &t, nil
}

func (s *Store) loadGenres(ctx context.Context, titleID int64) ([]Genre, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.id, g.name FROM genres g
		JOIN title_genres tg ON tg.genre_id = g.id
		WHERE tg.title_id = ? ORDER BY g.name`, titleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Genre
	for rows.Next() {
		var g Genre
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) loadSeasons(ctx context.Context, titleID int64) ([]Season, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, season_number, name, overview, air_date, poster_path
		FROM seasons WHERE title_id = ? ORDER BY season_number`, titleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var seasons []Season
	byID := map[int64]*Season{}
	for rows.Next() {
		var se Season
		var overview, poster sql.NullString
		var air sql.NullTime
		if err := rows.Scan(&se.ID, &se.SeasonNumber, &se.Name, &overview, &air, &poster); err != nil {
			return nil, err
		}
		se.Overview = overview.String
		se.AirDate = dateStr(air)
		se.PosterPath = poster.String
		seasons = append(seasons, se)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range seasons {
		byID[seasons[i].ID] = &seasons[i]
	}
	if len(seasons) == 0 {
		return seasons, nil
	}

	erows, err := s.db.QueryContext(ctx, `
		SELECT e.season_id, e.id, e.episode_number, e.name, e.overview, e.air_date, e.runtime, e.still_path
		FROM episodes e JOIN seasons s ON s.id = e.season_id
		WHERE s.title_id = ? ORDER BY e.episode_number`, titleID)
	if err != nil {
		return nil, err
	}
	defer erows.Close()
	for erows.Next() {
		var seasonID int64
		var ep Episode
		var overview, still sql.NullString
		var air sql.NullTime
		var runtime sql.NullInt64
		if err := erows.Scan(&seasonID, &ep.ID, &ep.EpisodeNumber, &ep.Name, &overview, &air, &runtime, &still); err != nil {
			return nil, err
		}
		ep.Overview = overview.String
		ep.AirDate = dateStr(air)
		if runtime.Valid {
			r := int(runtime.Int64)
			ep.Runtime = &r
		}
		ep.StillPath = still.String
		if se := byID[seasonID]; se != nil {
			se.Episodes = append(se.Episodes, ep)
		}
	}
	return seasons, erows.Err()
}

// EpisodeContext identifies an episode and its parent title, for the watch page.
type EpisodeContext struct {
	TitleID       int64
	TitleName     string
	SeasonNumber  int
	EpisodeNumber int
	EpisodeName   string
}

// GetEpisodeContext resolves a single episode id to its parent title and
// season/episode numbers. Returns (nil, nil) when no such episode exists.
func (s *Store) GetEpisodeContext(ctx context.Context, episodeID int64) (*EpisodeContext, error) {
	var ec EpisodeContext
	var name sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT t.id, t.title, s.season_number, e.episode_number, e.name
		FROM episodes e
		JOIN seasons s ON s.id = e.season_id
		JOIN titles t ON t.id = s.title_id
		WHERE e.id = ?`, episodeID).Scan(
		&ec.TitleID, &ec.TitleName, &ec.SeasonNumber, &ec.EpisodeNumber, &name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ec.EpisodeName = name.String
	return &ec, nil
}

// EpisodesInSeasonOf returns all episodes sharing the given episode's season,
// ordered by episode number, for same-season navigation on the watch page.
func (s *Store) EpisodesInSeasonOf(ctx context.Context, episodeID int64) ([]Episode, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.episode_number, e.name, e.overview, e.air_date, e.runtime, e.still_path
		FROM episodes e
		WHERE e.season_id = (SELECT season_id FROM episodes WHERE id = ?)
		ORDER BY e.episode_number`, episodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		var ep Episode
		var overview, still sql.NullString
		var air sql.NullTime
		var runtime sql.NullInt64
		if err := rows.Scan(&ep.ID, &ep.EpisodeNumber, &ep.Name, &overview, &air, &runtime, &still); err != nil {
			return nil, err
		}
		ep.Overview = overview.String
		ep.AirDate = dateStr(air)
		if runtime.Valid {
			r := int(runtime.Int64)
			ep.Runtime = &r
		}
		ep.StillPath = still.String
		out = append(out, ep)
	}
	return out, rows.Err()
}

func dateStr(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format("2006-01-02")
}

// --- Videos (read-only) ----------------------------------------------------

// videoColumns is the shared SELECT list / scan order for video rows, joining
// the torrent source for info_hash + magnet (never the raw .torrent bytes).
const videoColumns = `v.id, v.source_id, v.title_id, v.episode_id, v.name, v.resolution,
	src.info_hash, src.magnet, v.file_index, v.file_path, v.file_size, v.created_at`

func scanVideo(sc interface{ Scan(...any) error }) (Video, error) {
	var v Video
	var titleID, episodeID sql.NullInt64
	if err := sc.Scan(&v.ID, &v.SourceID, &titleID, &episodeID, &v.Name, &v.Resolution,
		&v.InfoHash, &v.Magnet, &v.FileIndex, &v.FilePath, &v.FileSize, &v.CreatedAt); err != nil {
		return Video{}, err
	}
	if titleID.Valid {
		v.TitleID = &titleID.Int64
	}
	if episodeID.Valid {
		v.EpisodeID = &episodeID.Int64
	}
	return v, nil
}

func (s *Store) queryVideos(ctx context.Context, where string, arg int64) ([]Video, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+videoColumns+`
		FROM videos v
		JOIN torrent_sources src ON src.id = v.source_id
		WHERE `+where+` ORDER BY v.created_at DESC, v.id DESC`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VideosForTitle returns the movie videos attached to a title, newest first
// (so the first entry is the default/latest source).
func (s *Store) VideosForTitle(ctx context.Context, titleID int64) ([]Video, error) {
	return s.queryVideos(ctx, "v.title_id = ?", titleID)
}

// VideosForEpisode returns the videos attached to a single TV episode, newest
// first.
func (s *Store) VideosForEpisode(ctx context.Context, episodeID int64) ([]Video, error) {
	return s.queryVideos(ctx, "v.episode_id = ?", episodeID)
}

// GetVideo loads one video by id (with its source's info_hash, magnet, and — when
// stored — the raw .torrent bytes), used by the prepare endpoint to add the
// torrent to the streamer. Unlike the list queries it also selects the
// torrent_file blob so playback can hand the streamer the metainfo directly and
// skip the DHT metadata fetch. Returns (nil, nil) when no such video exists.
func (s *Store) GetVideo(ctx context.Context, id int64) (*Video, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+videoColumns+`, src.torrent_file
		FROM videos v
		JOIN torrent_sources src ON src.id = v.source_id
		WHERE v.id = ?`, id)
	var v Video
	var titleID, episodeID sql.NullInt64
	if err := row.Scan(&v.ID, &v.SourceID, &titleID, &episodeID, &v.Name, &v.Resolution,
		&v.InfoHash, &v.Magnet, &v.FileIndex, &v.FilePath, &v.FileSize, &v.CreatedAt,
		&v.TorrentFile); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if titleID.Valid {
		v.TitleID = &titleID.Int64
	}
	if episodeID.Valid {
		v.EpisodeID = &episodeID.Int64
	}
	return &v, nil
}

// TitleIDForVideo resolves a video id to the title it ultimately belongs to,
// following episode -> season -> title for a TV source. The prepare endpoint
// needs this because entitlements are per TITLE while it is handed only a video
// id, and videos.title_id is set for movies but NULL for episodes. Returns
// (0, nil) when no such video exists.
func (s *Store) TitleIDForVideo(ctx context.Context, videoID int64) (int64, error) {
	var titleID sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(v.title_id, se.title_id)
		FROM videos v
		LEFT JOIN episodes e  ON e.id = v.episode_id
		LEFT JOIN seasons  se ON se.id = e.season_id
		WHERE v.id = ?`, videoID).Scan(&titleID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return titleID.Int64, nil
}

// --- Subtitles (read-only) -------------------------------------------------

// subtitleColumns is the shared SELECT list / scan order for subtitle rows.
const subtitleColumns = `id, title_id, episode_id, provider, provider_file_id, language,
	name, download_count, format, storage_backend, storage_key, metadata, created_at`

func scanSubtitle(sc interface{ Scan(...any) error }) (Subtitle, error) {
	var sub Subtitle
	var titleID, episodeID sql.NullInt64
	var meta []byte
	if err := sc.Scan(&sub.ID, &titleID, &episodeID, &sub.Provider, &sub.ProviderFileID,
		&sub.Language, &sub.Name, &sub.DownloadCount, &sub.Format, &sub.StorageBackend,
		&sub.StorageKey, &meta, &sub.CreatedAt); err != nil {
		return Subtitle{}, err
	}
	if titleID.Valid {
		sub.TitleID = &titleID.Int64
	}
	if episodeID.Valid {
		sub.EpisodeID = &episodeID.Int64
	}
	if len(meta) > 0 {
		sub.Metadata = append(json.RawMessage{}, meta...)
	}
	return sub, nil
}

// GetSubtitle loads one subtitle by id, returning (nil, nil) when none matches.
func (s *Store) GetSubtitle(ctx context.Context, id int64) (*Subtitle, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+subtitleColumns+` FROM subtitles WHERE id = ?`, id)
	sub, err := scanSubtitle(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

func (s *Store) querySubtitles(ctx context.Context, where string, arg int64) ([]Subtitle, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+subtitleColumns+` FROM subtitles WHERE `+where+` ORDER BY language, created_at`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subtitle
	for rows.Next() {
		sub, err := scanSubtitle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// SubtitlesForTitle returns the subtitles attached to a movie title.
func (s *Store) SubtitlesForTitle(ctx context.Context, titleID int64) ([]Subtitle, error) {
	return s.querySubtitles(ctx, "title_id = ?", titleID)
}

// SubtitlesForEpisode returns the subtitles attached to a single TV episode.
func (s *Store) SubtitlesForEpisode(ctx context.Context, episodeID int64) ([]Subtitle, error) {
	return s.querySubtitles(ctx, "episode_id = ?", episodeID)
}

// --- Accounts and saved titles -----------------------------------------------
//
// These two tables are the ONLY things the viewer writes. The catalog itself
// (titles, videos, subtitles, …) stays strictly read-only, and the schema is
// still owned entirely by the admin service — the viewer never migrates.

// maxBookmarks caps a single merge request so a hostile or broken client cannot
// push an unbounded insert.
const maxBookmarks = 500

// UpsertGoogleUser creates or refreshes the row for a Google identity and
// returns it. The (provider, provider_uid) unique key is the identity, so a user
// who changes their Google display name, avatar or even email address keeps the
// same row and the same saved list.
func (s *Store) UpsertGoogleUser(ctx context.Context, id *googleIdentity) (*User, error) {
	// `id = LAST_INSERT_ID(id)` is what makes LastInsertId return the EXISTING
	// row's id on the update branch — without it, a returning user would come
	// back with 0. Works on both MySQL 8 and the MariaDB used in production.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO users (provider, provider_uid, email, email_verified, name, avatar_url, locale, last_login_at)
		VALUES ('google', ?, ?, ?, ?, ?, ?, NOW())
		ON DUPLICATE KEY UPDATE
			email = VALUES(email), email_verified = VALUES(email_verified),
			name = VALUES(name), avatar_url = VALUES(avatar_url),
			locale = VALUES(locale), last_login_at = NOW(), id = LAST_INSERT_ID(id)`,
		id.Sub, id.Email, id.EmailVerified, id.Name, id.Picture, id.Locale)
	if err != nil {
		return nil, err
	}
	userID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.UserByID(ctx, userID)
}

// UserByID loads a user together with the set of titles they have saved, for the
// session middleware. A blocked user, or one whose row is gone, comes back as
// (nil, nil) — a stale cookie, not an error.
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	var u User
	var email, name, avatar sql.NullString
	var planExpires, compExpires sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, email, name, avatar_url, plan, plan_expires_at, comp_expires_at
		FROM users WHERE id = ? AND is_blocked = 0`, id).
		Scan(&u.ID, &email, &name, &avatar, &u.Plan, &planExpires, &compExpires)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.Email = email.String
	u.Name = name.String
	u.AvatarURL = avatar.String
	if planExpires.Valid {
		t := planExpires.Time
		u.PlanExpiresAt = &t
	}
	if compExpires.Valid {
		t := compExpires.Time
		u.CompExpiresAt = &t
	}

	u.SavedIDs, err = s.SavedTitleIDs(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// HasTitleUnlock reports whether this user has bought the permanent 4K unlock
// for one title. Only consulted when a paid-gated source is actually on the
// page, so free 720p traffic never pays for it.
func (s *Store) HasTitleUnlock(ctx context.Context, userID, titleID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM user_title_unlocks WHERE user_id = ? AND title_id = ?`,
		userID, titleID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// --- Billing ---------------------------------------------------------------
//
// Every timestamp comparison below is done with MySQL's own NOW(), never a Go
// time.Time. The app and the database can disagree about the clock or the zone,
// and an invoice must not expire early (or refuse to) because of that skew.

const invoiceColumns = `id, ref, user_id, kind, plan_code, title_id, amount_usd_cents,
	chain, pay_to, pay_amount, token, status, received, paid_chain, tx_hash,
	expires_at, paid_at, created_at`

func scanInvoice(sc interface{ Scan(...any) error }) (*Invoice, error) {
	var inv Invoice
	var titleID sql.NullInt64
	var paidAt sql.NullTime
	if err := sc.Scan(&inv.ID, &inv.Ref, &inv.UserID, &inv.Kind, &inv.PlanCode, &titleID,
		&inv.AmountUSDCents, &inv.Chain, &inv.PayTo, &inv.PayAmount, &inv.Token,
		&inv.Status, &inv.Received, &inv.PaidChain, &inv.TxHash,
		&inv.ExpiresAt, &paidAt, &inv.CreatedAt); err != nil {
		return nil, err
	}
	if titleID.Valid {
		inv.TitleID = &titleID.Int64
	}
	if paidAt.Valid {
		t := paidAt.Time
		inv.PaidAt = &t
	}
	return &inv, nil
}

// CreateInvoice writes a pending invoice, reserving its amount via
// (lock_ns, amount_lock). A duplicate-key error here is the reservation being
// taken — the caller retries with different dust (see billing.go).
func (s *Store) CreateInvoice(ctx context.Context, inv *Invoice, lockNS string, ttlMin int) error {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO payment_invoices
			(ref, user_id, kind, plan_code, title_id, amount_usd_cents, chain, pay_to,
			 pay_amount, token, rate_usd, status, lock_ns, amount_lock, expires_at)
		VALUES (?,?,?,?,?,?,?,?,CAST(? AS DECIMAL(36,18)),?,?, 'pending', ?, CAST(? AS DECIMAL(36,18)),
			DATE_ADD(NOW(), INTERVAL ? MINUTE))`,
		inv.Ref, inv.UserID, inv.Kind, inv.PlanCode, inv.TitleID, inv.AmountUSDCents,
		inv.Chain, inv.PayTo, inv.PayAmount, inv.Token, stableUSDRate,
		lockNS, inv.PayAmount, ttlMin)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	inv.ID = id
	return nil
}

// InvoiceByRef loads one invoice by its public ref. Returns (nil, nil) on miss.
func (s *Store) InvoiceByRef(ctx context.Context, ref string) (*Invoice, error) {
	inv, err := scanInvoice(s.db.QueryRowContext(ctx,
		`SELECT `+invoiceColumns+` FROM payment_invoices WHERE ref = ?`, ref))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// CountPendingInvoices is how many invoices in a namespace are still payable. It
// is what lets the poller skip an expensive log query when nothing is owed.
func (s *Store) CountPendingInvoices(ctx context.Context, lockNS string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM payment_invoices
		 WHERE status = 'pending' AND lock_ns = ? AND expires_at > NOW()`, lockNS).Scan(&n)
	return n, err
}

// MatchPendingInvoice finds the payable invoice that reserved this exact amount.
// The CAST makes the comparison a DECIMAL one rather than a string one, so
// "5.0001" and "5.000100" match — they are the same quantity of money.
// Returns (nil, nil) when nothing owes this amount.
func (s *Store) MatchPendingInvoice(ctx context.Context, lockNS, amount string) (*Invoice, error) {
	inv, err := scanInvoice(s.db.QueryRowContext(ctx,
		`SELECT `+invoiceColumns+` FROM payment_invoices
		 WHERE status = 'pending' AND lock_ns = ?
		   AND amount_lock = CAST(? AS DECIMAL(36,18))
		   AND expires_at > NOW()
		 LIMIT 1`, lockNS, amount))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// ExpireInvoices releases the reservations held by invoices that ran out of
// time. Clearing lock_ns/amount_lock is the point: it returns that amount to the
// pool (the unique index ignores NULLs).
func (s *Store) ExpireInvoices(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE payment_invoices
		SET status = 'expired', lock_ns = NULL, amount_lock = NULL
		WHERE status = 'pending' AND expires_at <= NOW()`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SettleInvoice marks an invoice paid and grants its entitlement in ONE
// transaction, returning false when the invoice was already settled.
//
// The `AND status = 'pending'` guard is the whole idempotency story: a repeated
// scan observation, a restart mid-settle and a rewound cursor all land on zero
// rows affected and change nothing. Everything downstream depends on that, so do
// not "simplify" the guard away.
func (s *Store) SettleInvoice(ctx context.Context, inv *Invoice, ob observedPayment, passDays int) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE payment_invoices
		SET status = 'paid', lock_ns = NULL, amount_lock = NULL, paid_at = NOW(),
		    received = CAST(? AS DECIMAL(36,18)), paid_chain = ?, tx_hash = ?
		WHERE id = ? AND status = 'pending'`,
		ob.Amount, ob.Chain, ob.TxHash, inv.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}

	switch inv.Kind {
	case "pass":
		// GREATEST(...) is what makes renewals stack instead of truncating: an
		// unexpired pass extends from its own end date, a lapsed one from today.
		if _, err := tx.ExecContext(ctx, `
			UPDATE users
			SET plan = 'premium',
			    plan_expires_at = DATE_ADD(GREATEST(COALESCE(plan_expires_at, NOW()), NOW()), INTERVAL ? DAY)
			WHERE id = ?`, passDays, inv.UserID); err != nil {
			return false, err
		}
	case "title":
		if inv.TitleID == nil {
			return false, fmt.Errorf("invoice %s is a title unlock with no title_id", inv.Ref)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT IGNORE INTO user_title_unlocks (user_id, title_id, invoice_id)
			VALUES (?,?,?)`, inv.UserID, *inv.TitleID, inv.ID); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("invoice %s has unknown kind %q", inv.Ref, inv.Kind)
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ChainCursor is where this chain's scan got to ("" when never scanned).
func (s *Store) ChainCursor(ctx context.Context, chain string) (string, error) {
	var c string
	err := s.db.QueryRowContext(ctx,
		`SELECT scan_cursor FROM billing_chain_cursors WHERE chain = ?`, chain).Scan(&c)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return c, nil
}

func (s *Store) SetChainCursor(ctx context.Context, chain, cursor string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO billing_chain_cursors (chain, scan_cursor) VALUES (?,?)
		ON DUPLICATE KEY UPDATE scan_cursor = VALUES(scan_cursor)`, chain, cursor)
	return err
}

// SavedTitleIDs is the set of title ids this user has saved, used to pre-mark
// every save button on a server-rendered page.
func (s *Store) SavedTitleIDs(ctx context.Context, userID int64) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT title_id FROM user_bookmarks WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SavedTitles returns the user's saved titles, newest save first, using the same
// column list as the discovery grid so /bookmarks can render them with the very
// same "card" partial.
func (s *Store) SavedTitles(ctx context.Context, userID int64) ([]TitleSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+titleSummaryColumns+`
		FROM titles t
		JOIN user_bookmarks b ON b.title_id = t.id
		WHERE b.user_id = ?
		ORDER BY b.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TitleSummary
	for rows.Next() {
		t, err := scanTitleSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddBookmark saves a title for a user. Idempotent: saving an already-saved
// title updates nothing and is not an error, so a double-click is harmless.
func (s *Store) AddBookmark(ctx context.Context, userID, titleID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_bookmarks (user_id, title_id) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE user_id = user_id`, userID, titleID)
	return err
}

// RemoveBookmark unsaves a title. Removing something that was never saved is a
// no-op, not an error.
func (s *Store) RemoveBookmark(ctx context.Context, userID, titleID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM user_bookmarks WHERE user_id = ? AND title_id = ?`, userID, titleID)
	return err
}

// MergeBookmarks folds a browser's pre-login localStorage list into the account.
// INSERT IGNORE makes it idempotent and lets ids that no longer exist in `titles`
// fall away on the foreign key instead of failing the whole batch — a stale
// localStorage list is expected input here, not an error.
func (s *Store) MergeBookmarks(ctx context.Context, userID int64, titleIDs []int64) error {
	if len(titleIDs) == 0 {
		return nil
	}
	if len(titleIDs) > maxBookmarks {
		titleIDs = titleIDs[:maxBookmarks]
	}
	placeholders := make([]string, 0, len(titleIDs))
	args := make([]any, 0, len(titleIDs)*2)
	for _, id := range titleIDs {
		placeholders = append(placeholders, "(?, ?)")
		args = append(args, userID, id)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT IGNORE INTO user_bookmarks (user_id, title_id) VALUES `+strings.Join(placeholders, ", "),
		args...)
	return err
}

// ClearBookmarks empties a user's saved list in one statement, backing the
// "clear all" button.
func (s *Store) ClearBookmarks(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_bookmarks WHERE user_id = ?`, userID)
	return err
}

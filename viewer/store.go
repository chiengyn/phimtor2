package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

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
}

// TitleFilter narrows the discovery list. Zero values mean "no constraint".
type TitleFilter struct {
	Query   string // free-text match against the (localized) title or original title
	GenreID int    // TMDB genre id; 0 = any
	Type    string // "movie" | "tv"; anything else = any
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
	if len(where) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(where, " AND "), args
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
	query := `SELECT id, tmdb_id, type, title, original_title, air_date, poster_path, vote_average FROM titles` +
		clause + " ORDER BY updated_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TitleSummary
	for rows.Next() {
		var t TitleSummary
		var origTitle, poster sql.NullString
		var air sql.NullTime
		var vote sql.NullFloat64
		if err := rows.Scan(&t.ID, &t.TMDBID, &t.Type, &t.Title, &origTitle, &air, &poster, &vote); err != nil {
			return nil, err
		}
		t.OriginalTitle = origTitle.String
		t.AirDate = dateStr(air)
		t.PosterPath = poster.String
		t.VoteAverage = vote.Float64
		out = append(out, t)
	}
	return out, rows.Err()
}

// SitemapEntry is one indexable title for the XML sitemap.
type SitemapEntry struct {
	ID        int64
	UpdatedAt time.Time
}

// SitemapTitles lists every title with its last-modified time, newest first,
// for sitemap.xml. Missing timestamps fall back to "now" so the entry is still
// emitted with a valid <lastmod>.
func (s *Store) SitemapTitles(ctx context.Context) ([]SitemapEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, updated_at FROM titles ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SitemapEntry
	for rows.Next() {
		var e SitemapEntry
		var updated sql.NullTime
		if err := rows.Scan(&e.ID, &updated); err != nil {
			return nil, err
		}
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
		`SELECT id, tmdb_id, type, title, original_title, air_date, poster_path, vote_average FROM titles ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var order []int64 // title ids in newest-first order
	byID := map[int64]TitleSummary{}
	var all, movies, tv []TitleSummary
	for rows.Next() {
		var t TitleSummary
		var origTitle, poster sql.NullString
		var air sql.NullTime
		var vote sql.NullFloat64
		if err := rows.Scan(&t.ID, &t.TMDBID, &t.Type, &t.Title, &origTitle, &air, &poster, &vote); err != nil {
			return nil, err
		}
		t.OriginalTitle = origTitle.String
		t.AirDate = dateStr(air)
		t.PosterPath = poster.String
		t.VoteAverage = vote.Float64
		byID[t.ID] = t
		order = append(order, t.ID)
		all = append(all, t)
		switch t.Type {
		case "movie":
			movies = append(movies, t)
		case "tv":
			tv = append(tv, t)
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

// GetVideo loads one video by id (with its source's info_hash and magnet), used
// by the prepare endpoint to add the torrent to the streamer. Returns (nil, nil)
// when no such video exists.
func (s *Store) GetVideo(ctx context.Context, id int64) (*Video, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+videoColumns+`
		FROM videos v
		JOIN torrent_sources src ON src.id = v.source_id
		WHERE v.id = ?`, id)
	v, err := scanVideo(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
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

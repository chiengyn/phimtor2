package main

import (
	"context"
	"database/sql"
	"fmt"
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
	ID         int64
	TMDBID     int
	Type       string
	Title      string
	AirDate    string
	PosterPath string
}

// TitleFilter narrows the discovery list. Zero values mean "no constraint".
type TitleFilter struct {
	Query   string // free-text match against the (localized) title
	GenreID int    // TMDB genre id; 0 = any
	Type    string // "movie" | "tv"; anything else = any
}

// ListTitles returns title summaries matching the filter, newest first.
func (s *Store) ListTitles(ctx context.Context, f TitleFilter) ([]TitleSummary, error) {
	var where []string
	var args []any

	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, "title LIKE ?")
		args = append(args, "%"+q+"%")
	}
	if f.GenreID > 0 {
		where = append(where, "id IN (SELECT title_id FROM title_genres WHERE genre_id = ?)")
		args = append(args, f.GenreID)
	}
	if f.Type == "movie" || f.Type == "tv" {
		where = append(where, "type = ?")
		args = append(args, f.Type)
	}

	query := `SELECT id, tmdb_id, type, title, air_date, poster_path FROM titles`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY updated_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TitleSummary
	for rows.Next() {
		var t TitleSummary
		var air sql.NullTime
		var poster sql.NullString
		if err := rows.Scan(&t.ID, &t.TMDBID, &t.Type, &t.Title, &air, &poster); err != nil {
			return nil, err
		}
		t.AirDate = dateStr(air)
		t.PosterPath = poster.String
		out = append(out, t)
	}
	return out, rows.Err()
}

// Row is a labelled horizontal strip of titles on the browse home page.
type Row struct {
	Key    string // query string that re-filters to this row, e.g. "type=movie"
	Label  string
	Titles []TitleSummary
}

// ListRows groups every title into Netflix-style browse rows: one row per type
// (movies, then TV) followed by one row per genre, newest titles first within
// each row. Empty rows are omitted.
func (s *Store) ListRows(ctx context.Context) ([]Row, error) {
	// Load every title once, newest first.
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tmdb_id, type, title, air_date, poster_path FROM titles ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var order []int64 // title ids in newest-first order
	byID := map[int64]TitleSummary{}
	var movies, tv []TitleSummary
	for rows.Next() {
		var t TitleSummary
		var air sql.NullTime
		var poster sql.NullString
		if err := rows.Scan(&t.ID, &t.TMDBID, &t.Type, &t.Title, &air, &poster); err != nil {
			return nil, err
		}
		t.AirDate = dateStr(air)
		t.PosterPath = poster.String
		byID[t.ID] = t
		order = append(order, t.ID)
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
	if len(movies) > 0 {
		out = append(out, Row{Key: "type=movie", Label: "Phim lẻ", Titles: movies})
	}
	if len(tv) > 0 {
		out = append(out, Row{Key: "type=tv", Label: "Phim bộ", Titles: tv})
	}
	for _, gid := range genreOrder {
		if ts := genreTitles[gid]; len(ts) > 0 {
			out = append(out, Row{Key: fmt.Sprintf("genre=%d", gid), Label: genreName[gid], Titles: ts})
		}
	}
	return out, nil
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

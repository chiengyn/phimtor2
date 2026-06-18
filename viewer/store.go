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

func dateStr(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format("2006-01-02")
}

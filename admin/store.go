package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

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

// Migrate applies every embedded migration in migrations/ that has not yet run,
// in filename order, recording each one in the schema_migrations table so it is
// never applied twice. Add new schema changes as additional numbered .sql files
// (e.g. migrations/0002_add_foo.sql); existing files must never be edited.
//
// Within each file, statements are split on ";" and run individually. "--" line
// comments are stripped first so a semicolon inside a comment cannot split a
// statement.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    VARCHAR(255) NOT NULL PRIMARY KEY,
			applied_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if applied[name] {
			continue
		}
		if err := s.applyMigration(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) appliedMigrations(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func (s *Store) applyMigration(ctx context.Context, name string) error {
	body, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", name, err)
	}
	for _, stmt := range strings.Split(stripLineComments(string(body)), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration %s: exec %q: %w", name, firstLine(stmt), err)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO schema_migrations (version) VALUES (?)`, name); err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	return nil
}

// UpsertTitle inserts or refreshes a title and all of its genres, and (for TV)
// its seasons and episodes, atomically. On return t.ID holds the internal id.
func (s *Store) UpsertTitle(ctx context.Context, t *Title) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO titles
			(tmdb_id, type, title, original_title, overview, air_date, runtime,
			 poster_path, backdrop_path, vote_average, original_language, status)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			id = LAST_INSERT_ID(id),
			title = VALUES(title),
			original_title = VALUES(original_title),
			overview = VALUES(overview),
			air_date = VALUES(air_date),
			runtime = VALUES(runtime),
			poster_path = VALUES(poster_path),
			backdrop_path = VALUES(backdrop_path),
			vote_average = VALUES(vote_average),
			original_language = VALUES(original_language),
			status = VALUES(status)`,
		t.TMDBID, t.Type, t.Title, t.OriginalTitle, nullStr(t.Overview),
		dateArg(t.AirDate), t.Runtime, nullStr(t.PosterPath), nullStr(t.BackdropPath),
		t.VoteAverage, nullStr(t.OriginalLanguage), nullStr(t.Status))
	if err != nil {
		return fmt.Errorf("upsert title: %w", err)
	}
	titleID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	t.ID = titleID

	// Genres + join rows (replace the set wholesale).
	if _, err := tx.ExecContext(ctx, `DELETE FROM title_genres WHERE title_id = ?`, titleID); err != nil {
		return fmt.Errorf("clear title_genres: %w", err)
	}
	for _, g := range t.Genres {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO genres (id, name) VALUES (?, ?) ON DUPLICATE KEY UPDATE name = VALUES(name)`,
			g.ID, g.Name); err != nil {
			return fmt.Errorf("upsert genre: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO title_genres (title_id, genre_id) VALUES (?, ?)`,
			titleID, g.ID); err != nil {
			return fmt.Errorf("link genre: %w", err)
		}
	}

	// Seasons + episodes (TV only).
	for i := range t.Seasons {
		se := &t.Seasons[i]
		sres, err := tx.ExecContext(ctx, `
			INSERT INTO seasons (title_id, season_number, name, overview, air_date, poster_path)
			VALUES (?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE
				id = LAST_INSERT_ID(id),
				name = VALUES(name),
				overview = VALUES(overview),
				air_date = VALUES(air_date),
				poster_path = VALUES(poster_path)`,
			titleID, se.SeasonNumber, se.Name, nullStr(se.Overview),
			dateArg(se.AirDate), nullStr(se.PosterPath))
		if err != nil {
			return fmt.Errorf("upsert season: %w", err)
		}
		seasonID, err := sres.LastInsertId()
		if err != nil {
			return err
		}
		se.ID = seasonID

		if _, err := tx.ExecContext(ctx, `DELETE FROM episodes WHERE season_id = ?`, seasonID); err != nil {
			return fmt.Errorf("clear episodes: %w", err)
		}
		for _, ep := range se.Episodes {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO episodes (season_id, episode_number, name, overview, air_date, runtime, still_path)
				VALUES (?,?,?,?,?,?,?)`,
				seasonID, ep.EpisodeNumber, ep.Name, nullStr(ep.Overview),
				dateArg(ep.AirDate), ep.Runtime, nullStr(ep.StillPath)); err != nil {
				return fmt.Errorf("insert episode: %w", err)
			}
		}
	}

	return tx.Commit()
}

// TitleSummary is the lightweight row returned by ListTitles.
type TitleSummary struct {
	ID         int64  `json:"id"`
	TMDBID     int    `json:"tmdb_id"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	AirDate    string `json:"air_date"`
	PosterPath string `json:"poster_path"`
}

func (s *Store) ListTitles(ctx context.Context) ([]TitleSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tmdb_id, type, title, air_date, poster_path
		FROM titles ORDER BY updated_at DESC`)
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

// DeleteTitle removes a title; genres/seasons/episodes cascade via FKs. Returns
// false when no row matched.
func (s *Store) DeleteTitle(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM titles WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// --- small helpers for NULL-able columns ---

func nullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func dateArg(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func dateStr(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format("2006-01-02")
}

// stripLineComments removes "--" comment lines so the semicolon splitter only
// sees real statement terminators.
func stripLineComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

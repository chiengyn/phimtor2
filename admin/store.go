package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
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

type translationBackfillItem struct {
	ID     int64
	TMDBID int
	Type   string
}

// NextMissingTranslation returns one catalog item that has not yet received the
// requested locale. One-at-a-time processing keeps TMDB traffic predictable,
// especially for TV titles whose seasons require additional requests.
func (s *Store) NextMissingTranslation(ctx context.Context, locale string, afterID int64) (*translationBackfillItem, error) {
	var item translationBackfillItem
	err := s.db.QueryRowContext(ctx, `
		SELECT t.id, t.tmdb_id, t.type
		FROM titles t
		WHERE t.id > ? AND (
			NOT EXISTS (
				SELECT 1 FROM title_translations tr
				WHERE tr.title_id = t.id AND tr.locale = ?
			)
			OR EXISTS (
				SELECT 1 FROM title_genres tg
				WHERE tg.title_id = t.id AND NOT EXISTS (
					SELECT 1 FROM genre_translations gr
					WHERE gr.genre_id = tg.genre_id AND gr.locale = ?
				)
			)
			OR EXISTS (
				SELECT 1 FROM seasons se
				WHERE se.title_id = t.id AND NOT EXISTS (
					SELECT 1 FROM season_translations sr
					WHERE sr.season_id = se.id AND sr.locale = ?
				)
			)
			OR EXISTS (
				SELECT 1 FROM episodes ep
				JOIN seasons se ON se.id = ep.season_id
				WHERE se.title_id = t.id AND NOT EXISTS (
					SELECT 1 FROM episode_translations er
					WHERE er.episode_id = ep.id AND er.locale = ?
				)
			)
		)
		ORDER BY t.id
		LIMIT 1`, afterID, locale, locale, locale, locale).Scan(&item.ID, &item.TMDBID, &item.Type)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

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
// its seasons and episodes, atomically. Seasons and episodes are upserted in
// place (not delete-and-reinsert), so refreshing a title's metadata preserves
// the videos/subtitles attached to its episodes. On return t.ID holds the
// internal id.
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
	for _, tr := range t.Translations {
		if strings.TrimSpace(tr.Locale) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO title_translations
				(title_id, locale, title, overview, poster_path, backdrop_path)
			VALUES (?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE
				title = VALUES(title), overview = VALUES(overview),
				poster_path = VALUES(poster_path), backdrop_path = VALUES(backdrop_path)`,
			titleID, tr.Locale, tr.Title, nullStr(tr.Overview),
			nullStr(tr.PosterPath), nullStr(tr.BackdropPath)); err != nil {
			return fmt.Errorf("upsert title translation %s: %w", tr.Locale, err)
		}
	}

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
		for _, tr := range g.Translations {
			if strings.TrimSpace(tr.Locale) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO genre_translations (genre_id, locale, name) VALUES (?,?,?)
				ON DUPLICATE KEY UPDATE name = VALUES(name)`, g.ID, tr.Locale, tr.Name); err != nil {
				return fmt.Errorf("upsert genre translation %s: %w", tr.Locale, err)
			}
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
		for _, tr := range se.Translations {
			if strings.TrimSpace(tr.Locale) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO season_translations (season_id, locale, name, overview, poster_path)
				VALUES (?,?,?,?,?)
				ON DUPLICATE KEY UPDATE name = VALUES(name), overview = VALUES(overview), poster_path = VALUES(poster_path)`,
				seasonID, tr.Locale, tr.Name, nullStr(tr.Overview), nullStr(tr.PosterPath)); err != nil {
				return fmt.Errorf("upsert season translation %s: %w", tr.Locale, err)
			}
		}

		// Upsert episodes keyed on uniq_episode (season_id, episode_number) rather
		// than delete-and-reinsert, so an existing episode keeps its id — and thus
		// any videos/subtitles attached to it (which cascade-delete with the row).
		// This makes a metadata refresh non-destructive to attached media.
		for i := range se.Episodes {
			ep := &se.Episodes[i]
			eres, err := tx.ExecContext(ctx, `
				INSERT INTO episodes (season_id, episode_number, name, overview, air_date, runtime, still_path)
				VALUES (?,?,?,?,?,?,?)
				ON DUPLICATE KEY UPDATE
					id = LAST_INSERT_ID(id),
					name = VALUES(name),
					overview = VALUES(overview),
					air_date = VALUES(air_date),
					runtime = VALUES(runtime),
					still_path = VALUES(still_path)`,
				seasonID, ep.EpisodeNumber, ep.Name, nullStr(ep.Overview),
				dateArg(ep.AirDate), ep.Runtime, nullStr(ep.StillPath))
			if err != nil {
				return fmt.Errorf("upsert episode: %w", err)
			}
			epID, err := eres.LastInsertId()
			if err != nil {
				return err
			}
			ep.ID = epID
			for _, tr := range ep.Translations {
				if strings.TrimSpace(tr.Locale) == "" {
					continue
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO episode_translations (episode_id, locale, name, overview)
					VALUES (?,?,?,?)
					ON DUPLICATE KEY UPDATE name = VALUES(name), overview = VALUES(overview)`,
					epID, tr.Locale, tr.Name, nullStr(tr.Overview)); err != nil {
					return fmt.Errorf("upsert episode translation %s: %w", tr.Locale, err)
				}
			}
		}
	}

	return tx.Commit()
}

// TitleSummary is the lightweight row returned by ListTitles.
type TitleSummary struct {
	ID            int64  `json:"id"`
	TMDBID        int    `json:"tmdb_id"`
	Type          string `json:"type"`
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title"`
	AirDate       string `json:"air_date"`
	PosterPath    string `json:"poster_path"`
}

// titleSearchWhere builds the WHERE clause (and its args) that filters titles by
// a free-text query against the localized and original titles. An empty query
// matches everything. Shared by CountTitles and ListTitles so the count and the
// page stay in sync.
func titleSearchWhere(q string) (string, []any) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", nil
	}
	like := "%" + q + "%"
	return " WHERE title LIKE ? OR original_title LIKE ?", []any{like, like}
}

// CountTitles returns the number of titles matching the (optional) search query,
// for computing the number of catalogue-list pages.
func (s *Store) CountTitles(ctx context.Context, q string) (int, error) {
	where, args := titleSearchWhere(q)
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM titles`+where, args...).Scan(&n)
	return n, err
}

// ListTitles returns one page of title summaries matching the (optional) search
// query, newest first; offset skips earlier pages.
func (s *Store) ListTitles(ctx context.Context, q string, limit, offset int) ([]TitleSummary, error) {
	where, args := titleSearchWhere(q)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tmdb_id, type, title, original_title, air_date, poster_path
		FROM titles`+where+` ORDER BY updated_at DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TitleSummary
	for rows.Next() {
		var t TitleSummary
		var air sql.NullTime
		var poster, orig sql.NullString
		if err := rows.Scan(&t.ID, &t.TMDBID, &t.Type, &t.Title, &orig, &air, &poster); err != nil {
			return nil, err
		}
		t.OriginalTitle = orig.String
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
		byEpisode, err := s.loadVideosForEpisodes(ctx, id)
		if err != nil {
			return nil, err
		}
		subsByEpisode, err := s.loadSubtitlesForEpisodes(ctx, id)
		if err != nil {
			return nil, err
		}
		for i := range t.Seasons {
			for j := range t.Seasons[i].Episodes {
				ep := &t.Seasons[i].Episodes[j]
				ep.Videos = byEpisode[ep.ID]
				ep.Subtitles = subsByEpisode[ep.ID]
			}
		}
	} else {
		if t.Videos, err = s.loadVideosForTitle(ctx, id); err != nil {
			return nil, err
		}
		if t.Subtitles, err = s.SubtitlesForTitle(ctx, id); err != nil {
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

// ListFeatured returns the hand-picked hero-billboard titles in display order
// (position ascending). Joined to titles so a title deleted out from under the
// featured row simply drops out.
func (s *Store) ListFeatured(ctx context.Context) ([]TitleSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.tmdb_id, t.type, t.title, t.original_title, t.air_date, t.poster_path
		FROM featured_titles f
		JOIN titles t ON t.id = f.title_id
		ORDER BY f.position ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TitleSummary
	for rows.Next() {
		var t TitleSummary
		var air sql.NullTime
		var poster, orig sql.NullString
		if err := rows.Scan(&t.ID, &t.TMDBID, &t.Type, &t.Title, &orig, &air, &poster); err != nil {
			return nil, err
		}
		t.OriginalTitle = orig.String
		t.AirDate = dateStr(air)
		t.PosterPath = poster.String
		out = append(out, t)
	}
	return out, rows.Err()
}

// SearchFeaturable returns titles matching q that are NOT already featured, for
// the "add to featured" picker. Newest first, capped at limit.
func (s *Store) SearchFeaturable(ctx context.Context, q string, limit int) ([]TitleSummary, error) {
	where, args := titleSearchWhere(q)
	clause := " WHERE"
	if where != "" {
		clause = where + " AND"
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tmdb_id, type, title, original_title, air_date, poster_path
		FROM titles`+clause+` id NOT IN (SELECT title_id FROM featured_titles)
		ORDER BY updated_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TitleSummary
	for rows.Next() {
		var t TitleSummary
		var air sql.NullTime
		var poster, orig sql.NullString
		if err := rows.Scan(&t.ID, &t.TMDBID, &t.Type, &t.Title, &orig, &air, &poster); err != nil {
			return nil, err
		}
		t.OriginalTitle = orig.String
		t.AirDate = dateStr(air)
		t.PosterPath = poster.String
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddFeatured appends a title to the featured list at the next position. It is
// idempotent: featuring an already-featured title leaves it (and its position)
// untouched. The FK rejects a title id that does not exist.
func (s *Store) AddFeatured(ctx context.Context, titleID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(position), 0) + 1 FROM featured_titles`).Scan(&next); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO featured_titles (title_id, position) VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE position = position`, titleID, next); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveFeatured drops a title from the featured list. ok is false when it was
// not featured. Remaining positions keep their order (gaps are harmless).
func (s *Store) RemoveFeatured(ctx context.Context, titleID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM featured_titles WHERE title_id = ?`, titleID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MoveFeatured shifts a featured title one slot earlier (up) or later (down) by
// swapping positions with its neighbor. A move past either end is a no-op.
func (s *Store) MoveFeatured(ctx context.Context, titleID int64, up bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var pos int
	err = tx.QueryRowContext(ctx, `SELECT position FROM featured_titles WHERE title_id = ?`, titleID).Scan(&pos)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	// The adjacent row in the move direction, if any.
	neighbor := `SELECT title_id, position FROM featured_titles WHERE position > ? ORDER BY position ASC LIMIT 1`
	if up {
		neighbor = `SELECT title_id, position FROM featured_titles WHERE position < ? ORDER BY position DESC LIMIT 1`
	}
	var nbID int64
	var nbPos int
	err = tx.QueryRowContext(ctx, neighbor, pos).Scan(&nbID, &nbPos)
	if err == sql.ErrNoRows {
		return nil // already at the end/start
	}
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE featured_titles SET position = ? WHERE title_id = ?`, nbPos, titleID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE featured_titles SET position = ? WHERE title_id = ?`, pos, nbID); err != nil {
		return err
	}
	return tx.Commit()
}

// TorrentSourceExists reports whether a torrent_sources row already exists
// for infoHash. Used by the crawl jobs (crawl.go) to skip a torrent they've
// already imported on a previous run.
func (s *Store) TorrentSourceExists(ctx context.Context, infoHash string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM torrent_sources WHERE info_hash = ? LIMIT 1`, infoHash).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// TitleExistsByTMDBID reports whether a title with the given TMDB id and
// type already exists. Used by the top-rated backfill crawl to skip a movie
// already in the catalog before spending a YTS lookup on it.
func (s *Store) TitleExistsByTMDBID(ctx context.Context, tmdbID int, mediaType string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM titles WHERE tmdb_id = ? AND type = ? LIMIT 1`, tmdbID, mediaType).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// --- videos & torrent sources ---

// execQuerier is the subset of *sql.DB / *sql.Tx that upsertTorrentSource needs,
// so it can run either standalone or inside a transaction.
type execQuerier interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

// upsertTorrentSource inserts the torrent source for an info_hash, or returns the
// existing one (refreshing the magnet, and backfilling the .torrent bytes if this
// call supplies them and the stored ones are NULL). Returns the source id.
func upsertTorrentSource(ctx context.Context, q execQuerier, infoHash, magnet string, torrentFile []byte) (int64, error) {
	var file interface{}
	if len(torrentFile) > 0 {
		file = torrentFile
	}
	res, err := q.ExecContext(ctx, `
		INSERT INTO torrent_sources (info_hash, magnet, torrent_file)
		VALUES (?,?,?)
		ON DUPLICATE KEY UPDATE
			id = LAST_INSERT_ID(id),
			magnet = VALUES(magnet),
			torrent_file = COALESCE(VALUES(torrent_file), torrent_file)`,
		infoHash, magnet, file)
	if err != nil {
		return 0, fmt.Errorf("upsert torrent source: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

// TorrentFileByInfoHash returns the stored raw .torrent bytes for a source, or
// nil when the source is magnet-only (or unknown). Used to attach the file to a
// watch-page add so the streamer can skip the DHT metadata fetch.
func (s *Store) TorrentFileByInfoHash(ctx context.Context, infoHash string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT torrent_file FROM torrent_sources WHERE info_hash = ?`, infoHash).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// VideoSource returns a video's torrent-source identity (info_hash + magnet) and
// whether the raw .torrent bytes are already stored, by video id. infoHash is ""
// when no such video exists. Used by the manual backfill button.
func (s *Store) VideoSource(ctx context.Context, id int64) (infoHash, magnet string, hasTorrentFile bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT src.info_hash, src.magnet, src.torrent_file IS NOT NULL
		FROM videos v JOIN torrent_sources src ON src.id = v.source_id
		WHERE v.id = ?`, id).Scan(&infoHash, &magnet, &hasTorrentFile)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return infoHash, magnet, hasTorrentFile, err
}

// BackfillTorrentFile stores the resolved .torrent bytes for a previously
// magnet-only source. The WHERE guard keeps it idempotent — it only fills a gap
// and never overwrites bytes already present, so a concurrent harvest and manual
// fetch can't clobber each other.
func (s *Store) BackfillTorrentFile(ctx context.Context, infoHash string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("empty torrent file")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE torrent_sources SET torrent_file = ? WHERE info_hash = ? AND torrent_file IS NULL`,
		data, infoHash)
	return err
}

// MagnetOnlyInfoHashes filters the given infohashes down to those whose source
// still has no stored .torrent bytes, so the harvester only fetches metainfo for
// sources that actually need it. Returns an empty slice for an empty input.
func (s *Store) MagnetOnlyInfoHashes(ctx context.Context, hashes []string) ([]string, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(hashes))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]interface{}, len(hashes))
	for i, h := range hashes {
		args[i] = h
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT info_hash FROM torrent_sources
		 WHERE torrent_file IS NULL AND info_hash IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// insertVideo writes one videos row, sharing the given source. Exactly one of
// v.TitleID / v.EpisodeID must be non-nil (mirrors chk_video_owner). On return
// v.ID and v.SourceID are set.
func insertVideo(ctx context.Context, q execQuerier, v *Video, sourceID int64) error {
	if (v.TitleID == nil) == (v.EpisodeID == nil) {
		return fmt.Errorf("video must reference exactly one of title or episode")
	}
	res, err := q.ExecContext(ctx, `
		INSERT INTO videos
			(source_id, title_id, episode_id, name, resolution, file_index, file_path, file_size)
		VALUES (?,?,?,?,?,?,?,?)`,
		sourceID, nullInt64(v.TitleID), nullInt64(v.EpisodeID), v.Name, v.Resolution,
		v.FileIndex, v.FilePath, v.FileSize)
	if err != nil {
		return fmt.Errorf("insert video: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	v.ID = id
	v.SourceID = sourceID
	return nil
}

// AddVideo upserts the torrent source for v.InfoHash and inserts one video that
// references it. torrentFile holds the raw .torrent bytes for file uploads, or
// nil for magnet input. On return v.ID and v.SourceID hold the new ids.
func (s *Store) AddVideo(ctx context.Context, v *Video, torrentFile []byte) error {
	if (v.TitleID == nil) == (v.EpisodeID == nil) {
		return fmt.Errorf("video must reference exactly one of title or episode")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	sourceID, err := upsertTorrentSource(ctx, tx, v.InfoHash, v.Magnet, torrentFile)
	if err != nil {
		return err
	}
	if err := insertVideo(ctx, tx, v, sourceID); err != nil {
		return err
	}
	return tx.Commit()
}

// AddVideoBatch upserts one torrent source and inserts one video per item, all
// sharing that source — this is how a season pack (one .torrent, many episode
// files) is stored without duplicating the magnet or the .torrent bytes. Each
// item must reference an episode. On return each item's ID and SourceID are set.
func (s *Store) AddVideoBatch(ctx context.Context, infoHash, magnet string, torrentFile []byte, items []*Video) error {
	if len(items) == 0 {
		return fmt.Errorf("no videos to add")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	sourceID, err := upsertTorrentSource(ctx, tx, infoHash, magnet, torrentFile)
	if err != nil {
		return err
	}
	for _, v := range items {
		if v.EpisodeID == nil {
			return fmt.Errorf("each video in a pack must reference an episode")
		}
		v.InfoHash, v.Magnet = infoHash, magnet
		if err := insertVideo(ctx, tx, v, sourceID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetVideo loads one video by id, with its source's info_hash and magnet (but not
// the raw .torrent bytes). Returns (nil, nil) when no such video exists.
func (s *Store) GetVideo(ctx context.Context, id int64) (*Video, error) {
	var v Video
	var titleID, episodeID sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT v.id, v.source_id, v.title_id, v.episode_id, v.name, v.resolution,
		       src.info_hash, src.magnet, v.file_index, v.file_path, v.file_size, v.created_at
		FROM videos v
		JOIN torrent_sources src ON src.id = v.source_id
		WHERE v.id = ?`, id).Scan(
		&v.ID, &v.SourceID, &titleID, &episodeID, &v.Name, &v.Resolution,
		&v.InfoHash, &v.Magnet, &v.FileIndex, &v.FilePath, &v.FileSize, &v.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
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

// TitleIDForEpisode resolves the owning title id for an episode (episode ->
// season -> title), so the player page can link back to the title detail. The
// bool is false when the episode does not exist.
func (s *Store) TitleIDForEpisode(ctx context.Context, episodeID int64) (int64, bool) {
	var titleID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT s.title_id FROM episodes e
		JOIN seasons s ON s.id = e.season_id
		WHERE e.id = ?`, episodeID).Scan(&titleID)
	if err != nil {
		return 0, false
	}
	return titleID, true
}

// DeleteVideo removes a video, then reaps its torrent source if no other video
// still references it. Returns false when no video row matched.
func (s *Store) DeleteVideo(ctx context.Context, id int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var sourceID int64
	err = tx.QueryRowContext(ctx, `SELECT source_id FROM videos WHERE id = ?`, id).Scan(&sourceID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM videos WHERE id = ?`, id); err != nil {
		return false, err
	}
	// Reap the source only when this was its last video.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM torrent_sources
		WHERE id = ? AND NOT EXISTS (SELECT 1 FROM videos WHERE source_id = ?)`,
		sourceID, sourceID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// loadVideosForTitle returns the movie videos attached directly to a title.
func (s *Store) loadVideosForTitle(ctx context.Context, titleID int64) ([]Video, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.id, v.source_id, v.title_id, v.name, v.resolution, src.info_hash, src.magnet,
		       src.torrent_file IS NOT NULL AS has_torrent_file,
		       v.file_index, v.file_path, v.file_size, v.created_at
		FROM videos v
		JOIN torrent_sources src ON src.id = v.source_id
		WHERE v.title_id = ? ORDER BY v.resolution, v.created_at`, titleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Video
	for rows.Next() {
		var v Video
		var tid sql.NullInt64
		if err := rows.Scan(&v.ID, &v.SourceID, &tid, &v.Name, &v.Resolution, &v.InfoHash, &v.Magnet,
			&v.HasTorrentFile, &v.FileIndex, &v.FilePath, &v.FileSize, &v.CreatedAt); err != nil {
			return nil, err
		}
		if tid.Valid {
			v.TitleID = &tid.Int64
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// loadVideosForEpisodes returns, in one query, every episode video under a title
// keyed by episode_id (joining through seasons like loadSeasons does), so
// GetTitle can attach them without an N+1.
func (s *Store) loadVideosForEpisodes(ctx context.Context, titleID int64) (map[int64][]Video, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.id, v.source_id, v.episode_id, v.name, v.resolution, src.info_hash, src.magnet,
		       src.torrent_file IS NOT NULL AS has_torrent_file,
		       v.file_index, v.file_path, v.file_size, v.created_at
		FROM videos v
		JOIN torrent_sources src ON src.id = v.source_id
		JOIN episodes e ON e.id = v.episode_id
		JOIN seasons s ON s.id = e.season_id
		WHERE s.title_id = ? ORDER BY v.episode_id, v.resolution, v.created_at`, titleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byEpisode := map[int64][]Video{}
	for rows.Next() {
		var v Video
		var eid sql.NullInt64
		if err := rows.Scan(&v.ID, &v.SourceID, &eid, &v.Name, &v.Resolution, &v.InfoHash, &v.Magnet,
			&v.HasTorrentFile, &v.FileIndex, &v.FilePath, &v.FileSize, &v.CreatedAt); err != nil {
			return nil, err
		}
		if eid.Valid {
			v.EpisodeID = &eid.Int64
			byEpisode[eid.Int64] = append(byEpisode[eid.Int64], v)
		}
	}
	return byEpisode, rows.Err()
}

// --- subtitles ---

// AddSubtitle inserts one subtitle row (the file itself is already stored in a
// BlobStore by the caller). Exactly one of sub.TitleID / sub.EpisodeID must be
// set (mirrors chk_subtitle_owner). On return sub.ID holds the new id.
func (s *Store) AddSubtitle(ctx context.Context, sub *Subtitle) error {
	if (sub.TitleID == nil) == (sub.EpisodeID == nil) {
		return fmt.Errorf("subtitle must reference exactly one of title or episode")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO subtitles
			(title_id, episode_id, provider, provider_file_id, language, name,
			 download_count, format, storage_backend, storage_key, metadata)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		nullInt64(sub.TitleID), nullInt64(sub.EpisodeID), sub.Provider, sub.ProviderFileID,
		sub.Language, sub.Name, sub.DownloadCount, sub.Format, sub.StorageBackend,
		sub.StorageKey, nullJSON(sub.Metadata))
	if err != nil {
		return fmt.Errorf("insert subtitle: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	sub.ID = id
	if sub.TitleID != nil {
		return s.refreshTitleVietsub(ctx, *sub.TitleID)
	}
	return nil
}

// refreshTitleVietsub recomputes the denormalized titles.has_vietsub flag from
// the subtitle rows (recompute-from-truth, so it is idempotent and handles
// deleting one of several Vietnamese subtitles). updated_at is preserved so a
// flag flip doesn't reorder the viewer's newest-first browse lists.
func (s *Store) refreshTitleVietsub(ctx context.Context, titleID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE titles
		SET has_vietsub = EXISTS (SELECT 1 FROM subtitles WHERE title_id = ? AND language = 'vi'),
		    updated_at = updated_at
		WHERE id = ?`, titleID, titleID)
	if err != nil {
		return fmt.Errorf("refresh has_vietsub for title %d: %w", titleID, err)
	}
	return nil
}

// subtitleColumns is the shared SELECT list / scan order for subtitle rows.
const subtitleColumns = `id, title_id, episode_id, provider, provider_file_id, language,
	name, download_count, format, storage_backend, storage_key, metadata, created_at,
	added_by_user_id`

// scanSubtitle reads one subtitle row in subtitleColumns order.
func scanSubtitle(sc interface{ Scan(...any) error }) (Subtitle, error) {
	var sub Subtitle
	var titleID, episodeID, addedBy sql.NullInt64
	var meta []byte
	if err := sc.Scan(&sub.ID, &titleID, &episodeID, &sub.Provider, &sub.ProviderFileID,
		&sub.Language, &sub.Name, &sub.DownloadCount, &sub.Format, &sub.StorageBackend,
		&sub.StorageKey, &meta, &sub.CreatedAt, &addedBy); err != nil {
		return Subtitle{}, err
	}
	if titleID.Valid {
		sub.TitleID = &titleID.Int64
	}
	if episodeID.Valid {
		sub.EpisodeID = &episodeID.Int64
	}
	if addedBy.Valid {
		sub.AddedByUserID = &addedBy.Int64
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

// DeleteSubtitle removes a subtitle row and returns the deleted row (so the
// caller can delete its blob from the right backend). Returns (nil, nil) when no
// row matched.
func (s *Store) DeleteSubtitle(ctx context.Context, id int64) (*Subtitle, error) {
	sub, err := s.GetSubtitle(ctx, id)
	if err != nil || sub == nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM subtitles WHERE id = ?`, id); err != nil {
		return nil, err
	}
	if sub.TitleID != nil {
		if err := s.refreshTitleVietsub(ctx, *sub.TitleID); err != nil {
			return nil, err
		}
	}
	return sub, nil
}

// SubtitlesForTitle returns the subtitles attached directly to a movie title.
func (s *Store) SubtitlesForTitle(ctx context.Context, titleID int64) ([]Subtitle, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+subtitleColumns+` FROM subtitles WHERE title_id = ? ORDER BY language, created_at`, titleID)
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

// SubtitlesForEpisode returns the subtitles attached to a single TV episode.
func (s *Store) SubtitlesForEpisode(ctx context.Context, episodeID int64) ([]Subtitle, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+subtitleColumns+` FROM subtitles WHERE episode_id = ? ORDER BY language, created_at`, episodeID)
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

// loadSubtitlesForEpisodes returns, in one query, every episode subtitle under a
// title keyed by episode_id (joining through seasons like loadVideosForEpisodes),
// so GetTitle can attach them without an N+1.
func (s *Store) loadSubtitlesForEpisodes(ctx context.Context, titleID int64) (map[int64][]Subtitle, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sub.id, sub.title_id, sub.episode_id, sub.provider, sub.provider_file_id, sub.language,
		       sub.name, sub.download_count, sub.format, sub.storage_backend, sub.storage_key, sub.metadata, sub.created_at
		FROM subtitles sub
		JOIN episodes e ON e.id = sub.episode_id
		JOIN seasons se ON se.id = e.season_id
		WHERE se.title_id = ? ORDER BY sub.episode_id, sub.language, sub.created_at`, titleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byEpisode := map[int64][]Subtitle{}
	for rows.Next() {
		sub, err := scanSubtitle(rows)
		if err != nil {
			return nil, err
		}
		if sub.EpisodeID != nil {
			byEpisode[*sub.EpisodeID] = append(byEpisode[*sub.EpisodeID], sub)
		}
	}
	return byEpisode, rows.Err()
}

// --- small helpers for NULL-able columns ---

func nullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(p *int64) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

func nullJSON(m json.RawMessage) interface{} {
	if len(m) == 0 {
		return nil
	}
	return []byte(m)
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

// --- Viewer accounts ----------------------------------------------------------
//
// The admin OWNS these tables (migrations/0007_users.sql) and writes exactly TWO
// columns across them: users.comp_expires_at and users.comp_granted_at, the
// admin-granted 4K unlock (SetUserComp, below). Everything else here is written
// only by the public viewer — rows on login, user_bookmarks on save/unsave, and
// plan / plan_expires_at when an invoice settles.
//
// That split is load-bearing, not incidental: because the two services write
// DISJOINT columns, revoking a comp cannot cancel a pass the user paid for. Keep
// admin writes confined to comp_*.

// userSearchWhere builds the WHERE clause (and its args) filtering users by a
// free-text query against name and email. An empty query matches everything.
// Shared by CountUsers and ListUsers so the count and the page stay in sync.
func userSearchWhere(q string) (string, []any) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", nil
	}
	like := "%" + q + "%"
	return " WHERE u.name LIKE ? OR u.email LIKE ?", []any{like, like}
}

// CountUsers returns the number of accounts matching the (optional) search
// query, for computing the number of user-list pages.
func (s *Store) CountUsers(ctx context.Context, q string) (int, error) {
	where, args := userSearchWhere(q)
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users u`+where, args...).Scan(&n)
	return n, err
}

// ListUsers returns one page of accounts matching the (optional) search query,
// newest signup first, each with the number of titles they have saved.
func (s *Store) ListUsers(ctx context.Context, q string, limit, offset int) ([]User, error) {
	where, args := userSearchWhere(q)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.provider, u.provider_uid, u.email, u.email_verified, u.name,
		       u.avatar_url, u.plan, u.plan_expires_at, u.comp_expires_at, u.comp_granted_at,
		       u.is_blocked, u.last_login_at, u.created_at,
		       (SELECT COUNT(*) FROM user_bookmarks b WHERE b.user_id = u.id)
		FROM users u`+where+`
		ORDER BY u.created_at DESC
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		var lastLogin, planExpires, compExpires, compGranted sql.NullTime
		if err := rows.Scan(&u.ID, &u.Provider, &u.ProviderUID, &u.Email, &u.EmailVerified,
			&u.Name, &u.AvatarURL, &u.Plan, &planExpires, &compExpires, &compGranted,
			&u.IsBlocked, &lastLogin, &u.CreatedAt,
			&u.BookmarkCount); err != nil {
			return nil, err
		}
		if compExpires.Valid {
			t := compExpires.Time
			u.CompExpiresAt = &t
		}
		if compGranted.Valid {
			t := compGranted.Time
			u.CompGrantedAt = &t
		}
		if lastLogin.Valid {
			t := lastLogin.Time
			u.LastLoginAt = &t
		}
		if planExpires.Valid {
			t := planExpires.Time
			u.PlanExpiresAt = &t
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUserComp grants or revokes the complimentary 4K unlock for one account.
// until == nil revokes.
//
// This is the ONLY write this service makes to `users`, and it names ONLY the
// comp_ columns. That restriction is the whole safety argument for letting two
// services write this row: plan / plan_expires_at belong to the viewer's billing
// path, so revoking a comp provably cannot cancel a subscription the user paid
// for. Do not be tempted to "tidy up" plan here — setting it to 'free' would
// silently cancel a paying customer.
func (s *Store) SetUserComp(ctx context.Context, userID int64, until *time.Time) error {
	if until == nil {
		_, err := s.db.ExecContext(ctx,
			`UPDATE users SET comp_expires_at = NULL, comp_granted_at = NULL WHERE id = ?`, userID)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET comp_expires_at = ?, comp_granted_at = NOW() WHERE id = ?`, *until, userID)
	return err
}

// --- Billing, read-only -------------------------------------------------------
//
// Everything below is SELECT only, and must stay that way. The viewer is the
// sole writer of payment_invoices / user_title_unlocks / billing_chain_cursors
// (migrations/0008_billing.sql declares them here because the admin owns the
// schema, not because it owns the data). The admin reports on them at
// GET /payments; writing one from here would put a second writer on the settle
// path, racing the poller for the same row. Gifts go through SetUserComp.

// invoiceStatuses are the filter values the payments monitor accepts. The filter
// is matched against this set and never interpolated into SQL.
var invoiceStatuses = map[string]bool{"paid": true, "pending": true, "expired": true}

// invoiceStatusWhere builds the status filter. An unknown or empty status
// matches everything, so a hand-edited query string degrades to "all" rather
// than to an error.
func invoiceStatusWhere(status string) (string, []any) {
	if !invoiceStatuses[status] {
		return "", nil
	}
	return " WHERE i.status = ?", []any{status}
}

// CountInvoices is how many invoices match the (optional) status filter.
func (s *Store) CountInvoices(ctx context.Context, status string) (int, error) {
	where, args := invoiceStatusWhere(status)
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM payment_invoices i`+where, args...).Scan(&n)
	return n, err
}

// ListInvoices returns one page of invoices, newest first, each with the buyer
// and (for a title unlock) the film it bought. Both joins are LEFT: the title FK
// is ON DELETE SET NULL precisely so a receipt outlives the catalog row, and an
// INNER join would make those purchases vanish from the ledger.
func (s *Store) ListInvoices(ctx context.Context, status string, limit, offset int) ([]Invoice, error) {
	where, args := invoiceStatusWhere(status)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.ref, i.user_id, i.kind, i.plan_code, i.title_id, i.amount_usd_cents,
		       i.chain, i.pay_to, i.pay_amount, i.token, i.status, i.received, i.paid_chain,
		       i.tx_hash, i.expires_at, i.paid_at, i.created_at,
		       COALESCE(u.email, ''), COALESCE(u.name, ''), COALESCE(t.title, '')
		FROM payment_invoices i
		LEFT JOIN users  u ON u.id = i.user_id
		LEFT JOIN titles t ON t.id = i.title_id`+where+`
		ORDER BY i.created_at DESC
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Invoice
	for rows.Next() {
		var inv Invoice
		var titleID sql.NullInt64
		var paidAt sql.NullTime
		if err := rows.Scan(&inv.ID, &inv.Ref, &inv.UserID, &inv.Kind, &inv.PlanCode,
			&titleID, &inv.AmountUSDCents, &inv.Chain, &inv.PayTo, &inv.PayAmount,
			&inv.Token, &inv.Status, &inv.Received, &inv.PaidChain, &inv.TxHash,
			&inv.ExpiresAt, &paidAt, &inv.CreatedAt,
			&inv.UserEmail, &inv.UserName, &inv.TitleName); err != nil {
			return nil, err
		}
		if titleID.Valid {
			inv.TitleID = &titleID.Int64
		}
		if paidAt.Valid {
			t := paidAt.Time
			inv.PaidAt = &t
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// PaymentTotals summarises the ledger and the entitlements currently in force.
// Two queries because they are two different questions: the first counts what
// the ledger recorded, the second what is live right now.
//
// Every aggregate is wrapped in COALESCE because SUM() over an empty table is
// NULL, and this table is empty until the first sale.
func (s *Store) PaymentTotals(ctx context.Context) (PaymentTotals, error) {
	var t PaymentTotals
	// Every SUM is CAST to SIGNED, not merely COALESCEd. SUM() returns DECIMAL in
	// both MySQL and MariaDB, the driver hands a DECIMAL back as text, and text
	// with a fractional part will not scan into an int. The CAST makes the wire
	// format an integer regardless of how the server chose to type the sum.
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       CAST(COALESCE(SUM(status = 'paid'), 0) AS SIGNED),
		       CAST(COALESCE(SUM(CASE WHEN status = 'paid'
		                             THEN amount_usd_cents ELSE 0 END), 0) AS SIGNED),
		       CAST(COALESCE(SUM(CASE WHEN status = 'paid'
		                              AND paid_at >= DATE_SUB(NOW(), INTERVAL 30 DAY)
		                             THEN amount_usd_cents ELSE 0 END), 0) AS SIGNED),
		       CAST(COALESCE(SUM(status = 'pending' AND expires_at > NOW()), 0) AS SIGNED),
		       CAST(COALESCE(SUM(status = 'expired'), 0) AS SIGNED)
		FROM payment_invoices`).Scan(
		&t.Invoices, &t.PaidCount, &t.PaidCents, &t.Cents30d, &t.PendingNow, &t.ExpiredCount)
	if err != nil {
		return PaymentTotals{}, err
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM users WHERE plan_expires_at > NOW()),
		       (SELECT COUNT(*) FROM users WHERE comp_expires_at > NOW()),
		       (SELECT COUNT(*) FROM user_title_unlocks)`).Scan(
		&t.ActivePasses, &t.ActiveComps, &t.TitleUnlocks)
	if err != nil {
		return PaymentTotals{}, err
	}
	return t, nil
}

// ListChainCursors returns every chain the viewer's poller has ever recorded
// progress for, most recently advanced first. A chain that disappeared from
// BILLING_EVM_CHAINS keeps its row and simply goes stale, which is the point —
// silently dropping a rail is exactly what this page should make visible.
func (s *Store) ListChainCursors(ctx context.Context) ([]ChainCursor, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT chain, scan_cursor, updated_at
		FROM billing_chain_cursors
		ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChainCursor
	for rows.Next() {
		var c ChainCursor
		if err := rows.Scan(&c.Chain, &c.Cursor, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListPayToAddresses returns the receive addresses the ledger has actually
// quoted to buyers, most recent first. See PayToUse for why this is worth a
// panel of its own: a misconfigured address fails silently, and this is where it
// becomes obvious.
func (s *Store) ListPayToAddresses(ctx context.Context, limit int) ([]PayToUse, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pay_to, COUNT(*), MAX(created_at)
		FROM payment_invoices
		GROUP BY pay_to
		ORDER BY MAX(created_at) DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PayToUse
	for rows.Next() {
		var p PayToUse
		if err := rows.Scan(&p.Address, &p.Invoices, &p.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

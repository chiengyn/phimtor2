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
		SELECT id, tmdb_id, type, title, air_date, poster_path
		FROM titles`+where+` ORDER BY updated_at DESC LIMIT ? OFFSET ?`, args...)
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
			&v.FileIndex, &v.FilePath, &v.FileSize, &v.CreatedAt); err != nil {
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
			&v.FileIndex, &v.FilePath, &v.FileSize, &v.CreatedAt); err != nil {
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
	return nil
}

// subtitleColumns is the shared SELECT list / scan order for subtitle rows.
const subtitleColumns = `id, title_id, episode_id, provider, provider_file_id, language,
	name, download_count, format, storage_backend, storage_key, metadata, created_at`

// scanSubtitle reads one subtitle row in subtitleColumns order.
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

package main

import (
	"encoding/json"
	"time"
)

// Domain types shared by the TMDB client (which builds them), the store (which
// persists/loads them) and the HTTP layer (which serializes them). Dates are
// kept as "YYYY-MM-DD" strings (empty when unknown) so they map cleanly to both
// the MySQL DATE columns and JSON.

type Title struct {
	ID               int64      `json:"id"`
	TMDBID           int        `json:"tmdb_id"`
	Type             string     `json:"type"` // "movie" | "tv"
	Title            string     `json:"title"`
	OriginalTitle    string     `json:"original_title"`
	Overview         string     `json:"overview"`
	AirDate          string     `json:"air_date"`
	Runtime          *int       `json:"runtime"`
	PosterPath       string     `json:"poster_path"`
	BackdropPath     string     `json:"backdrop_path"`
	VoteAverage      float64    `json:"vote_average"`
	OriginalLanguage string     `json:"original_language"`
	Status           string     `json:"status"`
	Genres           []Genre    `json:"genres,omitempty"`
	Seasons          []Season   `json:"seasons,omitempty"`
	Videos           []Video    `json:"videos,omitempty"`    // movie sources (TV uses Episode.Videos)
	Subtitles        []Subtitle `json:"subtitles,omitempty"` // movie subtitles (TV uses Episode.Subtitles)
	CreatedAt        time.Time  `json:"created_at,omitempty"`
	UpdatedAt        time.Time  `json:"updated_at,omitempty"`
}

type Genre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type Season struct {
	ID           int64     `json:"id"`
	SeasonNumber int       `json:"season_number"`
	Name         string    `json:"name"`
	Overview     string    `json:"overview"`
	AirDate      string    `json:"air_date"`
	PosterPath   string    `json:"poster_path"`
	Episodes     []Episode `json:"episodes,omitempty"`
}

type Episode struct {
	ID            int64      `json:"id"`
	EpisodeNumber int        `json:"episode_number"`
	Name          string     `json:"name"`
	Overview      string     `json:"overview"`
	AirDate       string     `json:"air_date"`
	Runtime       *int       `json:"runtime"`
	StillPath     string     `json:"still_path"`
	Videos        []Video    `json:"videos,omitempty"`
	Subtitles     []Subtitle `json:"subtitles,omitempty"`
}

// Video is one playable file for a movie or a single TV episode, at a given
// resolution. Exactly one of TitleID / EpisodeID is set. A video points at a
// torrent_sources row (SourceID) plus the FileIndex of its file inside that
// torrent; many videos can share one source (a season pack maps each episode to
// a different file in the same .torrent). InfoHash / Magnet are loaded from the
// source via JOIN. The raw .torrent bytes live on the source, never on the
// video, and are never sent to the browser.
type Video struct {
	ID         int64     `json:"id"`
	SourceID   int64     `json:"source_id,omitempty"`
	TitleID    *int64    `json:"title_id,omitempty"`
	EpisodeID  *int64    `json:"episode_id,omitempty"`
	Name       string    `json:"name"`
	Resolution string    `json:"resolution"` // "2160p" | "1080p" | "720p"
	InfoHash   string    `json:"info_hash"`
	Magnet     string    `json:"magnet"`
	FileIndex  int       `json:"file_index"`
	FilePath   string    `json:"file_path"`
	FileSize   int64     `json:"file_size"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
}

// Subtitle is one persisted subtitle file attached to a movie or a single TV
// episode (exactly one of TitleID / EpisodeID is set, mirroring Video). It was
// fetched from Provider (e.g. "opensubtitles") — ProviderFileID, DownloadCount
// and any extra Metadata are kept so the same file can be re-identified — and
// the file itself lives in a BlobStore: StorageBackend names which one ("local"
// or "s3") and StorageKey is its key/path there. Format is the on-disk format
// (currently always "vtt").
type Subtitle struct {
	ID             int64           `json:"id"`
	TitleID        *int64          `json:"title_id,omitempty"`
	EpisodeID      *int64          `json:"episode_id,omitempty"`
	Provider       string          `json:"provider"`
	ProviderFileID string          `json:"provider_file_id"`
	Language       string          `json:"language"`
	Name           string          `json:"name"`
	DownloadCount  int             `json:"download_count"`
	Format         string          `json:"format"`
	StorageBackend string          `json:"storage_backend"`
	StorageKey     string          `json:"storage_key"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	CreatedAt      time.Time       `json:"created_at,omitempty"`
}

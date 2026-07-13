package main

import (
	"encoding/json"
	"time"
)

// Domain types loaded from the shared MySQL database (owned and populated by the
// admin service) and rendered by the HTML templates. Dates are kept as
// "YYYY-MM-DD" strings (empty when unknown), matching the MySQL DATE columns.

type Title struct {
	ID               int64
	TMDBID           int
	Type             string // "movie" | "tv"
	Title            string
	OriginalTitle    string
	Overview         string
	AirDate          string
	Runtime          *int
	PosterPath       string
	BackdropPath     string
	VoteAverage      float64
	OriginalLanguage string
	Status           string
	Genres           []Genre
	Seasons          []Season
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Genre struct {
	ID   int
	Name string
}

type Season struct {
	ID           int64
	SeasonNumber int
	Name         string
	Overview     string
	AirDate      string
	PosterPath   string
	Episodes     []Episode
}

type Episode struct {
	ID            int64
	EpisodeNumber int
	Name          string
	Overview      string
	AirDate       string
	Runtime       *int
	StillPath     string
}

// Video is one playable file for a movie or a single TV episode, at a given
// resolution. Exactly one of TitleID / EpisodeID is set. InfoHash / Magnet come
// from the joined torrent_sources row; the raw .torrent bytes (TorrentFile) are
// loaded only by GetVideo. Mirrors admin's Video (the viewer reads what admin
// writes); the viewer never sends Magnet to the browser — it POSTs it to the
// streamer server-side.
type Video struct {
	ID         int64  `json:"id"`
	SourceID   int64  `json:"source_id,omitempty"`
	TitleID    *int64 `json:"title_id,omitempty"`
	EpisodeID  *int64 `json:"episode_id,omitempty"`
	Name       string `json:"name"`
	Resolution string `json:"resolution"` // "2160p" | "1080p" | "720p"
	InfoHash   string `json:"info_hash"`
	Magnet     string `json:"-"` // server-side only; never serialized to the browser
	// TorrentFile holds the raw .torrent bytes when the source has them stored
	// (magnet-only sources leave it nil). Server-side only — sent to the streamer
	// to skip the DHT metadata fetch, never serialized to the browser. Only the
	// single-video GetVideo load populates it; the list queries skip the blob.
	TorrentFile []byte    `json:"-"`
	FileIndex   int       `json:"file_index"`
	FilePath    string    `json:"file_path"`
	FileSize    int64     `json:"file_size"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
}

// Subtitle is one persisted subtitle file attached to a movie or single episode
// (exactly one of TitleID / EpisodeID set). The file lives in a BlobStore:
// StorageBackend names which one ("local" | "s3") and StorageKey is its key.
// Mirrors admin's Subtitle; the viewer reads it read-only.
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

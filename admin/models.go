package main

import "time"

// Domain types shared by the TMDB client (which builds them), the store (which
// persists/loads them) and the HTTP layer (which serializes them). Dates are
// kept as "YYYY-MM-DD" strings (empty when unknown) so they map cleanly to both
// the MySQL DATE columns and JSON.

type Title struct {
	ID               int64     `json:"id"`
	TMDBID           int       `json:"tmdb_id"`
	Type             string    `json:"type"` // "movie" | "tv"
	Title            string    `json:"title"`
	OriginalTitle    string    `json:"original_title"`
	Overview         string    `json:"overview"`
	AirDate          string    `json:"air_date"`
	Runtime          *int      `json:"runtime"`
	PosterPath       string    `json:"poster_path"`
	BackdropPath     string    `json:"backdrop_path"`
	VoteAverage      float64   `json:"vote_average"`
	OriginalLanguage string    `json:"original_language"`
	Status           string    `json:"status"`
	Genres           []Genre   `json:"genres,omitempty"`
	Seasons          []Season  `json:"seasons,omitempty"`
	Torrents         []Torrent `json:"torrents,omitempty"` // movie sources (TV uses Episode.Torrents)
	CreatedAt        time.Time `json:"created_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
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
	ID            int64     `json:"id"`
	EpisodeNumber int       `json:"episode_number"`
	Name          string    `json:"name"`
	Overview      string    `json:"overview"`
	AirDate       string    `json:"air_date"`
	Runtime       *int      `json:"runtime"`
	StillPath     string    `json:"still_path"`
	Torrents      []Torrent `json:"torrents,omitempty"`
}

// Torrent is one playable source for a movie or a single TV episode, at a given
// resolution. Exactly one of TitleID / EpisodeID is set. The raw .torrent file
// bytes are deliberately not a field here — they are never sent to the browser;
// they are written from a []byte argument in Store.AddTorrent and only read back
// by the (future) streamer re-add path.
type Torrent struct {
	ID         int64     `json:"id"`
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

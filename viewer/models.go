package main

import "time"

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

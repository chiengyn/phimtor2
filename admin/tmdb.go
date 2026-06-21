package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const tmdbBaseURL = "https://api.themoviedb.org/3"

// TMDBClient fetches movie/TV metadata. Every field is requested in the primary
// language (Vietnamese) and any field that comes back empty is backfilled from
// a second request in the fallback language (English).
type TMDBClient struct {
	apiKey       string
	lang         string
	fallbackLang string
	http         *http.Client
}

func NewTMDBClient(apiKey, lang, fallbackLang string) *TMDBClient {
	return &TMDBClient{
		apiKey:       apiKey,
		lang:         lang,
		fallbackLang: fallbackLang,
		http:         &http.Client{Timeout: 30 * time.Second},
	}
}

// FindMovieByIMDbID resolves a TMDB movie id from an IMDb id (e.g.
// "tt1234567"). Used by the crawl jobs (crawl.go) to match a YTS movie back
// to its TMDB metadata.
func (c *TMDBClient) FindMovieByIMDbID(ctx context.Context, imdbID string) (int, error) {
	var res struct {
		MovieResults []struct {
			ID int `json:"id"`
		} `json:"movie_results"`
	}
	if err := c.get(ctx, "/find/"+url.PathEscape(imdbID)+"?external_source=imdb_id", c.lang, &res); err != nil {
		return 0, fmt.Errorf("find by imdb id: %w", err)
	}
	if len(res.MovieResults) == 0 {
		return 0, fmt.Errorf("no TMDB movie found for IMDb id %s", imdbID)
	}
	return res.MovieResults[0].ID, nil
}

// ListTopRatedMovieIDs returns the TMDB movie ids on one page (20 per page)
// of the top-rated list, used by the top-rated backfill crawl.
func (c *TMDBClient) ListTopRatedMovieIDs(ctx context.Context, page int) ([]int, error) {
	var res struct {
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if err := c.get(ctx, "/movie/top_rated?page="+strconv.Itoa(page), c.lang, &res); err != nil {
		return nil, fmt.Errorf("list top rated: %w", err)
	}
	ids := make([]int, len(res.Results))
	for i, r := range res.Results {
		ids[i] = r.ID
	}
	return ids, nil
}

// movieImdbID fetches just the IMDb id for a TMDB movie id. Used by the
// top-rated backfill to look up a matching YTS torrent. Kept separate from
// fetchMovie/Title since imdb_id isn't a stored column.
func (c *TMDBClient) movieImdbID(ctx context.Context, id int) (string, error) {
	var res struct {
		ImdbID string `json:"imdb_id"`
	}
	if err := c.get(ctx, "/movie/"+strconv.Itoa(id), c.lang, &res); err != nil {
		return "", fmt.Errorf("get imdb id: %w", err)
	}
	return res.ImdbID, nil
}

// FetchTitle retrieves a full Title for the given media type and TMDB id.
func (c *TMDBClient) FetchTitle(ctx context.Context, mediaType string, id int) (*Title, error) {
	switch mediaType {
	case "movie":
		return c.fetchMovie(ctx, id)
	case "tv":
		return c.fetchTV(ctx, id)
	default:
		return nil, fmt.Errorf("unknown media type %q", mediaType)
	}
}

// --- TMDB response shapes (only the fields we store) ---

type tmdbMovie struct {
	ID               int     `json:"id"`
	Title            string  `json:"title"`
	OriginalTitle    string  `json:"original_title"`
	Overview         string  `json:"overview"`
	ReleaseDate      string  `json:"release_date"`
	Runtime          *int    `json:"runtime"`
	PosterPath       string  `json:"poster_path"`
	BackdropPath     string  `json:"backdrop_path"`
	VoteAverage      float64 `json:"vote_average"`
	OriginalLanguage string  `json:"original_language"`
	Status           string  `json:"status"`
	Genres           []Genre `json:"genres"`
}

type tmdbTV struct {
	ID               int     `json:"id"`
	Name             string  `json:"name"`
	OriginalName     string  `json:"original_name"`
	Overview         string  `json:"overview"`
	FirstAirDate     string  `json:"first_air_date"`
	EpisodeRunTime   []int   `json:"episode_run_time"`
	PosterPath       string  `json:"poster_path"`
	BackdropPath     string  `json:"backdrop_path"`
	VoteAverage      float64 `json:"vote_average"`
	OriginalLanguage string  `json:"original_language"`
	Status           string  `json:"status"`
	Genres           []Genre `json:"genres"`
	Seasons          []struct {
		SeasonNumber int `json:"season_number"`
	} `json:"seasons"`
}

type tmdbSeason struct {
	SeasonNumber int    `json:"season_number"`
	Name         string `json:"name"`
	Overview     string `json:"overview"`
	AirDate      string `json:"air_date"`
	PosterPath   string `json:"poster_path"`
	Episodes     []struct {
		EpisodeNumber int    `json:"episode_number"`
		Name          string `json:"name"`
		Overview      string `json:"overview"`
		AirDate       string `json:"air_date"`
		Runtime       *int   `json:"runtime"`
		StillPath     string `json:"still_path"`
	} `json:"episodes"`
}

func (c *TMDBClient) fetchMovie(ctx context.Context, id int) (*Title, error) {
	var m tmdbMovie
	if err := c.get(ctx, "/movie/"+strconv.Itoa(id), c.lang, &m); err != nil {
		return nil, err
	}
	if m.Overview == "" || m.Title == "" || len(m.Genres) == 0 {
		var fb tmdbMovie
		if err := c.get(ctx, "/movie/"+strconv.Itoa(id), c.fallbackLang, &fb); err == nil {
			m.Title = fill(m.Title, fb.Title)
			m.Overview = fill(m.Overview, fb.Overview)
			if len(m.Genres) == 0 {
				m.Genres = fb.Genres
			}
		}
	}

	return &Title{
		TMDBID:           m.ID,
		Type:             "movie",
		Title:            m.Title,
		OriginalTitle:    m.OriginalTitle,
		Overview:         m.Overview,
		AirDate:          m.ReleaseDate,
		Runtime:          m.Runtime,
		PosterPath:       m.PosterPath,
		BackdropPath:     m.BackdropPath,
		VoteAverage:      m.VoteAverage,
		OriginalLanguage: m.OriginalLanguage,
		Status:           m.Status,
		Genres:           m.Genres,
	}, nil
}

func (c *TMDBClient) fetchTV(ctx context.Context, id int) (*Title, error) {
	var t tmdbTV
	if err := c.get(ctx, "/tv/"+strconv.Itoa(id), c.lang, &t); err != nil {
		return nil, err
	}
	if t.Overview == "" || t.Name == "" || len(t.Genres) == 0 {
		var fb tmdbTV
		if err := c.get(ctx, "/tv/"+strconv.Itoa(id), c.fallbackLang, &fb); err == nil {
			t.Name = fill(t.Name, fb.Name)
			t.Overview = fill(t.Overview, fb.Overview)
			if len(t.Genres) == 0 {
				t.Genres = fb.Genres
			}
		}
	}

	var runtime *int
	if len(t.EpisodeRunTime) > 0 {
		runtime = &t.EpisodeRunTime[0]
	}

	title := &Title{
		TMDBID:           t.ID,
		Type:             "tv",
		Title:            t.Name,
		OriginalTitle:    t.OriginalName,
		Overview:         t.Overview,
		AirDate:          t.FirstAirDate,
		Runtime:          runtime,
		PosterPath:       t.PosterPath,
		BackdropPath:     t.BackdropPath,
		VoteAverage:      t.VoteAverage,
		OriginalLanguage: t.OriginalLanguage,
		Status:           t.Status,
		Genres:           t.Genres,
	}

	for _, s := range t.Seasons {
		season, err := c.fetchSeason(ctx, id, s.SeasonNumber)
		if err != nil {
			return nil, err
		}
		title.Seasons = append(title.Seasons, *season)
	}
	return title, nil
}

func (c *TMDBClient) fetchSeason(ctx context.Context, tvID, seasonNumber int) (*Season, error) {
	path := fmt.Sprintf("/tv/%d/season/%d", tvID, seasonNumber)
	var s tmdbSeason
	if err := c.get(ctx, path, c.lang, &s); err != nil {
		return nil, err
	}

	// Backfill empty overviews (season-level or any episode) from the fallback
	// language with a single extra request.
	needFallback := s.Overview == ""
	for _, ep := range s.Episodes {
		if ep.Overview == "" || ep.Name == "" {
			needFallback = true
			break
		}
	}
	if needFallback {
		var fb tmdbSeason
		if err := c.get(ctx, path, c.fallbackLang, &fb); err == nil {
			s.Name = fill(s.Name, fb.Name)
			s.Overview = fill(s.Overview, fb.Overview)
			fbEp := map[int]int{} // episode_number -> index in fb.Episodes
			for i, e := range fb.Episodes {
				fbEp[e.EpisodeNumber] = i
			}
			for i := range s.Episodes {
				if j, ok := fbEp[s.Episodes[i].EpisodeNumber]; ok {
					s.Episodes[i].Name = fill(s.Episodes[i].Name, fb.Episodes[j].Name)
					s.Episodes[i].Overview = fill(s.Episodes[i].Overview, fb.Episodes[j].Overview)
				}
			}
		}
	}

	season := &Season{
		SeasonNumber: s.SeasonNumber,
		Name:         s.Name,
		Overview:     s.Overview,
		AirDate:      s.AirDate,
		PosterPath:   s.PosterPath,
	}
	for _, e := range s.Episodes {
		season.Episodes = append(season.Episodes, Episode{
			EpisodeNumber: e.EpisodeNumber,
			Name:          e.Name,
			Overview:      e.Overview,
			AirDate:       e.AirDate,
			Runtime:       e.Runtime,
			StillPath:     e.StillPath,
		})
	}
	return season, nil
}

func (c *TMDBClient) get(ctx context.Context, path, language string, out any) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	u := fmt.Sprintf("%s%s%sapi_key=%s&language=%s",
		tmdbBaseURL, path, sep, url.QueryEscape(c.apiKey), url.QueryEscape(language))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e struct {
			StatusMessage string `json:"status_message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.StatusMessage != "" {
			return fmt.Errorf("tmdb %s: %s", path, e.StatusMessage)
		}
		return fmt.Errorf("tmdb %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func fill(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

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

// TMDBClient fetches movie/TV metadata in both configured languages. The
// primary response still feeds the legacy columns, with the fallback filling
// blanks, while both unmerged responses are retained as translation rows.
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
	SeasonNumber int           `json:"season_number"`
	Name         string        `json:"name"`
	Overview     string        `json:"overview"`
	AirDate      string        `json:"air_date"`
	PosterPath   string        `json:"poster_path"`
	Episodes     []tmdbEpisode `json:"episodes"`
}

type tmdbEpisode struct {
	EpisodeNumber int    `json:"episode_number"`
	Name          string `json:"name"`
	Overview      string `json:"overview"`
	AirDate       string `json:"air_date"`
	Runtime       *int   `json:"runtime"`
	StillPath     string `json:"still_path"`
}

func (c *TMDBClient) fetchMovie(ctx context.Context, id int) (*Title, error) {
	var m tmdbMovie
	if err := c.get(ctx, "/movie/"+strconv.Itoa(id), c.lang, &m); err != nil {
		return nil, err
	}
	var fb tmdbMovie
	fallbackOK := c.fallbackLang == c.lang
	if fallbackOK {
		fb = m
	} else if err := c.get(ctx, "/movie/"+strconv.Itoa(id), c.fallbackLang, &fb); err == nil {
		fallbackOK = true
	}

	primaryRaw := m
	m.Title = fill(m.Title, fb.Title)
	m.Overview = fill(m.Overview, fb.Overview)
	m.PosterPath = fill(m.PosterPath, fb.PosterPath)
	m.BackdropPath = fill(m.BackdropPath, fb.BackdropPath)
	m.Genres = mergeLocalizedGenres(m.Genres, fb.Genres, c.lang, c.fallbackLang, fallbackOK)

	title := &Title{
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
	}
	title.Translations = append(title.Translations, movieTranslation(primaryRaw, canonicalLocale(c.lang)))
	if fallbackOK && canonicalLocale(c.fallbackLang) != canonicalLocale(c.lang) {
		title.Translations = append(title.Translations, movieTranslation(fb, canonicalLocale(c.fallbackLang)))
	}
	return title, nil
}

func (c *TMDBClient) fetchTV(ctx context.Context, id int) (*Title, error) {
	var t tmdbTV
	if err := c.get(ctx, "/tv/"+strconv.Itoa(id), c.lang, &t); err != nil {
		return nil, err
	}
	var fb tmdbTV
	fallbackOK := c.fallbackLang == c.lang
	if fallbackOK {
		fb = t
	} else if err := c.get(ctx, "/tv/"+strconv.Itoa(id), c.fallbackLang, &fb); err == nil {
		fallbackOK = true
	}

	primaryRaw := t
	t.Name = fill(t.Name, fb.Name)
	t.Overview = fill(t.Overview, fb.Overview)
	t.PosterPath = fill(t.PosterPath, fb.PosterPath)
	t.BackdropPath = fill(t.BackdropPath, fb.BackdropPath)
	t.Genres = mergeLocalizedGenres(t.Genres, fb.Genres, c.lang, c.fallbackLang, fallbackOK)
	if len(t.Seasons) == 0 && fallbackOK {
		t.Seasons = fb.Seasons
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
	title.Translations = append(title.Translations, tvTranslation(primaryRaw, canonicalLocale(c.lang)))
	if fallbackOK && canonicalLocale(c.fallbackLang) != canonicalLocale(c.lang) {
		title.Translations = append(title.Translations, tvTranslation(fb, canonicalLocale(c.fallbackLang)))
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
	var fb tmdbSeason
	fallbackOK := c.fallbackLang == c.lang
	if fallbackOK {
		fb = s
	} else if err := c.get(ctx, path, c.fallbackLang, &fb); err == nil {
		fallbackOK = true
	}
	primaryRaw := s
	s.Name = fill(s.Name, fb.Name)
	s.Overview = fill(s.Overview, fb.Overview)
	s.PosterPath = fill(s.PosterPath, fb.PosterPath)
	if len(s.Episodes) == 0 && fallbackOK {
		s.Episodes = fb.Episodes
	}
	fbEpisodes := make(map[int]struct {
		Name, Overview string
	})
	if fallbackOK {
		for _, ep := range fb.Episodes {
			fbEpisodes[ep.EpisodeNumber] = struct{ Name, Overview string }{ep.Name, ep.Overview}
		}
	}

	season := &Season{
		SeasonNumber: s.SeasonNumber,
		Name:         s.Name,
		Overview:     s.Overview,
		AirDate:      s.AirDate,
		PosterPath:   s.PosterPath,
	}
	season.Translations = append(season.Translations, SeasonTranslation{
		Locale: canonicalLocale(c.lang), Name: primaryRaw.Name,
		Overview: primaryRaw.Overview, PosterPath: primaryRaw.PosterPath,
	})
	if fallbackOK && canonicalLocale(c.fallbackLang) != canonicalLocale(c.lang) {
		season.Translations = append(season.Translations, SeasonTranslation{
			Locale: canonicalLocale(c.fallbackLang), Name: fb.Name,
			Overview: fb.Overview, PosterPath: fb.PosterPath,
		})
	}
	for _, e := range s.Episodes {
		ep := Episode{
			EpisodeNumber: e.EpisodeNumber,
			Name:          fill(e.Name, fbEpisodes[e.EpisodeNumber].Name),
			Overview:      fill(e.Overview, fbEpisodes[e.EpisodeNumber].Overview),
			AirDate:       e.AirDate,
			Runtime:       e.Runtime,
			StillPath:     e.StillPath,
		}
		if raw, ok := seasonEpisode(primaryRaw, e.EpisodeNumber); ok {
			ep.Translations = append(ep.Translations, EpisodeTranslation{
				Locale: canonicalLocale(c.lang), Name: raw.Name, Overview: raw.Overview,
			})
		}
		if fallbackOK && canonicalLocale(c.fallbackLang) != canonicalLocale(c.lang) {
			raw, _ := seasonEpisode(fb, e.EpisodeNumber)
			ep.Translations = append(ep.Translations, EpisodeTranslation{
				Locale: canonicalLocale(c.fallbackLang), Name: raw.Name, Overview: raw.Overview,
			})
		}
		season.Episodes = append(season.Episodes, ep)
	}
	return season, nil
}

func canonicalLocale(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	if i := strings.IndexAny(language, "-_"); i >= 0 {
		language = language[:i]
	}
	return language
}

func movieTranslation(m tmdbMovie, locale string) TitleTranslation {
	return TitleTranslation{Locale: locale, Title: m.Title, Overview: m.Overview, PosterPath: m.PosterPath, BackdropPath: m.BackdropPath}
}

func tvTranslation(t tmdbTV, locale string) TitleTranslation {
	return TitleTranslation{Locale: locale, Title: t.Name, Overview: t.Overview, PosterPath: t.PosterPath, BackdropPath: t.BackdropPath}
}

func mergeLocalizedGenres(primary, fallback []Genre, primaryLang, fallbackLang string, fallbackOK bool) []Genre {
	byID := make(map[int]Genre, len(primary)+len(fallback))
	order := make([]int, 0, len(primary)+len(fallback))
	for _, g := range primary {
		byID[g.ID] = g
		order = append(order, g.ID)
	}
	for _, g := range fallback {
		if _, ok := byID[g.ID]; !ok {
			byID[g.ID] = g
			order = append(order, g.ID)
		}
	}
	fallbackNames := make(map[int]string, len(fallback))
	for _, g := range fallback {
		fallbackNames[g.ID] = g.Name
	}
	primaryNames := make(map[int]string, len(primary))
	for _, g := range primary {
		primaryNames[g.ID] = g.Name
	}
	out := make([]Genre, 0, len(order))
	for _, id := range order {
		g := byID[id]
		g.Name = fill(primaryNames[id], fallbackNames[id])
		g.Translations = append(g.Translations, GenreTranslation{Locale: canonicalLocale(primaryLang), Name: primaryNames[id]})
		if fallbackOK && canonicalLocale(fallbackLang) != canonicalLocale(primaryLang) {
			g.Translations = append(g.Translations, GenreTranslation{Locale: canonicalLocale(fallbackLang), Name: fallbackNames[id]})
		}
		out = append(out, g)
	}
	return out
}

func seasonEpisode(s tmdbSeason, number int) (tmdbEpisode, bool) {
	for _, ep := range s.Episodes {
		if ep.EpisodeNumber == number {
			return ep, true
		}
	}
	return tmdbEpisode{}, false
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

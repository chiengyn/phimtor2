package main

// The JSON surface the Android TV client reads.
//
// Every endpoint here is a thin wrapper over a store method the HTML handlers
// already use. That is the point: the TV app is a second CLIENT of this service,
// never a second implementation of it. In particular the watch endpoints below
// emit the very same watchVideo DTO the watch page receives, built by the very
// same s.toWatchVideos, so resolutionLock stays the single source of truth for
// what plays and why — and handlePrepareSource remains the one place that
// actually enforces it, for the TV exactly as for the browser.
//
// TWO things differ from every other route in this codebase, both because of one
// fact: a television cannot be force-refreshed the way a web page can, and an
// APK installed today will still be calling these URLs in two years.
//
//  1. The path is VERSIONED (/api/tv/v1/...). Nothing else here needs that.
//  2. The wire types are declared in this file rather than reusing the domain
//     structs directly, so renaming a field in models.go cannot silently break
//     a client nobody can update. Where a browser-facing DTO already exists
//     (watchVideo, watchSubtitle) it is reused as-is — those are already a
//     published contract.

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// Poster and backdrop buckets. A television is a 1080p-or-better display seen
// from across a room, so the w342 poster the web cards use looks soft on it.
const (
	tvPosterSize   = "w500"
	tvBackdropSize = "w1280"
	tvStillSize    = "w300"
)

// tvCard is one title in a row, a grid or a saved list.
type tvCard struct {
	ID          int64   `json:"id"`
	Type        string  `json:"type"`
	Title       string  `json:"title"`
	Original    string  `json:"original_title,omitempty"`
	Year        string  `json:"year,omitempty"`
	Poster      string  `json:"poster,omitempty"`
	VoteAverage float64 `json:"vote_average"`
	HasSubtitle bool    `json:"has_subtitle"`
}

type tvRow struct {
	Key    string   `json:"key"`
	Label  string   `json:"label"`
	Ranked bool     `json:"ranked"`
	Titles []tvCard `json:"titles"`
}

type tvGenre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type tvEpisode struct {
	ID            int64  `json:"id"`
	EpisodeNumber int    `json:"episode_number"`
	Name          string `json:"name"`
	Overview      string `json:"overview,omitempty"`
	AirDate       string `json:"air_date,omitempty"`
	Runtime       *int   `json:"runtime,omitempty"`
	Still         string `json:"still,omitempty"`
}

type tvSeason struct {
	ID           int64       `json:"id"`
	SeasonNumber int         `json:"season_number"`
	Name         string      `json:"name"`
	Overview     string      `json:"overview,omitempty"`
	AirDate      string      `json:"air_date,omitempty"`
	Episodes     []tvEpisode `json:"episodes"`
}

type tvTitle struct {
	ID          int64      `json:"id"`
	Type        string     `json:"type"`
	Title       string     `json:"title"`
	Original    string     `json:"original_title,omitempty"`
	Overview    string     `json:"overview,omitempty"`
	AirDate     string     `json:"air_date,omitempty"`
	Runtime     *int       `json:"runtime,omitempty"`
	Poster      string     `json:"poster,omitempty"`
	Backdrop    string     `json:"backdrop,omitempty"`
	VoteAverage float64    `json:"vote_average"`
	Genres      []tvGenre  `json:"genres"`
	Seasons     []tvSeason `json:"seasons,omitempty"`
}

// tvWatch is everything the player screen needs for one movie or episode. It
// mirrors watchData, minus the fields that exist only because the HTML template
// cannot reach the pageData envelope.
type tvWatch struct {
	Heading   string `json:"heading"`
	Sub       string `json:"sub,omitempty"`
	OwnerKind string `json:"owner_kind"` // "title" | "episode"
	OwnerID   int64  `json:"owner_id"`
	// TitleID is the entitlement owner. It differs from OwnerID on an episode,
	// where OwnerID is the episode and entitlements hang off its title.
	TitleID   int64           `json:"title_id"`
	Videos    []watchVideo    `json:"videos"`
	Subtitles []watchSubtitle `json:"subtitles"`
	// UpgradeURL is where to send someone who hit a paid lock — rendered as a QR
	// code on the TV, because nobody completes a crypto payment with a remote.
	// "" when billing is off, exactly as on the web.
	UpgradeURL   string      `json:"upgrade_url,omitempty"`
	SeasonNumber int         `json:"season_number,omitempty"`
	Episodes     []tvEpisode `json:"episodes,omitempty"`
}

// tvLocale resolves the locale for an API request. The X-Phimnet-Locale header
// is the existing convention for this service's JSON endpoints (it is what
// handlePrepareSource and the subtitle API read); ?locale= is accepted too
// because it is far easier to get right from a native HTTP client.
func tvLocale(r *http.Request) Locale {
	if locale, ok := parseLocale(r.Header.Get("X-Phimnet-Locale")); ok {
		return locale
	}
	if locale, ok := parseLocale(r.URL.Query().Get("locale")); ok {
		return locale
	}
	return LocaleVI
}

func toTVCard(t TitleSummary) tvCard {
	return tvCard{
		ID:          t.ID,
		Type:        t.Type,
		Title:       t.Title,
		Original:    t.OriginalTitle,
		Year:        yearOf(t.AirDate),
		Poster:      tmdbImageURL(tvPosterSize, t.PosterPath),
		VoteAverage: t.VoteAverage,
		HasSubtitle: t.HasSubtitle,
	}
}

func toTVCards(ts []TitleSummary) []tvCard {
	out := make([]tvCard, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTVCard(t))
	}
	return out
}

func toTVEpisodes(eps []Episode) []tvEpisode {
	out := make([]tvEpisode, 0, len(eps))
	for _, e := range eps {
		out = append(out, tvEpisode{
			ID:            e.ID,
			EpisodeNumber: e.EpisodeNumber,
			Name:          e.Name,
			Overview:      e.Overview,
			AirDate:       e.AirDate,
			Runtime:       e.Runtime,
			Still:         tmdbImageURL(tvStillSize, e.StillPath),
		})
	}
	return out
}

// handleTVHome returns the browse home: the curated hero picks plus the same
// Netflix-style rows the web home renders. Row labels are resolved here rather
// than shipped as keys, so a locale the app does not yet ship strings for still
// reads correctly.
func (s *Server) handleTVHome(w http.ResponseWriter, r *http.Request) {
	locale := tvLocale(r)
	rows, err := s.store.ListRows(r.Context(), locale)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "catalog unavailable")
		return
	}
	out := make([]tvRow, 0, len(rows))
	for _, row := range rows {
		label := row.Label
		if row.LabelKey != "" {
			label = tr(locale, row.LabelKey)
		}
		out = append(out, tvRow{
			Key:    row.Key,
			Label:  label,
			Ranked: row.Ranked,
			Titles: toTVCards(row.Titles),
		})
	}

	// The hero, built exactly as handleHome builds it: the admin's curated
	// featured_titles, reloaded in full for their backdrops. An empty list is a
	// valid answer — the app falls back to the first row's top titles, the same
	// fallback the web home makes.
	hero := make([]tvTitle, 0, tvHeroLimit)
	ids, err := s.store.FeaturedTitleIDs(r.Context(), tvHeroLimit)
	if err != nil {
		log.Printf("tv: featured titles: %v", err)
	}
	for _, id := range ids {
		t, err := s.store.GetTitle(r.Context(), locale, id)
		if err != nil || t == nil {
			continue
		}
		hero = append(hero, toTVTitle(t, false))
	}

	writeJSON(w, http.StatusOK, map[string]any{"hero": hero, "rows": out})
}

// tvHeroLimit is how many curated picks the hero carousel carries.
const tvHeroLimit = 10

// tvPageSize is the grid page size. Larger than the web's because a TV grid is
// scrolled with a D-pad and a short page means constant paging.
const tvPageSize = 60

// handleTVTitles is the paginated discovery grid behind search and the genre
// filters: the same ListTitles/CountTitles pair the web grid uses, so the page
// and the total can never disagree.
func (s *Server) handleTVTitles(w http.ResponseWriter, r *http.Request) {
	locale := tvLocale(r)
	q := r.URL.Query()
	filter := TitleFilter{
		Query:    q.Get("q"),
		Type:     q.Get("type"),
		Subtitle: q.Get("subtitle"),
	}
	if g, err := strconv.Atoi(q.Get("genre")); err == nil {
		filter.GenreID = g
	}
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}

	total, err := s.store.CountTitles(r.Context(), locale, filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "catalog unavailable")
		return
	}
	titles, err := s.store.ListTitles(r.Context(), locale, filter, tvPageSize, (page-1)*tvPageSize)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "catalog unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"page":      page,
		"page_size": tvPageSize,
		"total":     total,
		"titles":    toTVCards(titles),
	})
}

// handleTVGenres lists only genres that have at least one title, matching the
// web filter dropdown.
func (s *Server) handleTVGenres(w http.ResponseWriter, r *http.Request) {
	genres, err := s.store.ListGenres(r.Context(), tvLocale(r))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "catalog unavailable")
		return
	}
	out := make([]tvGenre, 0, len(genres))
	for _, g := range genres {
		out = append(out, tvGenre{ID: g.ID, Name: g.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"genres": out})
}

func toTVTitle(t *Title, withSeasons bool) tvTitle {
	out := tvTitle{
		ID:          t.ID,
		Type:        t.Type,
		Title:       t.Title,
		Original:    t.OriginalTitle,
		Overview:    t.Overview,
		AirDate:     t.AirDate,
		Runtime:     t.Runtime,
		Poster:      tmdbImageURL(tvPosterSize, t.PosterPath),
		Backdrop:    tmdbImageURL(tvBackdropSize, t.BackdropPath),
		VoteAverage: t.VoteAverage,
		Genres:      make([]tvGenre, 0, len(t.Genres)),
	}
	for _, g := range t.Genres {
		out.Genres = append(out.Genres, tvGenre{ID: g.ID, Name: g.Name})
	}
	if withSeasons {
		out.Seasons = make([]tvSeason, 0, len(t.Seasons))
		for _, se := range t.Seasons {
			out.Seasons = append(out.Seasons, tvSeason{
				ID:           se.ID,
				SeasonNumber: se.SeasonNumber,
				Name:         se.Name,
				Overview:     se.Overview,
				AirDate:      se.AirDate,
				Episodes:     toTVEpisodes(se.Episodes),
			})
		}
	}
	return out
}

// handleTVTitle is the detail screen: the full title with genres and, for a
// series, every season and episode.
func (s *Server) handleTVTitle(w http.ResponseWriter, r *http.Request) {
	locale := tvLocale(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid title id")
		return
	}
	t, err := s.store.GetTitle(r.Context(), locale, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "catalog unavailable")
		return
	}
	if t == nil {
		writeJSONError(w, http.StatusNotFound, "title not found")
		return
	}
	writeJSON(w, http.StatusOK, toTVTitle(t, true))
}

// handleTVWatchMovie and handleTVWatchEpisode are the endpoints that matter.
//
// They return the SAME watchVideo values the web watch page is given —
// available/lock stamped by s.toWatchVideos from s.resolutionLock — so the TV
// renders the identical three gates (sign in / upgrade / coming soon) without
// knowing a single rule behind them. Playback itself still goes through
// handlePrepareSource, which re-checks all of it server-side, because these
// fields are advisory presentation exactly as the web chips are.
func (s *Server) handleTVWatchMovie(w http.ResponseWriter, r *http.Request) {
	locale := tvLocale(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid title id")
		return
	}
	title, err := s.store.GetTitle(r.Context(), locale, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "catalog unavailable")
		return
	}
	if title == nil {
		writeJSONError(w, http.StatusNotFound, "title not found")
		return
	}
	videos, err := s.store.VideosForTitle(r.Context(), title.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "catalog unavailable")
		return
	}
	subs, err := s.store.SubtitlesForTitle(r.Context(), title.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "catalog unavailable")
		return
	}
	access := s.accessForTitle(r, title.ID, hasLockedResolution(videos))
	writeJSON(w, http.StatusOK, tvWatch{
		Heading:    title.Title,
		Sub:        yearOf(title.AirDate),
		OwnerKind:  "title",
		OwnerID:    title.ID,
		TitleID:    title.ID,
		Videos:     s.toWatchVideos(videos, access),
		Subtitles:  toWatchSubtitles(subs),
		UpgradeURL: s.upgradeURL(locale, title.ID),
	})
}

func (s *Server) handleTVWatchEpisode(w http.ResponseWriter, r *http.Request) {
	locale := tvLocale(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid episode id")
		return
	}
	ec, err := s.store.GetEpisodeContext(r.Context(), locale, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "catalog unavailable")
		return
	}
	if ec == nil {
		writeJSONError(w, http.StatusNotFound, "episode not found")
		return
	}
	videos, err := s.store.VideosForEpisode(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "catalog unavailable")
		return
	}
	subs, err := s.store.SubtitlesForEpisode(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "catalog unavailable")
		return
	}
	// Same-season siblings power "next episode" without a second round trip.
	eps, err := s.store.EpisodesInSeasonOf(r.Context(), locale, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "catalog unavailable")
		return
	}
	sub := tr(locale, "watch.episode_sub", ec.SeasonNumber, ec.EpisodeNumber)
	if ec.EpisodeName != "" {
		sub += ": " + ec.EpisodeName
	}
	access := s.accessForTitle(r, ec.TitleID, hasLockedResolution(videos))
	writeJSON(w, http.StatusOK, tvWatch{
		Heading:      ec.TitleName,
		Sub:          sub,
		OwnerKind:    "episode",
		OwnerID:      id,
		TitleID:      ec.TitleID,
		Videos:       s.toWatchVideos(videos, access),
		Subtitles:    toWatchSubtitles(subs),
		UpgradeURL:   s.upgradeURL(locale, ec.TitleID),
		SeasonNumber: ec.SeasonNumber,
		Episodes:     toTVEpisodes(eps),
	})
}

// handleTVMe reports who the caller is, so the app can render its account
// screen and decide whether to offer pairing. Anonymous is a 200 with
// signed_in false, not a 401 — browsing without an account is supported, and a
// 401 here would make the app treat it as an error state.
func (s *Server) handleTVMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if u == nil {
		writeJSON(w, http.StatusOK, map[string]any{"signed_in": false})
		return
	}
	now := time.Now()
	writeJSON(w, http.StatusOK, map[string]any{
		"signed_in":    true,
		"name":         u.DisplayName(),
		"email":        u.Email,
		"avatar":       u.AvatarURL,
		"entitled":     u.HasPass(now) || u.HasComp(now),
		"entitled_til": u.EntitledUntil(now),
	})
}

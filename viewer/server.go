package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

const tmdbImageBase = "https://image.tmdb.org/t/p/"

type Server struct {
	store    *Store
	router   chi.Router
	home     *template.Template
	detail   *template.Template
	watch    *template.Template
	notFound *template.Template
	grid     *template.Template

	// streamerPublicURL is injected into the watch page for the browser to reach
	// the streamer's stats + stream endpoints. streamer is the server-side client
	// the viewer uses to add torrents (the browser never adds directly). blobs
	// serves saved subtitle files read-only, keyed by storage backend name.
	streamerPublicURL string
	streamer          *streamerClient
	blobs             map[string]BlobStore
}

func NewServer(store *Store, cfg Config) (*Server, error) {
	blobs, err := newReadOnlyBlobStores(cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{
		store:             store,
		streamerPublicURL: strings.TrimRight(cfg.StreamerPublicURL, "/"),
		streamer:          newStreamerClient(cfg.StreamerInternalURL),
		blobs:             blobs,
	}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	s.setupRouter()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// funcMap holds the small helpers the templates rely on for TMDB image URLs and
// date formatting.
var funcMap = template.FuncMap{
	// img builds a TMDB image URL for a size bucket (e.g. "w342"); empty paths
	// yield "" so templates can fall back to a placeholder.
	"img": func(size, path string) string {
		if path == "" {
			return ""
		}
		return tmdbImageBase + size + path
	},
	// year extracts the 4-digit year from a "YYYY-MM-DD" date string.
	"year": yearOf,
	// rating formats a vote average to one decimal place.
	"rating": func(v float64) string {
		return fmt.Sprintf("%.1f", v)
	},
}

// yearOf extracts the 4-digit year from a "YYYY-MM-DD" date string.
func yearOf(date string) string {
	if len(date) >= 4 {
		return date[:4]
	}
	return ""
}

func (s *Server) parseTemplates() error {
	parse := func(files ...string) (*template.Template, error) {
		paths := make([]string, len(files))
		for i, f := range files {
			paths[i] = "templates/" + f
		}
		return template.New("").Funcs(funcMap).ParseFiles(paths...)
	}

	var err error
	if s.home, err = parse("layout.html", "home.html", "rows.html", "grid.html"); err != nil {
		return err
	}
	if s.detail, err = parse("layout.html", "detail.html"); err != nil {
		return err
	}
	if s.watch, err = parse("layout.html", "watch.html"); err != nil {
		return err
	}
	if s.notFound, err = parse("layout.html", "404.html"); err != nil {
		return err
	}
	if s.grid, err = parse("grid.html"); err != nil {
		return err
	}
	return nil
}

func (s *Server) setupRouter() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Liveness probe for kamal-proxy / load balancers (no DB access).
	r.Get("/up", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	r.Get("/", s.handleHome)
	r.Get("/titles", s.handleTitlesFragment)
	r.Get("/titles/{id}", s.handleDetail)
	r.Get("/watch/movie/{id}", s.handleWatchMovie)
	r.Get("/watch/episode/{id}", s.handleWatchEpisode)

	// Viewer-mediated playback API (same-origin, called by the watch page JS).
	r.Post("/api/sources/{videoID}/prepare", s.handlePrepareSource)
	r.Get("/api/subtitles/{id}/file", s.handleSubtitleFile)

	fs := http.FileServer(http.Dir("static"))
	r.Handle("/static/*", http.StripPrefix("/static/", fs))

	s.router = r
}

// filterFromQuery reads the discovery filter from the request query params.
func filterFromQuery(r *http.Request) TitleFilter {
	f := TitleFilter{
		Query: r.URL.Query().Get("q"),
		Type:  r.URL.Query().Get("type"),
	}
	if g := r.URL.Query().Get("genre"); g != "" {
		if id, err := strconv.Atoi(g); err == nil {
			f.GenreID = id
		}
	}
	return f
}

type homeData struct {
	Rows     []Row          // browse view (no active filter)
	Titles   []TitleSummary // flat results (filter active)
	Genres   []Genre
	Query    string
	GenreID  int
	Type     string
	Filtered bool
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	f := filterFromQuery(r)

	genres, err := s.store.ListGenres(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := homeData{
		Genres:   genres,
		Query:    f.Query,
		GenreID:  f.GenreID,
		Type:     f.Type,
		Filtered: f.Query != "" || f.GenreID > 0 || f.Type != "",
	}

	if data.Filtered {
		if data.Titles, err = s.store.ListTitles(r.Context(), f); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		if data.Rows, err = s.store.ListRows(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.render(w, s.home, "layout", data)
}

// handleTitlesFragment returns only the grid of cards, for htmx swaps.
func (s *Server) handleTitlesFragment(w http.ResponseWriter, r *http.Request) {
	f := filterFromQuery(r)
	titles, err := s.store.ListTitles(r.Context(), f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, s.grid, "grid", titles)
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.renderNotFound(w)
		return
	}
	title, err := s.store.GetTitle(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if title == nil {
		s.renderNotFound(w)
		return
	}
	s.render(w, s.detail, "layout", title)
}

// watchData drives the watch page. Videos and subtitles are serialized to JSON
// strings injected via data-* attributes; the page JS adds the chosen source to
// the streamer (via the same-origin prepare endpoint) and streams it.
type watchData struct {
	Heading       string
	Sub           string
	BackHref      string
	StreamerURL   string // public streamer base URL (browser-reachable)
	OwnerKind     string // "title" | "episode"
	OwnerID       int64
	VideosJSON    string // JSON array, injected via a data-* attribute
	SubtitlesJSON string // JSON array, injected via a data-* attribute
	HasVideo      bool
}

// watchVideo is the browser-facing subset of a Video (no magnet — the viewer
// adds it to the streamer server-side).
type watchVideo struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Resolution string `json:"resolution"`
	FileSize   int64  `json:"file_size"`
}

// watchSubtitle is the browser-facing subset of a Subtitle.
type watchSubtitle struct {
	ID            int64  `json:"id"`
	Language      string `json:"language"`
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	DownloadCount int    `json:"download_count"`
}

func toWatchVideos(vs []Video) []watchVideo {
	out := make([]watchVideo, 0, len(vs))
	for _, v := range vs {
		out = append(out, watchVideo{ID: v.ID, Name: v.Name, Resolution: v.Resolution, FileSize: v.FileSize})
	}
	return out
}

func toWatchSubtitles(subs []Subtitle) []watchSubtitle {
	out := make([]watchSubtitle, 0, len(subs))
	for _, sub := range subs {
		out = append(out, watchSubtitle{
			ID:            sub.ID,
			Language:      sub.Language,
			Name:          sub.Name,
			Provider:      sub.Provider,
			DownloadCount: sub.DownloadCount,
		})
	}
	return out
}

func jsonOrEmpty(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func (s *Server) handleWatchMovie(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.renderNotFound(w)
		return
	}
	title, err := s.store.GetTitle(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if title == nil {
		s.renderNotFound(w)
		return
	}
	videos, err := s.store.VideosForTitle(r.Context(), title.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	subs, err := s.store.SubtitlesForTitle(r.Context(), title.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := watchData{
		Heading:       title.Title,
		Sub:           yearOf(title.AirDate),
		BackHref:      fmt.Sprintf("/titles/%d", title.ID),
		StreamerURL:   s.streamerPublicURL,
		OwnerKind:     "title",
		OwnerID:       title.ID,
		VideosJSON:    jsonOrEmpty(toWatchVideos(videos)),
		SubtitlesJSON: jsonOrEmpty(toWatchSubtitles(subs)),
		HasVideo:      len(videos) > 0,
	}
	s.render(w, s.watch, "layout", data)
}

func (s *Server) handleWatchEpisode(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.renderNotFound(w)
		return
	}
	ec, err := s.store.GetEpisodeContext(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ec == nil {
		s.renderNotFound(w)
		return
	}
	videos, err := s.store.VideosForEpisode(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	subs, err := s.store.SubtitlesForEpisode(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sub := fmt.Sprintf("Phần %d · Tập %d", ec.SeasonNumber, ec.EpisodeNumber)
	if ec.EpisodeName != "" {
		sub += ": " + ec.EpisodeName
	}
	data := watchData{
		Heading:       ec.TitleName,
		Sub:           sub,
		BackHref:      fmt.Sprintf("/titles/%d", ec.TitleID),
		StreamerURL:   s.streamerPublicURL,
		OwnerKind:     "episode",
		OwnerID:       id,
		VideosJSON:    jsonOrEmpty(toWatchVideos(videos)),
		SubtitlesJSON: jsonOrEmpty(toWatchSubtitles(subs)),
		HasVideo:      len(videos) > 0,
	}
	s.render(w, s.watch, "layout", data)
}

// handlePrepareSource adds the chosen video's torrent to the streamer
// server-to-server and returns the info hash + file index for the browser to
// stream. This is the only path that reaches the streamer's add API.
func (s *Server) handlePrepareSource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "videoID"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid video id")
		return
	}
	video, err := s.store.GetVideo(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if video == nil {
		writeJSONError(w, http.StatusNotFound, "video not found")
		return
	}
	infoHash, err := s.streamer.addTorrent(r.Context(), video.Magnet)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"infoHash":  infoHash,
		"fileIndex": video.FileIndex,
	})
}

// handleSubtitleFile serves a saved subtitle file read-only from the shared blob
// storage (the admin owns the bytes; the viewer only reads them).
func (s *Server) handleSubtitleFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid subtitle id")
		return
	}
	sub, err := s.store.GetSubtitle(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sub == nil {
		writeJSONError(w, http.StatusNotFound, "subtitle not found")
		return
	}
	store := s.blobs[sub.StorageBackend]
	if store == nil {
		writeJSONError(w, http.StatusInternalServerError, "storage backend "+sub.StorageBackend+" not configured")
		return
	}
	data, err := store.Get(r.Context(), sub.StorageKey)
	if errors.Is(err, errBlobNotFound) {
		writeJSONError(w, http.StatusNotFound, "subtitle file not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", subtitleContentType(sub.Format))
	w.Write(data)
}

// subtitleContentType maps a stored subtitle format to a Content-Type. Defaults
// to WebVTT, which is what the admin stores.
func subtitleContentType(format string) string {
	if strings.EqualFold(format, "srt") {
		return "application/x-subrip; charset=utf-8"
	}
	return "text/vtt; charset=utf-8"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) renderNotFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	s.render(w, s.notFound, "layout", nil)
}

func (s *Server) render(w http.ResponseWriter, t *template.Template, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

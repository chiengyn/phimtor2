package main

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

const tmdbImageBase = "https://image.tmdb.org/t/p/"

// mockVideoURL is a placeholder stream used by the watch page until real
// playback is wired up.
const mockVideoURL = "https://commondatastorage.googleapis.com/gtv-videos-bucket/sample/BigBuckBunny.mp4"

type Server struct {
	store    *Store
	router   chi.Router
	home     *template.Template
	detail   *template.Template
	watch    *template.Template
	notFound *template.Template
	grid     *template.Template
}

func NewServer(store *Store) (*Server, error) {
	s := &Server{store: store}
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

	r.Get("/", s.handleHome)
	r.Get("/titles", s.handleTitlesFragment)
	r.Get("/titles/{id}", s.handleDetail)
	r.Get("/watch/movie/{id}", s.handleWatchMovie)
	r.Get("/watch/episode/{id}", s.handleWatchEpisode)

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

// watchData drives the (mocked) watch page.
type watchData struct {
	Heading  string
	Sub      string
	VideoURL string
	BackHref string
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
	data := watchData{
		Heading:  title.Title,
		Sub:      yearOf(title.AirDate),
		VideoURL: mockVideoURL,
		BackHref: fmt.Sprintf("/titles/%d", title.ID),
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
	sub := fmt.Sprintf("Phần %d · Tập %d", ec.SeasonNumber, ec.EpisodeNumber)
	if ec.EpisodeName != "" {
		sub += ": " + ec.EpisodeName
	}
	data := watchData{
		Heading:  ec.TitleName,
		Sub:      sub,
		VideoURL: mockVideoURL,
		BackHref: fmt.Sprintf("/titles/%d", ec.TitleID),
	}
	s.render(w, s.watch, "layout", data)
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

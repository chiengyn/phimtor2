package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	store  *Store
	tmdb   *TMDBClient
	user   string
	pass   string
	router chi.Router
}

func NewServer(store *Store, tmdb *TMDBClient, user, pass string) *Server {
	s := &Server{store: store, tmdb: tmdb, user: user, pass: pass}
	s.setupRouter()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func (s *Server) setupRouter() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(s.basicAuth) // protects the UI and the API

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "static/index.html")
	})

	r.Route("/api/titles", func(r chi.Router) {
		r.Get("/", s.handleListTitles)
		r.Get("/{id}", s.handleGetTitle)
		r.Delete("/{id}", s.handleDeleteTitle)
	})
	r.Post("/api/import", s.handleImport)

	s.router = r
}

// basicAuth gates every route behind a single admin user/password.
func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.pass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="phimtor2-admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type importRequest struct {
	Ref  string `json:"ref"`
	Type string `json:"type"`
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	mediaType, id, err := parseRef(req.Ref, req.Type)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	title, err := s.tmdb.FetchTitle(ctx, mediaType, id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "tmdb: "+err.Error())
		return
	}
	if err := s.store.UpsertTitle(ctx, title); err != nil {
		writeError(w, http.StatusInternalServerError, "store: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, title)
}

func (s *Server) handleListTitles(w http.ResponseWriter, r *http.Request) {
	titles, err := s.store.ListTitles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if titles == nil {
		titles = []TitleSummary{}
	}
	writeJSON(w, http.StatusOK, titles)
}

func (s *Server) handleGetTitle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	title, err := s.store.GetTitle(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if title == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, title)
}

func (s *Server) handleDeleteTitle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	ok, err := s.store.DeleteTitle(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

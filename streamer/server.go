package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	manager       *TorrentManager
	router        chi.Router
	internalToken string
}

func NewServer(manager *TorrentManager, internalToken string) *Server {
	s := &Server{manager: manager, internalToken: internalToken}
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
	r.Use(corsMiddleware)

	// Liveness probe for kamal-proxy / load balancers (doesn't touch the
	// torrent client or the filesystem).
	r.Get("/up", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	r.Route("/api/torrents", func(r chi.Router) {
		// Public data plane: anonymous browsers (admin watch page, viewer) hit
		// these directly for the owning streamer's stats + stream.
		r.Get("/{infoHash}/stats", s.handleTorrentStats)
		r.Get("/{infoHash}/files/{fileIndex}/stream", s.handleStream)

		// Internal control plane: only the manager calls these (server-side, with
		// the bearer token). They never come from a browser.
		r.Group(func(r chi.Router) {
			r.Use(s.internalAuth)
			r.Get("/", s.handleListTorrents)
			r.Post("/", s.handleAddTorrent)
			r.Get("/{infoHash}", s.handleGetTorrent)
			r.Delete("/{infoHash}", s.handleRemoveTorrent)
		})
	})

	s.router = r
}

// internalAuth gates the control-plane routes behind a shared bearer token. When
// the token is empty (single-streamer dev), the gate is a no-op so the whole API
// stays reachable as before.
func (s *Server) internalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.internalToken != "" && !bearerEquals(r.Header.Get("Authorization"), s.internalToken) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerEquals reports whether an Authorization header carries the given token,
// using a constant-time comparison so a valid token isn't leaked by timing.
func bearerEquals(header, token string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header[len(prefix):]), []byte(token)) == 1
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleListTorrents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.manager.ListTorrents())
}

// handleGetTorrent returns the file structure for a single torrent by infoHash.
// Callers that already hold the infoHash use this instead of GET /api/torrents,
// which would return (and force them to scan) every tracked torrent.
func (s *Server) handleGetTorrent(w http.ResponseWriter, r *http.Request) {
	infoHash := chi.URLParam(r, "infoHash")
	info, ok := s.manager.GetTorrentInfo(infoHash)
	if !ok {
		writeError(w, http.StatusNotFound, "torrent not found")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleTorrentStats(w http.ResponseWriter, r *http.Request) {
	infoHash := chi.URLParam(r, "infoHash")
	stats, ok := s.manager.GetStats(infoHash)
	if !ok {
		writeError(w, http.StatusNotFound, "torrent not found")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleAddTorrent(w http.ResponseWriter, r *http.Request) {
	var infoHash string
	var err error

	ct := r.Header.Get("Content-Type")

	switch {
	case ct == "application/json":
		var body struct {
			Magnet string `json:"magnet"`
		}
		if err = json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if body.Magnet == "" {
			writeError(w, http.StatusBadRequest, "magnet field required")
			return
		}
		infoHash, err = s.manager.AddMagnet(body.Magnet)

	default:
		// Try multipart form for .torrent file upload
		if err = r.ParseMultipartForm(32 << 20); err != nil {
			// Fall back to URL-encoded form
			r.ParseForm()
			magnet := r.FormValue("magnet")
			if magnet == "" {
				writeError(w, http.StatusBadRequest, "provide magnet or .torrent file")
				return
			}
			infoHash, err = s.manager.AddMagnet(magnet)
			break
		}
		file, _, ferr := r.FormFile("torrent")
		if ferr == nil {
			defer file.Close()
			infoHash, err = s.manager.AddTorrentFile(file)
		} else {
			magnet := r.FormValue("magnet")
			if magnet == "" {
				writeError(w, http.StatusBadRequest, "provide magnet or .torrent file")
				return
			}
			infoHash, err = s.manager.AddMagnet(magnet)
		}
	}

	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"infoHash": infoHash})
}

func (s *Server) handleRemoveTorrent(w http.ResponseWriter, r *http.Request) {
	infoHash := chi.URLParam(r, "infoHash")
	if err := s.manager.RemoveTorrent(infoHash); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	infoHash := chi.URLParam(r, "infoHash")
	fileIndexStr := chi.URLParam(r, "fileIndex")

	fileIndex, err := strconv.Atoi(fileIndexStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid file index")
		return
	}

	reader, fileInfo, err := s.manager.GetFileReader(infoHash, fileIndex)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer reader.Close()

	if needsTranscode(fileInfo.Path) {
		if err := transcodeStream(r.Context(), reader, w); err != nil {
			// Response may have already started; can't write error header
			return
		}
		return
	}

	w.Header().Set("Content-Type", detectContentType(fileInfo.Path))
	http.ServeContent(w, r, fileInfo.Path, time.Time{}, reader)
}

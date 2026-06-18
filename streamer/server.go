package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	manager *TorrentManager
	router  chi.Router
}

func NewServer(manager *TorrentManager) *Server {
	s := &Server{manager: manager}
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

	// The production watch UI lives in the admin service, which calls these
	// endpoints from the browser (hence CORS). This minimal built-in page is a
	// backend test harness only (torrents + streaming, no subtitles), served
	// from a cwd-relative path.
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "static/index.html")
	})

	r.Route("/api/torrents", func(r chi.Router) {
		r.Get("/", s.handleListTorrents)
		r.Post("/", s.handleAddTorrent)
		r.Delete("/{infoHash}", s.handleRemoveTorrent)
		r.Get("/{infoHash}/files/{fileIndex}/stream", s.handleStream)
	})

	s.router = r
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

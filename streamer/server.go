package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	manager *TorrentManager
	os      *OpenSubtitlesClient
	router  chi.Router
}

func NewServer(manager *TorrentManager, os *OpenSubtitlesClient) *Server {
	s := &Server{manager: manager, os: os}
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

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "static/index.html")
	})

	r.Route("/api/torrents", func(r chi.Router) {
		r.Get("/", s.handleListTorrents)
		r.Post("/", s.handleAddTorrent)
		r.Delete("/{infoHash}", s.handleRemoveTorrent)
		r.Get("/{infoHash}/files/{fileIndex}/stream", s.handleStream)
		r.Get("/{infoHash}/files/{fileIndex}/subtitles", s.handleSearchSubtitles)
	})

	r.Route("/api/subtitles", func(r chi.Router) {
		r.Get("/download", s.handleDownloadSubtitle)
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

// handleSearchSubtitles searches OpenSubtitles for a playing file. It derives a
// text query (and season/episode) from the file name, optionally overridden by
// ?query=, and adds a best-effort moviehash for more accurate matches.
func (s *Server) handleSearchSubtitles(w http.ResponseWriter, r *http.Request) {
	if !s.os.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "OpenSubtitles not configured (set OPENSUBTITLES_API_KEY)")
		return
	}

	infoHash := chi.URLParam(r, "infoHash")
	fileIndex, err := strconv.Atoi(chi.URLParam(r, "fileIndex"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid file index")
		return
	}

	fi, err := s.manager.GetFileInfo(infoHash, fileIndex)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	title, season, episode := parseSubtitleQuery(path.Base(fi.Path))
	if q := strings.TrimSpace(r.URL.Query().Get("query")); q != "" {
		title = q
	}
	langs := r.URL.Query().Get("languages")
	if langs == "" {
		langs = "en"
	}

	params := SearchParams{Query: title, Languages: langs, Season: season, Episode: episode}

	hashCtx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	if h, err := s.manager.MovieHash(hashCtx, infoHash, fileIndex); err == nil {
		params.MovieHash = h
	}

	subs, err := s.os.Search(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, subs)
}

// handleDownloadSubtitle resolves an OpenSubtitles file_id to WebVTT text that
// the player loads as a caption track.
func (s *Server) handleDownloadSubtitle(w http.ResponseWriter, r *http.Request) {
	if !s.os.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "OpenSubtitles not configured (set OPENSUBTITLES_API_KEY)")
		return
	}

	fileID, err := strconv.Atoi(r.URL.Query().Get("file_id"))
	if err != nil || fileID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid file_id")
		return
	}

	vtt, err := s.os.Download(r.Context(), fileID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Write([]byte(vtt))
}

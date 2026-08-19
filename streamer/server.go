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
	manager      *TorrentManager
	router       chi.Router
	controlToken string
}

func NewServer(manager *TorrentManager, controlToken string) *Server {
	s := &Server{manager: manager, controlToken: controlToken}
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
		r.Get("/{infoHash}/files/{fileIndex}/pieces", s.handleFilePieces)
		r.Get("/{infoHash}/files/{fileIndex}/stream", s.handleStream)

		// Internal control plane: only the manager calls these (server-side, with
		// the bearer token). They never come from a browser.
		r.Group(func(r chi.Router) {
			r.Use(s.internalAuth)
			r.Get("/", s.handleListTorrents)
			r.Post("/", s.handleAddTorrent)
			r.Get("/{infoHash}", s.handleGetTorrent)
			r.Get("/{infoHash}/metainfo", s.handleGetMetainfo)
			r.Delete("/{infoHash}", s.handleRemoveTorrent)
		})
	})

	// Internal load summary for the manager's bandwidth-aware placement. Like the
	// control plane it is gated by the streamer's own control token.
	r.Group(func(r chi.Router) {
		r.Use(s.internalAuth)
		r.Get("/api/load", s.handleLoad)
	})

	s.router = r
}

// handleLoad reports the instance-wide load (swarm transfer rates + torrent
// count) the manager polls to place new torrents by current bandwidth.
func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.manager.AggregateStats())
}

// internalAuth gates the control-plane routes behind the streamer's control
// token — its self-generated identity, which the manager presents on every
// internal call once the streamer is approved. Only the manager holds it.
func (s *Server) internalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.controlToken != "" && !bearerEquals(r.Header.Get("Authorization"), s.controlToken) {
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
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
		// The watch page reads the raw stream itself (client-side remux), so it needs
		// the response's size/range metadata. Cross-origin JS can only see headers
		// that are explicitly exposed — without this a 206 arrives with a
		// Content-Range the reader cannot read, and it can't determine the file size.
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
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

// handleGetMetainfo returns the torrent's resolved metainfo as raw bencoded
// .torrent bytes so the admin can persist it and backfill a magnet-only source.
// A 409 means the torrent's metadata hasn't been fetched from the swarm yet
// (there is nothing to bencode); a 404 means no such torrent is tracked.
func (s *Server) handleGetMetainfo(w http.ResponseWriter, r *http.Request) {
	infoHash := chi.URLParam(r, "infoHash")
	data, ok, hasInfo := s.manager.Metainfo(infoHash)
	if !ok {
		writeError(w, http.StatusNotFound, "torrent not found")
		return
	}
	if !hasInfo {
		writeError(w, http.StatusConflict, "torrent metadata not resolved yet")
		return
	}
	w.Header().Set("Content-Type", "application/x-bittorrent")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
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

// handleFilePieces returns the per-piece download map for one file (see
// FilePieces). Public data plane, like stats: the browser polls the owning
// streamer directly to render a piece map while the file downloads.
func (s *Server) handleFilePieces(w http.ResponseWriter, r *http.Request) {
	infoHash := chi.URLParam(r, "infoHash")
	fileIndex, err := strconv.Atoi(chi.URLParam(r, "fileIndex"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid file index")
		return
	}
	pieces, ok := s.manager.GetFilePieces(infoHash, fileIndex)
	if !ok {
		writeError(w, http.StatusNotFound, "torrent or file not found")
		return
	}
	writeJSON(w, http.StatusOK, pieces)
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

	// Meter every byte served to the viewer; this egress rate is the load signal
	// the manager balances new torrents on.
	cw := &countingResponseWriter{ResponseWriter: w, manager: s.manager}

	// raw=1 opts out of transcoding: the caller wants the container's own bytes,
	// range-seekable, because it remuxes them itself in the browser. Without it the
	// behaviour is unchanged, so any client that doesn't ask keeps the old path.
	raw := r.URL.Query().Get("raw") == "1"

	if !raw && needsTranscode(fileInfo.Path) {
		if err := transcodeStream(r.Context(), reader, cw); err != nil {
			// Response may have already started; can't write error header
			return
		}
		return
	}

	// Direct play: the browser seeks by issuing a Range request at the new offset.
	// Boost the seek target's priority so the post-seek buffer fills ahead of
	// background work (ServeContent then streams the range as usual).
	//
	// Only real playback seeks qualify: a bounded request for a sliver of bytes is
	// some client reading structure, not a viewer jumping, and PrioritizeSeek raises
	// pieces to High without ever lowering them again.
	//
	// Measured caveat: the watch page's demuxer (Mediabunny) issues *open-ended*
	// ranges even for its container probes — including reads near EOF for the
	// Matroska cues — so this filter does not catch those, and they do pin a window
	// High. That stays cheap in practice because the probe is a handful of requests
	// per file, and on a large file the cues land inside the already-pinned
	// SUFFIX_MB tail. Revisit only if stale High windows show up in piece maps.
	if start, size, ok := rangeStart(r.Header.Get("Range")); ok && (size < 0 || size >= seekRangeMinBytes) {
		s.manager.PrioritizeSeek(infoHash, fileIndex, start)
	}

	cw.Header().Set("Content-Type", detectContentType(fileInfo.Path))
	http.ServeContent(cw, r, fileInfo.Path, time.Time{}, reader)
}

// seekRangeMinBytes is the smallest bounded range treated as a playback seek
// rather than a demuxer metadata probe. A player buffering forward asks for far
// more than this; a container parser reading an index or a track header asks for
// far less.
const seekRangeMinBytes = 1 << 20 // 1 MiB

// rangeStart returns the start byte offset and the requested span of a single
// "bytes=<start>-[end]" Range header. The span is -1 for an open-ended range
// (the shape a media element uses to stream forward from a seek point). It
// returns false for an absent, multi-range, or suffix ("bytes=-N") header and for
// a start of 0 — so only real forward seeks trigger the extra prioritization, not
// the initial full-file request.
func rangeStart(header string) (start, size int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false
	}
	spec := header[len(prefix):]
	if strings.ContainsRune(spec, ',') {
		return 0, 0, false // multi-range: leave it to ServeContent
	}
	dash := strings.IndexByte(spec, '-')
	if dash <= 0 {
		return 0, 0, false // suffix range or malformed
	}
	n, err := strconv.ParseInt(spec[:dash], 10, 64)
	if err != nil || n <= 0 {
		return 0, 0, false
	}
	rest := spec[dash+1:]
	if rest == "" {
		return n, -1, true // open-ended: streaming forward from here
	}
	end, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || end < n {
		return n, -1, true // malformed end; treat as open-ended
	}
	return n, end - n + 1, true
}

// countingResponseWriter wraps an http.ResponseWriter to meter bytes written to a
// viewer, adding each Write to the manager's egress counter. It deliberately does
// not promote ReadFrom, so http.ServeContent's io.Copy falls back to Write and
// every byte is counted.
type countingResponseWriter struct {
	http.ResponseWriter
	manager *TorrentManager
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.manager.AddBytesServed(int64(n))
	}
	return n, err
}

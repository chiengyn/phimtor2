package main

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// maxAddBody bounds the in-memory copy of an add request (.torrent files are
// small; magnets are tiny).
const maxAddBody = 32 << 20

type Server struct {
	reg    *Registry
	router chi.Router
}

func NewServer(reg *Registry) *Server {
	s := &Server{reg: reg}
	s.setupRouter()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.router.ServeHTTP(w, r) }

func (s *Server) setupRouter() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Liveness: never fans out, so the manager reports up even when a streamer is
	// down.
	r.Get("/up", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// Self-registration plane: streamers authenticate with the register token.
	r.Group(func(r chi.Router) {
		r.Use(s.bearer(s.reg.cfg.RegisterToken))
		r.Post("/api/instances/register", s.handleRegister)
		r.Post("/api/instances/heartbeat", s.handleHeartbeat)
		r.Post("/api/instances/deregister", s.handleDeregister)
	})

	// Control plane: admin/viewer authenticate with the internal token.
	r.Group(func(r chi.Router) {
		r.Use(s.bearer(s.reg.cfg.InternalToken))
		r.Post("/api/torrents", s.handleAddTorrent)
		r.Get("/api/torrents", s.handleListTorrents)
		r.Get("/api/torrents/{infoHash}", s.handleGetTorrent)
		r.Delete("/api/torrents/{infoHash}", s.handleDeleteTorrent)
		// Instance-scoped add/list (the per-streamer watch page): force placement
		// on / list only this instance, bypassing load balancing.
		r.Post("/api/instances/{id}/torrents", s.handleAddToInstance)
		r.Get("/api/instances/{id}/torrents", s.handleListInstance)
		r.Get("/admin/instances", s.handleInstances)
	})

	s.router = r
}

// bearer gates a route group behind a shared token (constant-time compare). An
// empty token disables the gate (dev).
func (s *Server) bearer(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token != "" && !bearerEquals(r.Header.Get("Authorization"), token) {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerEquals(header, token string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header[len(prefix):]), []byte(token)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// --- registration handlers ---

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          string `json:"id"`
		InternalURL string `json:"internalURL"`
		PublicURL   string `json:"publicURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeError(w, http.StatusBadRequest, "id, internalURL, publicURL required")
		return
	}
	if body.InternalURL == "" || body.PublicURL == "" {
		writeError(w, http.StatusBadRequest, "internalURL and publicURL required")
		return
	}
	s.reg.Register(body.ID, body.InternalURL, body.PublicURL)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	if !s.reg.Heartbeat(body.ID) {
		// Unknown instance (manager restarted): tell the streamer to re-register.
		writeError(w, http.StatusNotFound, "unknown instance")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeregister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	s.reg.Deregister(body.ID)
	w.WriteHeader(http.StatusNoContent)
}

// --- control-plane handlers ---

func (s *Server) handleAddTorrent(w http.ResponseWriter, r *http.Request) {
	body, contentType, magnet, ok := readAddBody(w, r)
	if !ok {
		return
	}
	res, err := s.reg.placeAdd(r.Context(), contentType, body, magnet)
	s.writeAddResult(w, res, err)
}

// handleAddToInstance forces an add onto a specific streamer (per-streamer watch
// page), bypassing load balancing.
func (s *Server) handleAddToInstance(w http.ResponseWriter, r *http.Request) {
	body, contentType, magnet, ok := readAddBody(w, r)
	if !ok {
		return
	}
	res, err := s.reg.addToInstance(r.Context(), chi.URLParam(r, "id"), contentType, body, magnet)
	s.writeAddResult(w, res, err)
}

// readAddBody reads the add request body and sniffs the magnet (for dedupe)
// without consuming it. ok=false means it already wrote an error response.
func readAddBody(w http.ResponseWriter, r *http.Request) (body []byte, contentType, magnet string, ok bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAddBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body")
		return nil, "", "", false
	}
	contentType = r.Header.Get("Content-Type")
	if contentType == "application/json" {
		var jb struct {
			Magnet string `json:"magnet"`
		}
		if json.Unmarshal(body, &jb) == nil {
			magnet = jb.Magnet
		}
	}
	return body, contentType, magnet, true
}

func (s *Server) writeAddResult(w http.ResponseWriter, res map[string]string, err error) {
	if err != nil {
		switch err {
		case errNoInstance:
			writeError(w, http.StatusServiceUnavailable, err.Error())
		case errInstanceNotFound:
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) handleListTorrents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.reg.aggregateList(r.Context()))
}

// handleListInstance lists just one streamer's torrents (per-streamer watch page).
func (s *Server) handleListInstance(w http.ResponseWriter, r *http.Request) {
	in, ok := s.reg.instanceByID(chi.URLParam(r, "id"))
	if !ok {
		writeError(w, http.StatusNotFound, "streamer instance not found")
		return
	}
	torrents, err := s.reg.listTorrents(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, torrents)
}

func (s *Server) handleGetTorrent(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "infoHash")
	entry, ok := s.reg.getTorrent(r.Context(), hash)
	if !ok {
		writeError(w, http.StatusNotFound, "torrent not found")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleDeleteTorrent(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "infoHash")
	if err := s.reg.deleteTorrent(r.Context(), hash); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleInstances is the dashboard status API: each instance with health and its
// current torrents.
func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	type instanceStatus struct {
		ID          string         `json:"id"`
		InternalURL string         `json:"internalURL"`
		PublicURL   string         `json:"publicURL"`
		Healthy     bool           `json:"healthy"`
		Torrents    []torrentEntry `json:"torrents"`
	}
	out := []instanceStatus{}
	for _, in := range s.reg.allInstances() {
		st := instanceStatus{
			ID:          in.ID,
			InternalURL: in.InternalURL,
			PublicURL:   in.PublicURL,
			Healthy:     in.healthy(s.reg.ttl),
			Torrents:    []torrentEntry{},
		}
		if st.Healthy {
			if torrents, err := s.reg.listTorrents(r.Context(), in); err == nil {
				st.Torrents = torrents
			}
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, out)
}

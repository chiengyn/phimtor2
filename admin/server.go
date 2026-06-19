package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

//go:embed templates/*.html
var templatesFS embed.FS

// tmdbImageBase is the CDN prefix for the poster thumbnails the UI renders.
const tmdbImageBase = "https://image.tmdb.org/t/p/w200"

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	// imgURL builds a poster thumbnail URL (empty path -> empty src).
	"imgURL": func(path string) string {
		if path == "" {
			return ""
		}
		return tmdbImageBase + path
	},
	// year extracts the leading year from a "YYYY-MM-DD" air date.
	"year": func(airDate string) string {
		if len(airDate) >= 4 {
			return airDate[:4]
		}
		return "—"
	},
	// filesize renders a byte count as a human-readable size.
	"filesize": func(n int64) string {
		if n <= 0 {
			return "—"
		}
		const unit = 1024
		if n < unit {
			return fmt.Sprintf("%d B", n)
		}
		div, exp := int64(unit), 0
		for x := n / unit; x >= unit; x /= unit {
			div *= unit
			exp++
		}
		return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
	},
}).ParseFS(templatesFS, "templates/*.html"))

type Server struct {
	store       *Store
	tmdb        *TMDBClient
	os          *OpenSubtitlesClient
	streamerURL string
	user        string
	pass        string
	router      chi.Router
}

func NewServer(store *Store, tmdb *TMDBClient, osc *OpenSubtitlesClient, streamerURL, user, pass string) *Server {
	s := &Server{store: store, tmdb: tmdb, os: osc, streamerURL: streamerURL, user: user, pass: pass}
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

	r.Get("/", s.handleIndex)
	r.Get("/watch", s.handleWatch)
	r.Get("/titles/{id}", s.handleTitleDetail)
	r.Get("/titles/{id}/torrents/new", s.handleAddTorrentPage)

	r.Route("/api/titles", func(r chi.Router) {
		r.Get("/", s.handleListTitles)
		r.Get("/{id}", s.handleGetTitle)
		r.Get("/{id}/torrents", s.handleListTitleTorrents)
		r.Delete("/{id}", s.handleDeleteTitle)
	})
	r.Post("/api/import", s.handleImport)

	r.Route("/api/torrents", func(r chi.Router) {
		r.Post("/", s.handleSaveTorrent)
		r.Delete("/{id}", s.handleDeleteTorrent)
	})

	r.Route("/api/subtitles", func(r chi.Router) {
		r.Get("/search", s.handleSearchSubtitles)
		r.Get("/download", s.handleDownloadSubtitle)
	})

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

// handleIndex renders the full admin page with the current catalog so the list
// is present on first paint (htmx refreshes it afterwards via the
// "titlesChanged" event).
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	titles, err := s.store.ListTitles(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, "index.html", map[string]any{"Titles": titles})
}

// handleImport reads the form-encoded request from the htmx form, imports the
// title and returns the status message fragment. On success it also fires the
// "titlesChanged" event so the list re-fetches itself.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	mediaType, id, err := parseRef(r.FormValue("ref"), r.FormValue("type"))
	if err != nil {
		renderMsg(w, "err", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	title, err := s.tmdb.FetchTitle(ctx, mediaType, id)
	if err != nil {
		renderMsg(w, "err", "tmdb: "+err.Error())
		return
	}
	if err := s.store.UpsertTitle(ctx, title); err != nil {
		renderMsg(w, "err", "store: "+err.Error())
		return
	}

	msg := "Đã nhập: " + title.Title
	if title.Type == "tv" {
		msg += " (" + strconv.Itoa(len(title.Seasons)) + " mùa)"
	}
	w.Header().Set("HX-Trigger", "titlesChanged")
	renderMsg(w, "ok", msg)
}

// handleWatch renders the torrent watch page. The page talks to the streamer
// (StreamerURL) directly from the browser for torrents/streaming, and back to
// this admin server for OpenSubtitles search/download.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	render(w, "watch.html", map[string]any{
		"StreamerURL":      s.streamerURL,
		"SubtitlesEnabled": s.os.Enabled(),
	})
}

// handleSearchSubtitles searches OpenSubtitles for a playing file. It derives a
// text query (and season/episode) from the file name passed by the watch page,
// optionally overridden by ?query=. There is no moviehash here — the admin holds
// no torrent data.
func (s *Server) handleSearchSubtitles(w http.ResponseWriter, r *http.Request) {
	if !s.os.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "OpenSubtitles not configured (set OPENSUBTITLES_API_KEY)")
		return
	}

	title, season, episode := parseSubtitleQuery(path.Base(r.URL.Query().Get("file")))
	if q := strings.TrimSpace(r.URL.Query().Get("query")); q != "" {
		title = q
	}
	if strings.TrimSpace(title) == "" {
		writeError(w, http.StatusBadRequest, "provide a file or query to search")
		return
	}
	langs := r.URL.Query().Get("languages")
	if langs == "" {
		langs = "en"
	}

	subs, err := s.os.Search(r.Context(), SearchParams{Query: title, Languages: langs, Season: season, Episode: episode})
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
	_, _ = w.Write([]byte(vtt))
}

// handleListTitles renders the titles list fragment for htmx to swap into #list.
func (s *Server) handleListTitles(w http.ResponseWriter, r *http.Request) {
	titles, err := s.store.ListTitles(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, "list", titles)
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
	// Empty 200 (not 204, which htmx ignores) so the card's outerHTML swap
	// replaces it with nothing — i.e. removes the row.
	w.WriteHeader(http.StatusOK)
}

// handleTitleDetail renders the per-title detail page, which lists the torrents
// attached to the movie (or to each episode for TV) and links to the add flow.
func (s *Server) handleTitleDetail(w http.ResponseWriter, r *http.Request) {
	title := s.lookupTitle(w, r)
	if title == nil {
		return
	}
	render(w, "detail.html", map[string]any{"Title": title, "StreamerURL": s.streamerURL})
}

// handleListTitleTorrents renders the torrent-region fragment that the detail
// page re-fetches when the "torrentsChanged" event fires.
func (s *Server) handleListTitleTorrents(w http.ResponseWriter, r *http.Request) {
	title := s.lookupTitle(w, r)
	if title == nil {
		return
	}
	render(w, "torrent-region", title)
}

// handleAddTorrentPage renders the standalone add-torrent page. An optional
// ?episode_id= scopes the new torrent to a single TV episode; otherwise it is
// attached to the movie title.
func (s *Server) handleAddTorrentPage(w http.ResponseWriter, r *http.Request) {
	title := s.lookupTitle(w, r)
	if title == nil {
		return
	}

	episodeID := r.URL.Query().Get("episode_id")
	contextLabel := title.Title
	if episodeID != "" {
		if eid, err := strconv.ParseInt(episodeID, 10, 64); err == nil {
			for i := range title.Seasons {
				for j := range title.Seasons[i].Episodes {
					if ep := title.Seasons[i].Episodes[j]; ep.ID == eid {
						contextLabel = fmt.Sprintf("%s — S%02dE%02d %s",
							title.Title, title.Seasons[i].SeasonNumber, ep.EpisodeNumber, ep.Name)
					}
				}
			}
		}
	}

	render(w, "add_torrent.html", map[string]any{
		"Title":        title,
		"EpisodeID":    episodeID,
		"ContextLabel": contextLabel,
		"StreamerURL":  s.streamerURL,
	})
}

// handleSaveTorrent persists a torrent the admin previewed against the streamer.
// It accepts either JSON (magnet input) or multipart/form-data carrying the
// original .torrent file plus the metadata fields. The magnet is always stored;
// for file uploads with no magnet it is synthesized from the infohash so the
// torrent stays re-addable later.
func (s *Server) handleSaveTorrent(w http.ResponseWriter, r *http.Request) {
	var (
		t         Torrent
		fileBytes []byte
	)

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Magnet     string `json:"magnet"`
			InfoHash   string `json:"info_hash"`
			FileIndex  int    `json:"file_index"`
			FilePath   string `json:"file_path"`
			FileSize   int64  `json:"file_size"`
			Name       string `json:"name"`
			Resolution string `json:"resolution"`
			TitleID    *int64 `json:"title_id"`
			EpisodeID  *int64 `json:"episode_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		t = Torrent{
			Magnet: body.Magnet, InfoHash: body.InfoHash, FileIndex: body.FileIndex,
			FilePath: body.FilePath, FileSize: body.FileSize, Name: body.Name,
			Resolution: body.Resolution, TitleID: body.TitleID, EpisodeID: body.EpisodeID,
		}
	} else {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "invalid form: "+err.Error())
			return
		}
		t.Magnet = r.FormValue("magnet")
		t.InfoHash = r.FormValue("info_hash")
		t.FileIndex, _ = strconv.Atoi(r.FormValue("file_index"))
		t.FilePath = r.FormValue("file_path")
		t.FileSize, _ = strconv.ParseInt(r.FormValue("file_size"), 10, 64)
		t.Name = r.FormValue("name")
		t.Resolution = r.FormValue("resolution")
		t.TitleID = parseOptInt64(r.FormValue("title_id"))
		t.EpisodeID = parseOptInt64(r.FormValue("episode_id"))
		if file, _, ferr := r.FormFile("torrent"); ferr == nil {
			defer file.Close()
			b, err := io.ReadAll(file)
			if err != nil {
				writeError(w, http.StatusBadRequest, "read .torrent: "+err.Error())
				return
			}
			fileBytes = b
		}
	}

	t.Name = strings.TrimSpace(t.Name)
	t.InfoHash = strings.ToLower(strings.TrimSpace(t.InfoHash))
	switch {
	case t.Name == "":
		writeError(w, http.StatusBadRequest, "name required")
		return
	case !validResolution[t.Resolution]:
		writeError(w, http.StatusBadRequest, "invalid resolution")
		return
	case t.InfoHash == "":
		writeError(w, http.StatusBadRequest, "info_hash required")
		return
	case (t.TitleID == nil) == (t.EpisodeID == nil):
		writeError(w, http.StatusBadRequest, "provide exactly one of title_id or episode_id")
		return
	}
	if strings.TrimSpace(t.Magnet) == "" {
		t.Magnet = fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=%s", t.InfoHash, url.QueryEscape(t.Name))
	}

	if err := s.store.AddTorrent(r.Context(), &t, fileBytes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "torrentsChanged")
	writeJSON(w, http.StatusCreated, map[string]int64{"id": t.ID})
}

// handleDeleteTorrent removes a torrent row. Like handleDeleteTitle it returns
// an empty 200 (not 204) so the htmx outerHTML swap removes the row.
func (s *Server) handleDeleteTorrent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	ok, err := s.store.DeleteTorrent(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("HX-Trigger", "torrentsChanged")
	w.WriteHeader(http.StatusOK)
}

// lookupTitle parses the {id} URL param and loads the full title, writing the
// appropriate error response and returning nil on bad id / not found / error.
func (s *Server) lookupTitle(w http.ResponseWriter, r *http.Request) *Title {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return nil
	}
	title, err := s.store.GetTitle(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	if title == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	return title
}

// validResolution is the fixed set accepted by the torrents.resolution ENUM.
var validResolution = map[string]bool{"2160p": true, "1080p": true, "720p": true}

// parseOptInt64 returns a *int64 for a non-empty numeric form value, else nil.
func parseOptInt64(s string) *int64 {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return &v
}

// render executes a named template into the response as HTML.
func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// renderMsg writes the status-message fragment (id="msg") swapped into the page
// by the import form, with the given CSS class ("ok"/"err"). It always returns
// 200 because htmx skips swaps on error status codes — the "err" class, not the
// HTTP status, conveys a failed import to the UI.
func renderMsg(w http.ResponseWriter, kind, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "msg", map[string]string{"Kind": kind, "Text": text}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

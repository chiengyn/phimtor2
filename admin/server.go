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
	r.Get("/videos/{id}/play", s.handlePlayVideo)

	r.Route("/api/titles", func(r chi.Router) {
		r.Get("/", s.handleListTitles)
		r.Get("/{id}", s.handleGetTitle)
		r.Get("/{id}/videos", s.handleListTitleVideos)
		r.Delete("/{id}", s.handleDeleteTitle)
	})
	r.Post("/api/import", s.handleImport)

	r.Route("/api/videos", func(r chi.Router) {
		r.Post("/", s.handleSaveVideo)
		r.Post("/batch", s.handleSaveVideoBatch)
		r.Delete("/{id}", s.handleDeleteVideo)
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

// handleListTitleVideos renders the video-region fragment that the detail page
// re-fetches when the "torrentsChanged" event fires.
func (s *Server) handleListTitleVideos(w http.ResponseWriter, r *http.Request) {
	title := s.lookupTitle(w, r)
	if title == nil {
		return
	}
	render(w, "video-region", title)
}

// packEpisode is one selectable episode passed to the season-pack add page so the
// browser can map each video file to an episode.
type packEpisode struct {
	ID     int64  `json:"id"`
	Number int    `json:"number"`
	Label  string `json:"label"`
}

// handleAddTorrentPage renders the standalone add-torrent page. It runs in one of
// three modes depending on the query string:
//   - ?season_id= : season-pack mode — map many files in one .torrent to the
//     episodes of that season.
//   - ?episode_id= : scope a single video to one TV episode.
//   - neither     : attach a single video to the movie title.
func (s *Server) handleAddTorrentPage(w http.ResponseWriter, r *http.Request) {
	title := s.lookupTitle(w, r)
	if title == nil {
		return
	}

	episodeID := r.URL.Query().Get("episode_id")
	seasonID := r.URL.Query().Get("season_id")
	contextLabel := title.Title
	packEpisodes := []packEpisode{}

	switch {
	case seasonID != "":
		if sid, err := strconv.ParseInt(seasonID, 10, 64); err == nil {
			for i := range title.Seasons {
				se := title.Seasons[i]
				if se.ID != sid {
					continue
				}
				if se.Name != "" {
					contextLabel = fmt.Sprintf("%s — %s", title.Title, se.Name)
				} else {
					contextLabel = fmt.Sprintf("%s — Mùa %d", title.Title, se.SeasonNumber)
				}
				for _, ep := range se.Episodes {
					packEpisodes = append(packEpisodes, packEpisode{
						ID:     ep.ID,
						Number: ep.EpisodeNumber,
						Label:  fmt.Sprintf("S%02dE%02d · %s", se.SeasonNumber, ep.EpisodeNumber, ep.Name),
					})
				}
			}
		}
	case episodeID != "":
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

	episodesJSON, _ := json.Marshal(packEpisodes)

	render(w, "add_torrent.html", map[string]any{
		"Title":        title,
		"EpisodeID":    episodeID,
		"SeasonID":     seasonID,
		"ContextLabel": contextLabel,
		"StreamerURL":  s.streamerURL,
		// Plain string embedded in a data- attribute (NOT inside a <script>):
		// html/template JSON-string-encodes values in script context, which would
		// turn this array into a quoted string and break JSON.parse on the client.
		"EpisodesJSON": string(episodesJSON),
	})
}

// handlePlayVideo renders the standalone player page for one stored video. The
// page (plain JS, like the watch page) ensures the torrent is added to the
// streamer (POSTing the stored magnet — idempotent), streams the video's
// specific file by info_hash + file_index, and polls the streamer's per-torrent
// stats endpoint. Subtitles use the same admin-side OpenSubtitles proxy.
func (s *Server) handlePlayVideo(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	video, err := s.store.GetVideo(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if video == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Link back to the owning title's detail page when we can resolve it.
	backURL := "/"
	if video.TitleID != nil {
		backURL = fmt.Sprintf("/titles/%d", *video.TitleID)
	} else if video.EpisodeID != nil {
		if titleID, ok := s.store.TitleIDForEpisode(r.Context(), *video.EpisodeID); ok {
			backURL = fmt.Sprintf("/titles/%d", titleID)
		}
	}

	render(w, "play.html", map[string]any{
		"Video":            video,
		"StreamerURL":      s.streamerURL,
		"SubtitlesEnabled": s.os.Enabled(),
		"BackURL":          backURL,
	})
}

// handleSaveVideo persists one video the admin previewed against the streamer.
// It accepts either JSON (magnet input) or multipart/form-data carrying the
// original .torrent file plus the metadata fields. The magnet is always stored
// on the torrent source; for file uploads with no magnet it is synthesized from
// the infohash so the torrent stays re-addable later.
func (s *Server) handleSaveVideo(w http.ResponseWriter, r *http.Request) {
	var (
		v         Video
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
		v = Video{
			Magnet: body.Magnet, InfoHash: body.InfoHash, FileIndex: body.FileIndex,
			FilePath: body.FilePath, FileSize: body.FileSize, Name: body.Name,
			Resolution: body.Resolution, TitleID: body.TitleID, EpisodeID: body.EpisodeID,
		}
	} else {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "invalid form: "+err.Error())
			return
		}
		v.Magnet = r.FormValue("magnet")
		v.InfoHash = r.FormValue("info_hash")
		v.FileIndex, _ = strconv.Atoi(r.FormValue("file_index"))
		v.FilePath = r.FormValue("file_path")
		v.FileSize, _ = strconv.ParseInt(r.FormValue("file_size"), 10, 64)
		v.Name = r.FormValue("name")
		v.Resolution = r.FormValue("resolution")
		v.TitleID = parseOptInt64(r.FormValue("title_id"))
		v.EpisodeID = parseOptInt64(r.FormValue("episode_id"))
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

	v.Name = strings.TrimSpace(v.Name)
	v.InfoHash = strings.ToLower(strings.TrimSpace(v.InfoHash))
	switch {
	case v.Name == "":
		writeError(w, http.StatusBadRequest, "name required")
		return
	case !validResolution[v.Resolution]:
		writeError(w, http.StatusBadRequest, "invalid resolution")
		return
	case v.InfoHash == "":
		writeError(w, http.StatusBadRequest, "info_hash required")
		return
	case (v.TitleID == nil) == (v.EpisodeID == nil):
		writeError(w, http.StatusBadRequest, "provide exactly one of title_id or episode_id")
		return
	}
	if strings.TrimSpace(v.Magnet) == "" {
		v.Magnet = fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=%s", v.InfoHash, url.QueryEscape(v.Name))
	}

	if err := s.store.AddVideo(r.Context(), &v, fileBytes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "torrentsChanged")
	writeJSON(w, http.StatusCreated, map[string]int64{"id": v.ID})
}

// handleSaveVideoBatch persists a whole season pack: one .torrent (one info_hash)
// whose files map to several episodes. It accepts JSON (magnet) or multipart (the
// .torrent uploaded once) carrying the shared info_hash/magnet/resolution plus an
// items array, one entry per episode->file mapping.
func (s *Server) handleSaveVideoBatch(w http.ResponseWriter, r *http.Request) {
	type item struct {
		EpisodeID *int64 `json:"episode_id"`
		FileIndex int    `json:"file_index"`
		FilePath  string `json:"file_path"`
		FileSize  int64  `json:"file_size"`
		Name      string `json:"name"`
	}
	var (
		infoHash, magnet, resolution string
		items                        []item
		fileBytes                    []byte
	)

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			InfoHash   string `json:"info_hash"`
			Magnet     string `json:"magnet"`
			Resolution string `json:"resolution"`
			Items      []item `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		infoHash, magnet, resolution, items = body.InfoHash, body.Magnet, body.Resolution, body.Items
	} else {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "invalid form: "+err.Error())
			return
		}
		infoHash = r.FormValue("info_hash")
		magnet = r.FormValue("magnet")
		resolution = r.FormValue("resolution")
		if err := json.Unmarshal([]byte(r.FormValue("items")), &items); err != nil {
			writeError(w, http.StatusBadRequest, "invalid items: "+err.Error())
			return
		}
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

	infoHash = strings.ToLower(strings.TrimSpace(infoHash))
	switch {
	case !validResolution[resolution]:
		writeError(w, http.StatusBadRequest, "invalid resolution")
		return
	case infoHash == "":
		writeError(w, http.StatusBadRequest, "info_hash required")
		return
	case len(items) == 0:
		writeError(w, http.StatusBadRequest, "no episodes mapped")
		return
	}

	seenIndex := map[int]bool{}
	videos := make([]*Video, 0, len(items))
	for _, it := range items {
		if it.EpisodeID == nil {
			writeError(w, http.StatusBadRequest, "each item must reference an episode")
			return
		}
		if seenIndex[it.FileIndex] {
			writeError(w, http.StatusBadRequest, "duplicate file_index in pack")
			return
		}
		seenIndex[it.FileIndex] = true
		name := strings.TrimSpace(it.Name)
		if name == "" {
			name = strings.TrimSuffix(path.Base(it.FilePath), path.Ext(it.FilePath))
		}
		videos = append(videos, &Video{
			EpisodeID: it.EpisodeID, Name: name, Resolution: resolution,
			FileIndex: it.FileIndex, FilePath: it.FilePath, FileSize: it.FileSize,
		})
	}

	if strings.TrimSpace(magnet) == "" {
		magnet = fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
	}

	if err := s.store.AddVideoBatch(r.Context(), infoHash, magnet, fileBytes, videos); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "torrentsChanged")
	writeJSON(w, http.StatusCreated, map[string]int{"count": len(videos)})
}

// handleDeleteVideo removes a video row (reaping its torrent source if it was the
// last user). Like handleDeleteTitle it returns an empty 200 (not 204) so the
// htmx outerHTML swap removes the row.
func (s *Server) handleDeleteVideo(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	ok, err := s.store.DeleteVideo(r.Context(), id)
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

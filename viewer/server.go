package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
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

	// streamerPublicURL is injected into the watch page for the browser to reach
	// the streamer's stats + stream endpoints. streamer is the server-side client
	// the viewer uses to add torrents (the browser never adds directly). blobs
	// serves saved subtitle files read-only, keyed by storage backend name.
	streamerPublicURL string
	streamer          *streamerClient
	blobs             map[string]BlobStore

	// publicURL is the viewer's own browser-facing origin (no trailing slash),
	// used to build absolute canonical / Open Graph / sitemap URLs. Empty in
	// local dev, where SEO URLs fall back to site-relative.
	publicURL string

	// discordURL is the public invite link to the support Discord channel,
	// exposed to templates via the discordURL helper. Empty → link is hidden.
	discordURL string
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
		publicURL:         strings.TrimRight(cfg.PublicURL, "/"),
		discordURL:        cfg.DiscordURL,
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

// baseFuncMap holds the pure (request-independent) helpers the templates rely on
// for TMDB image URLs and date formatting. SEO helpers that need the configured
// public origin are added per-server in funcMap.
var baseFuncMap = template.FuncMap{
	// img builds a TMDB image URL for a size bucket (e.g. "w342"); empty paths
	// yield "" so templates can fall back to a placeholder.
	"img": tmdbImageURL,
	// year extracts the 4-digit year from a "YYYY-MM-DD" date string.
	"year": yearOf,
	// rating formats a vote average to one decimal place.
	"rating": func(v float64) string {
		return fmt.Sprintf("%.1f", v)
	},
	// truncate shortens text to at most n runes (word-aware), appending an
	// ellipsis — used to keep meta descriptions within search-snippet length.
	"truncate": truncate,
	// inc returns i+1, used to turn a 0-based range index into a 1-based rank
	// numeral on the Top 10 row.
	"inc": func(i int) int { return i + 1 },
}

// funcMap returns the template helpers for this server: the pure baseFuncMap
// plus SEO closures (abs / jsonLD / siteJSONLD) that need the public origin.
func (s *Server) funcMap() template.FuncMap {
	fm := template.FuncMap{
		"abs":        s.abs,
		"jsonLD":     s.titleJSONLD,
		"siteJSONLD": s.siteJSONLD,
		// discordURL exposes the configured support-channel invite link (empty
		// when unset, so templates can hide the link).
		"discordURL": func() string { return s.discordURL },
	}
	for k, v := range baseFuncMap {
		fm[k] = v
	}
	return fm
}

// tmdbImageURL builds a TMDB image URL for a size bucket (e.g. "w342"); empty
// paths yield "" so callers can fall back to a placeholder.
func tmdbImageURL(size, path string) string {
	if path == "" {
		return ""
	}
	return tmdbImageBase + size + path
}

// abs turns a site-relative path into an absolute URL using the configured
// public origin. With no origin configured (local dev) it returns the path
// unchanged, which is still valid for a same-document canonical link.
func (s *Server) abs(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return s.publicURL + path
}

// yearOf extracts the 4-digit year from a "YYYY-MM-DD" date string.
func yearOf(date string) string {
	if len(date) >= 4 {
		return date[:4]
	}
	return ""
}

// truncate shortens s to at most n runes, cutting at the last word boundary and
// appending an ellipsis. Whitespace is collapsed so multi-line overviews render
// as a clean single-line description.
func truncate(n int, s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)[:n]
	if i := strings.LastIndex(string(r), " "); i > 0 {
		r = []rune(string(r)[:i])
	}
	return strings.TrimRight(string(r), " ,.;:") + "…"
}

func (s *Server) parseTemplates() error {
	parse := func(files ...string) (*template.Template, error) {
		paths := make([]string, len(files))
		for i, f := range files {
			paths[i] = "templates/" + f
		}
		return template.New("").Funcs(s.funcMap()).ParseFiles(paths...)
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

	// Crawler endpoints.
	r.Get("/robots.txt", s.handleRobots)
	r.Get("/sitemap.xml", s.handleSitemap)

	r.Get("/", s.handleHome)
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

// gridPageSize is how many title cards one discovery-grid page holds.
const gridPageSize = 36

// filterFromQuery reads (and normalizes) the discovery filter from the request
// query params. An unrecognized type or non-numeric genre is dropped, so the
// filter always holds canonical values.
func filterFromQuery(r *http.Request) TitleFilter {
	q := r.URL.Query()
	f := TitleFilter{Query: strings.TrimSpace(q.Get("q"))}
	if t := q.Get("type"); t == "movie" || t == "tv" {
		f.Type = t
	}
	if id, err := strconv.Atoi(q.Get("genre")); err == nil && id > 0 {
		f.GenreID = id
	}
	return f
}

// active reports whether any constraint is set (so the home page shows the grid
// rather than the browse rows).
func (f TitleFilter) active() bool {
	return f.Query != "" || f.GenreID > 0 || f.Type != ""
}

// pageFromQuery reads the 1-based page number; anything missing or < 1 is page 1.
func pageFromQuery(r *http.Request) int {
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 1 {
		return p
	}
	return 1
}

// homeURL builds the canonical "/" browse URL for a filter + page: empty
// constraints and page 1 are omitted, and keys are encoded in a stable order so
// the same state always yields the same shareable URL. Used both for the page
// links and to canonicalize the address (see handleHome's redirect).
func homeURL(f TitleFilter, page int) string {
	v := url.Values{}
	if f.Query != "" {
		v.Set("q", f.Query)
	}
	if f.GenreID > 0 {
		v.Set("genre", strconv.Itoa(f.GenreID))
	}
	if f.Type != "" {
		v.Set("type", f.Type)
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

// gridPage is one page of discovery-grid cards plus its pagination controls.
type gridPage struct {
	Titles []TitleSummary
	Pager  pager
}

// loadGridPage fetches one page of filtered titles (clamping the requested page
// to the valid range) and builds its pagination controls; each page link is a
// full "/" navigation so the URL changes as the user pages.
func (s *Server) loadGridPage(r *http.Request, f TitleFilter, page int) (gridPage, error) {
	total, err := s.store.CountTitles(r.Context(), f)
	if err != nil {
		return gridPage{}, err
	}
	pages := totalPages(total, gridPageSize)
	page = clampPage(page, pages)
	titles, err := s.store.ListTitles(r.Context(), f, gridPageSize, (page-1)*gridPageSize)
	if err != nil {
		return gridPage{}, err
	}
	return gridPage{
		Titles: titles,
		Pager:  buildPager(page, pages, func(n int) template.URL { return template.URL(homeURL(f, n)) }),
	}, nil
}

// --- Pagination ------------------------------------------------------------

// pager is the set of numbered page controls rendered under a paginated list.
// PrevURL/NextURL are empty at the respective ends. Links is the windowed run of
// page numbers (a Gap entry marks an elided "…" stretch).
type pager struct {
	Page    int
	Pages   int
	PrevURL template.URL
	NextURL template.URL
	Links   []pagerLink
}

type pagerLink struct {
	Num     int
	URL     template.URL
	Current bool
	Gap     bool // an ellipsis placeholder, not a real page
}

// Show reports whether the pager is worth rendering (more than one page).
func (p pager) Show() bool { return p.Pages > 1 }

func totalPages(total, size int) int {
	pages := (total + size - 1) / size
	if pages < 1 {
		return 1
	}
	return pages
}

func clampPage(page, pages int) int {
	if page < 1 {
		return 1
	}
	if page > pages {
		return pages
	}
	return page
}

// pageWindow lists the page numbers to show around the current page, always
// including the first and last; a 0 marks an elided gap between runs.
func pageWindow(page, pages int) []int {
	const span = 2 // pages shown on each side of the current one
	keep := map[int]bool{1: true, pages: true, page: true}
	for i := 1; i <= span; i++ {
		if page-i >= 1 {
			keep[page-i] = true
		}
		if page+i <= pages {
			keep[page+i] = true
		}
	}
	var out []int
	prev := 0
	for n := 1; n <= pages; n++ {
		if !keep[n] {
			continue
		}
		if prev != 0 && n-prev > 1 {
			out = append(out, 0) // gap
		}
		out = append(out, n)
		prev = n
	}
	return out
}

// buildPager assembles the pager for the current page, using urlFor to build the
// link for each page number.
func buildPager(page, pages int, urlFor func(int) template.URL) pager {
	p := pager{Page: page, Pages: pages}
	if pages <= 1 {
		return p
	}
	if page > 1 {
		p.PrevURL = urlFor(page - 1)
	}
	if page < pages {
		p.NextURL = urlFor(page + 1)
	}
	for _, n := range pageWindow(page, pages) {
		if n == 0 {
			p.Links = append(p.Links, pagerLink{Gap: true})
			continue
		}
		p.Links = append(p.Links, pagerLink{Num: n, URL: urlFor(n), Current: n == page})
	}
	return p
}

type homeData struct {
	Rows     []Row    // browse view (no active filter)
	Featured []*Title // browse-view hero billboard carousel (top-ranked titles); empty when filtered or empty
	Grid     gridPage // flat results (filter active)
	Genres   []Genre
	Query    string
	GenreID  int
	Type     string
	Filtered bool
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	f := filterFromQuery(r)
	reqPage := pageFromQuery(r)
	filtered := f.active()

	// Canonicalize the address: the browse view never paginates (force page 1),
	// and the filter form submits empty/odd params (q=&genre=&type=, key order)
	// that homeURL strips. Redirect once to the clean URL so what the user sees,
	// shares and bookmarks is the canonical "/?..." for this state.
	wantPage := reqPage
	if !filtered {
		wantPage = 1
	}
	if want := homeURL(f, wantPage); want != r.URL.RequestURI() {
		http.Redirect(w, r, want, http.StatusFound)
		return
	}

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
		Filtered: filtered,
	}

	if filtered {
		if data.Grid, err = s.loadGridPage(r, f, reqPage); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// A page past the end was clamped; send the address to the real last page.
		if eff := data.Grid.Pager.Page; eff != reqPage {
			http.Redirect(w, r, homeURL(f, eff), http.StatusFound)
			return
		}
	} else {
		if data.Rows, err = s.store.ListRows(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Hero billboard carousel: the first row's top titles (the Top-10 picks
		// when present, else the newest). Reload each in full so the hero can show
		// its backdrop and overview, which the lightweight row summaries omit.
		if len(data.Rows) > 0 {
			const heroCount = 5
			for i, sum := range data.Rows[0].Titles {
				if i >= heroCount {
					break
				}
				if t, err := s.store.GetTitle(r.Context(), sum.ID); err == nil {
					data.Featured = append(data.Featured, t)
				}
			}
		}
	}
	s.render(w, s.home, "layout", data)
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

// lockedResolutions are video qualities currently reserved for future paid
// users: the viewer still lists them (so users can see the source exists) but
// refuses to play them. 4K (2160p) is gated for now — remove the entry here to
// re-enable playback. resolutionAvailable is the single source of truth used by
// both the watch page (chip + default selection) and the prepare endpoint.
var lockedResolutions = map[string]bool{"2160p": true}

func resolutionAvailable(res string) bool {
	return !lockedResolutions[res]
}

// watchVideo is the browser-facing subset of a Video (no magnet — the viewer
// adds it to the streamer server-side). Available is false for sources gated
// behind lockedResolutions, so the browser can show but not play them.
type watchVideo struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Resolution string `json:"resolution"`
	FileSize   int64  `json:"file_size"`
	Available  bool   `json:"available"`
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
		out = append(out, watchVideo{ID: v.ID, Name: v.Name, Resolution: v.Resolution, FileSize: v.FileSize, Available: resolutionAvailable(v.Resolution)})
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
	if !resolutionAvailable(video.Resolution) {
		writeJSONError(w, http.StatusForbidden, "nguồn này hiện không khả dụng")
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

// --- SEO: structured data, robots, sitemap --------------------------------

// baseURL returns the absolute origin to use for crawler output. It prefers the
// configured public origin and falls back to the request's scheme + host (so
// robots.txt / sitemap.xml still emit absolute URLs in local dev).
func (s *Server) baseURL(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// titleJSONLD builds schema.org Movie/TVSeries + BreadcrumbList structured data
// for a detail page. json.Marshal escapes <, >, & by default, so the result is
// safe to embed directly inside a <script> element.
func (s *Server) titleJSONLD(t *Title) template.JS {
	if t == nil {
		return ""
	}
	url := s.abs(fmt.Sprintf("/titles/%d", t.ID))

	work := map[string]any{
		"@context": "https://schema.org",
		"@type":    "Movie",
		"name":     t.Title,
		"url":      url,
	}
	if t.Type == "tv" {
		work["@type"] = "TVSeries"
		if n := len(t.Seasons); n > 0 {
			work["numberOfSeasons"] = n
		}
	}
	if t.OriginalTitle != "" && t.OriginalTitle != t.Title {
		work["alternateName"] = t.OriginalTitle
	}
	if t.Overview != "" {
		work["description"] = t.Overview
	}
	if img := tmdbImageURL("w780", t.PosterPath); img != "" {
		work["image"] = img
	}
	if len(t.AirDate) >= 10 {
		work["datePublished"] = t.AirDate[:10]
	}
	if len(t.Genres) > 0 {
		gs := make([]string, len(t.Genres))
		for i, g := range t.Genres {
			gs[i] = g.Name
		}
		work["genre"] = gs
	}
	if t.OriginalLanguage != "" {
		work["inLanguage"] = t.OriginalLanguage
	}
	if t.Type != "tv" && t.Runtime != nil && *t.Runtime > 0 {
		work["duration"] = fmt.Sprintf("PT%dM", *t.Runtime)
	}

	breadcrumb := map[string]any{
		"@context": "https://schema.org",
		"@type":    "BreadcrumbList",
		"itemListElement": []any{
			map[string]any{"@type": "ListItem", "position": 1, "name": "Trang chủ", "item": s.abs("/")},
			map[string]any{"@type": "ListItem", "position": 2, "name": t.Title, "item": url},
		},
	}

	b, err := json.Marshal([]any{work, breadcrumb})
	if err != nil {
		return ""
	}
	return template.JS(b)
}

// siteJSONLD builds the site-wide WebSite structured data, including a
// SearchAction so search engines can offer a sitelinks search box.
func (s *Server) siteJSONLD() template.JS {
	site := map[string]any{
		"@context": "https://schema.org",
		"@type":    "WebSite",
		"name":     "phimnet",
		"url":      s.abs("/"),
		"potentialAction": map[string]any{
			"@type":       "SearchAction",
			"target":      s.abs("/?q={search_term_string}"),
			"query-input": "required name=search_term_string",
		},
	}
	b, err := json.Marshal(site)
	if err != nil {
		return ""
	}
	return template.JS(b)
}

func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "User-agent: *\nAllow: /\nDisallow: /api/\n\nSitemap: %s/sitemap.xml\n", s.baseURL(r))
}

// handleSitemap emits an XML sitemap of the home page plus every title detail
// page, with each title's last-modified date so crawlers can prioritise updates.
func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.SitemapTitles(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	base := s.baseURL(r)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	b.WriteString("  <url><loc>" + base + "/</loc><changefreq>daily</changefreq><priority>1.0</priority></url>\n")
	for _, e := range entries {
		b.WriteString(fmt.Sprintf(
			"  <url><loc>%s/titles/%d</loc><lastmod>%s</lastmod><changefreq>weekly</changefreq></url>\n",
			base, e.ID, e.UpdatedAt.Format("2006-01-02")))
	}
	b.WriteString("</urlset>\n")

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
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

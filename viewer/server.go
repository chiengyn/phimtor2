package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

const tmdbImageBase = "https://image.tmdb.org/t/p/"

type Server struct {
	store     *Store
	router    chi.Router
	home      localizedTemplate
	detail    localizedTemplate
	watch     localizedTemplate
	bookmarks localizedTemplate
	notFound  localizedTemplate

	// manager is the server-side client the viewer uses to add torrents via the
	// streamer manager (the browser never adds directly). The manager returns the
	// owning streamer's public URL per prepare, which the browser then uses to
	// reach that streamer's stats + stream endpoints directly. blobs serves saved
	// subtitle files read-only, keyed by storage backend name.
	manager *managerClient
	blobs   map[string]BlobStore

	// watcher reference-counts active watch sessions so a torrent is dropped via the
	// manager the moment its last viewer leaves the watch page, instead of lingering
	// until the streamer's idle reaper. Driven by the watch page's heartbeat + leave
	// beacon; its sweeper runs from main via watcher.run.
	watcher *watchTracker

	// publicURL is the viewer's own browser-facing origin (no trailing slash),
	// used to build absolute canonical / Open Graph / sitemap URLs. Empty in
	// local dev, where SEO URLs fall back to site-relative.
	publicURL string

	// discordURL is the public invite link to the support Discord channel,
	// exposed to templates via the discordURL helper. Empty → link is hidden.
	discordURL string

	// google and sess implement accounts. google is nil-safe and reports
	// enabled() == false when GOOGLE_CLIENT_ID is unset, in which case the
	// /auth routes are never registered and the header shows no login button —
	// the site then behaves exactly as it did before accounts existed.
	google *googleClient
	sess   *sessionSigner

	// billing is the paid 4K tier. Like google it is nil-safe and reports
	// enabled() == false when no crypto rail is configured, in which case 4K
	// falls back to the "sắp ra mắt" tier the site shipped with.
	billing *billingService

	plans   localizedTemplate
	invoice localizedTemplate
}

type localizedTemplate map[Locale]*template.Template

func NewServer(store *Store, cfg Config) (*Server, error) {
	if err := validateMessages(); err != nil {
		return nil, err
	}
	blobs, err := newReadOnlyBlobStores(cfg)
	if err != nil {
		return nil, err
	}
	sess, ephemeral, err := newSessionSigner(cfg.SessionSecret, cfg.secureCookies())
	if err != nil {
		return nil, err
	}
	if ephemeral {
		log.Printf("SESSION_SECRET unset — using an ephemeral key, so sessions will not survive a restart (set one with: openssl rand -hex 32)")
	}

	s := &Server{
		store:      store,
		manager:    newManagerClient(cfg.ManagerInternalURL, cfg.ManagerInternalToken),
		blobs:      blobs,
		publicURL:  strings.TrimRight(cfg.PublicURL, "/"),
		discordURL: cfg.DiscordURL,
		sess:       sess,
	}
	if cfg.accountsEnabled() {
		s.google = newGoogleClient(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.oauthRedirectURL())
		log.Printf("Google sign-in enabled (redirect URI %s)", cfg.oauthRedirectURL())
	} else {
		log.Printf("Google sign-in disabled (GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET unset)")
	}
	// Left nil when unconfigured (billingService is nil-safe), which is what makes
	// resolutionLock fall back to the pre-billing "sắp ra mắt" 4K tier.
	if cfg.billingEnabled() {
		s.billing = newBillingService(store, cfg)
		log.Printf("Crypto billing enabled (chains: %s)", strings.Join(s.billing.chains(), ", "))
	} else {
		log.Printf("Crypto billing disabled — 4K stays locked for everyone (needs accounts plus BILLING_EVM_ADDRESS + BILLING_EVM_CHAINS, or BILLING_TRON_ADDRESS)")
	}
	s.watcher = newWatchTracker(time.Duration(cfg.WatchHeartbeatTTL)*time.Second, s.manager.deleteTorrent)
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
func (s *Server) funcMap(locale Locale) template.FuncMap {
	fm := template.FuncMap{
		"abs": s.abs,
		"js": func(value string) template.JS {
			encoded, _ := json.Marshal(value)
			return template.JS(encoded)
		},
		"jsonLD":     func(t *Title) template.JS { return s.titleJSONLD(locale, t) },
		"siteJSONLD": func() template.JS { return s.siteJSONLD(locale) },
		"t":          func(key string, args ...any) string { return tr(locale, key, args...) },
		"typeName": func(kind string) string {
			if kind == "tv" {
				return tr(locale, "type.tv")
			}
			return tr(locale, "type.movie")
		},
		// URL builders for slugged detail/watch links, so templates and
		// server-side SEO output produce identical "<slug>-<id>" paths.
		"homePath":       func() string { return localeHome(locale) },
		"pagePath":       func(path string) string { return localeURL(locale, path) },
		"titlePath":      func(id int64, name, original string) string { return titlePath(locale, id, name, original) },
		"watchMoviePath": func(id int64, name, original string) string { return watchMoviePath(locale, id, name, original) },
		"watchEpisodePath": func(id int64, epNum int, titleName string) string {
			return watchEpisodePath(locale, id, epNum, titleName)
		},
		"rowHref": func(row Row) template.URL { return template.URL(localeHome(locale) + "?" + row.Key) },
		// discordURL exposes the configured support-channel invite link (empty
		// when unset, so templates can hide the link).
		"discordURL": func() string { return s.discordURL },
		// billingOn tells the shared header whether to offer the upgrade entry.
		// A funcMap helper rather than a pageData field for the same reason
		// discordURL is one: it is request-independent chrome, so making it a
		// field would mean every handler had to remember to populate it.
		"billingOn": func() bool { return s.billing.enabled() },
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

// --- SEO slugs -------------------------------------------------------------

// nonSlugChar matches any run of characters that aren't ASCII lowercase letters
// or digits; slugify collapses each such run to a single hyphen.
var nonSlugChar = regexp.MustCompile(`[^a-z0-9]+`)

// diacriticFolder strips Unicode combining marks (accents) so Vietnamese/Latin
// letters fold to their base ASCII form: it decomposes (NFD), drops the marks,
// then recomposes (NFC). đ/Đ don't decompose, so slugify special-cases them.
var diacriticFolder = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// slugify turns a title into an SEO/URL-friendly ASCII slug, e.g.
// "Người Nhện: Trở Về Nhà" → "nguoi-nhen-tro-ve-nha". Vietnamese diacritics are
// folded to ASCII, everything is lowercased, and any run of non-alphanumeric
// characters becomes a single hyphen (leading/trailing hyphens trimmed). An
// unsluggable name (only punctuation/CJK) yields "".
func slugify(s string) string {
	s = strings.ReplaceAll(s, "đ", "d")
	s = strings.ReplaceAll(s, "Đ", "D")
	if folded, _, err := transform.String(diacriticFolder, s); err == nil {
		s = folded
	}
	s = strings.ToLower(s)
	s = nonSlugChar.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// titleSlug builds a title's slug from its localized name, falling back to the
// original (usually native/English) title when the localized name is empty.
func titleSlug(name, original string) string {
	if slug := slugify(name); slug != "" {
		return slug
	}
	return slugify(original)
}

// parseIDFromSlug extracts the numeric id trailing a "<slug>-<id>" path segment.
// The id is always appended as "-<id>", so it is the part after the final "-";
// a bare "<id>" (the legacy URL form) has no hyphen and parses whole.
func parseIDFromSlug(s string) (int64, error) {
	if i := strings.LastIndex(s, "-"); i >= 0 {
		s = s[i+1:]
	}
	return strconv.ParseInt(s, 10, 64)
}

// joinSlugID composes a "<prefix>/<slug>-<id>" path; when the slug is empty it
// drops the hyphen and emits "<prefix>/<id>" so the URL never starts a segment
// with a stray "-".
func joinSlugID(prefix, slug string, id int64) string {
	if slug == "" {
		return fmt.Sprintf("%s/%d", prefix, id)
	}
	return fmt.Sprintf("%s/%s-%d", prefix, slug, id)
}

// titlePath / watchMoviePath / watchEpisodePath build the canonical slugged URLs
// for a title's detail and watch pages. They are exposed to templates via
// funcMap so markup and server-side SEO output construct identical URLs.
func titlePath(locale Locale, id int64, name, original string) string {
	return joinSlugID(localeURL(locale, "/titles"), titleSlug(name, original), id)
}

func watchMoviePath(locale Locale, id int64, name, original string) string {
	return joinSlugID(localeURL(locale, "/watch/movie"), titleSlug(name, original), id)
}

func watchEpisodePath(locale Locale, id int64, epNum int, titleName string) string {
	slug := titleSlug(titleName, "")
	if slug != "" {
		slug = fmt.Sprintf("%s-tap-%d", slug, epNum)
	} else {
		slug = fmt.Sprintf("tap-%d", epNum)
	}
	return joinSlugID(localeURL(locale, "/watch/episode"), slug, id)
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
	parse := func(locale Locale, files ...string) (*template.Template, error) {
		paths := make([]string, len(files))
		for i, f := range files {
			paths[i] = "templates/" + f
		}
		return template.New("").Funcs(s.funcMap(locale)).ParseFiles(paths...)
	}

	s.home, s.detail, s.watch = localizedTemplate{}, localizedTemplate{}, localizedTemplate{}
	s.bookmarks, s.notFound = localizedTemplate{}, localizedTemplate{}
	s.plans, s.invoice = localizedTemplate{}, localizedTemplate{}
	for _, locale := range supportedLocales {
		var err error
		if s.home[locale], err = parse(locale, "layout.html", "home.html", "rows.html", "grid.html"); err != nil {
			return fmt.Errorf("parse %s home templates: %w", locale, err)
		}
		if s.detail[locale], err = parse(locale, "layout.html", "detail.html"); err != nil {
			return fmt.Errorf("parse %s detail templates: %w", locale, err)
		}
		if s.watch[locale], err = parse(locale, "layout.html", "watch.html"); err != nil {
			return fmt.Errorf("parse %s watch templates: %w", locale, err)
		}
		if s.bookmarks[locale], err = parse(locale, "layout.html", "bookmarks.html", "grid.html"); err != nil {
			return fmt.Errorf("parse %s bookmarks templates: %w", locale, err)
		}
		if s.notFound[locale], err = parse(locale, "layout.html", "404.html"); err != nil {
			return fmt.Errorf("parse %s not-found templates: %w", locale, err)
		}
		if s.plans[locale], err = parse(locale, "layout.html", "plans.html"); err != nil {
			return fmt.Errorf("parse %s plans templates: %w", locale, err)
		}
		if s.invoice[locale], err = parse(locale, "layout.html", "invoice.html"); err != nil {
			return fmt.Errorf("parse %s invoice templates: %w", locale, err)
		}
	}
	return nil
}

func (s *Server) setupRouter() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	// Applied globally so every page renders its own signed-in header state.
	// Anonymous requests carry no session cookie and return before touching the
	// database, so the public site pays nothing for this.
	r.Use(s.currentUser)

	// Liveness probe for kamal-proxy / load balancers (no DB access).
	r.Get("/up", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// Crawler endpoints.
	r.Get("/robots.txt", s.handleRobots)
	r.Get("/sitemap.xml", s.handleSitemap)

	// Every human-facing page has an explicit locale prefix. The neutral root is
	// the only language-negotiated URL; all other unprefixed page routes are
	// permanent Vietnamese compatibility redirects.
	r.Get("/", s.handleLocaleRoot)
	r.Get("/{locale}", s.handleLocaleSlash)
	r.Route("/{locale}", func(r chi.Router) {
		r.Use(s.requireLocale)
		r.Get("/", s.handleHome)
		r.Get("/titles/{id}", s.handleDetail)
		r.Get("/bookmarks", s.handleBookmarks)
		r.Get("/watch/movie/{id}", s.handleWatchMovie)
		r.Get("/watch/episode/{id}", s.handleWatchEpisode)
		if s.billing.enabled() {
			r.Get("/plans", s.handlePlansPage)
			r.Get("/payment/{ref}", s.handleInvoicePage)
		}
	})
	for _, pattern := range []string{"/titles/{id}", "/bookmarks", "/watch/movie/{id}", "/watch/episode/{id}"} {
		r.Get(pattern, s.redirectLegacyVietnamese)
	}
	if s.billing.enabled() {
		r.Get("/goi", s.redirectLegacyPlans)
		r.Get("/thanh-toan/{ref}", s.redirectLegacyInvoice)
	}

	// Viewer-mediated playback API (same-origin, called by the watch page JS).
	r.Post("/api/sources/{videoID}/prepare", s.handlePrepareSource)
	r.Get("/api/subtitles/{id}/file", s.handleSubtitleFile)
	r.Get("/api/catalog/cards", s.handleCatalogCards)
	// Watch-session liveness: the watch page heartbeats while playing and beacons
	// a leave on page hide, so the torrent is dropped the instant its last viewer
	// goes away (see watchtracker.go).
	// Accounts. The Google routes exist only when sign-in is configured, so an
	// unconfigured deploy 404s them instead of failing mid-flow. Logout is always
	// registered so an existing session can be cleared even if sign-in is later
	// turned off, and is POST-only so no prefetch can sign anyone out.
	if s.google.enabled() {
		r.Get("/auth/google/start", s.handleGoogleStart)
		r.Get("/auth/google/callback", s.handleGoogleCallback)
	}
	r.Post("/auth/logout", s.handleLogout)

	// Saved titles ("xem sau") for signed-in visitors. Anonymous visitors keep
	// their list in localStorage and never call these.
	r.Route("/api/bookmarks", func(r chi.Router) {
		r.Use(s.requireUser)
		r.Get("/", s.handleListSaved)
		r.Delete("/", s.handleClearSaved)
		r.Post("/merge", s.handleMergeSaved)
		r.Post("/{titleID}", s.handleAddSaved)
		r.Delete("/{titleID}", s.handleRemoveSaved)
	})

	r.Post("/api/watch/heartbeat", s.handleWatchHeartbeat)
	r.Post("/api/watch/leave", s.handleWatchLeave)

	// Billing APIs stay unprefixed; the page URL returned when creating an
	// invoice is localized from the request payload.
	if s.billing.enabled() {
		r.Route("/api/billing", func(r chi.Router) {
			r.Use(s.requireUser)
			r.Post("/invoices", s.handleCreateInvoice)
			r.Get("/invoices/{ref}", s.handleInvoiceStatus)
		})
	}

	fs := http.FileServer(http.Dir("static"))
	r.Handle("/static/*", http.StripPrefix("/static/", fs))
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		locale := localeFromRequestPath(r.URL.Path)
		ctx := context.WithValue(r.Context(), localeContextKey{}, locale)
		s.renderNotFound(w, r.WithContext(ctx))
	})

	s.router = r
}

func (s *Server) requireLocale(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		locale, ok := parseLocale(chi.URLParam(r, "locale"))
		if !ok || string(locale) != chi.URLParam(r, "locale") {
			http.NotFound(w, r)
			return
		}
		setLocaleCookie(w, r, locale)
		w.Header().Set("Content-Language", string(locale))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), localeContextKey{}, locale)))
	})
}

func (s *Server) handleLocaleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, localeHome(LocaleVI)+"?"+r.URL.RawQuery, http.StatusMovedPermanently)
		return
	}
	w.Header().Add("Vary", "Accept-Language")
	w.Header().Add("Vary", "Cookie")
	http.Redirect(w, r, localeHome(preferredLocale(r)), http.StatusFound)
}

func (s *Server) handleLocaleSlash(w http.ResponseWriter, r *http.Request) {
	locale, ok := parseLocale(chi.URLParam(r, "locale"))
	if !ok || string(locale) != chi.URLParam(r, "locale") {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, localeHome(locale), http.StatusMovedPermanently)
}

func (s *Server) redirectLegacyVietnamese(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, localizeLegacyURL(LocaleVI, r), http.StatusMovedPermanently)
}

func (s *Server) redirectLegacyPlans(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	http.Redirect(w, r, localeQueryURL(LocaleVI, "/plans", values), http.StatusMovedPermanently)
}

func (s *Server) redirectLegacyInvoice(w http.ResponseWriter, r *http.Request) {
	target := localeURL(LocaleVI, "/payment/"+url.PathEscape(chi.URLParam(r, "ref")))
	http.Redirect(w, r, target, http.StatusMovedPermanently)
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
	if sub := q.Get("subtitle"); sub == "vi" || sub == "en" {
		f.Subtitle = sub
	} else if q.Get("vietsub") == "1" {
		f.Subtitle = "vi"
	}
	return f
}

// active reports whether any constraint is set (so the home page shows the grid
// rather than the browse rows).
func (f TitleFilter) active() bool {
	return f.Query != "" || f.GenreID > 0 || f.Type != "" || f.Subtitle != ""
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
func homeURL(locale Locale, f TitleFilter, page int) string {
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
	if f.Subtitle != "" {
		v.Set("subtitle", f.Subtitle)
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	if len(v) == 0 {
		return localeHome(locale)
	}
	return localeHome(locale) + "?" + v.Encode()
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
	locale := localeFromContext(r.Context())
	total, err := s.store.CountTitles(r.Context(), locale, f)
	if err != nil {
		return gridPage{}, err
	}
	pages := totalPages(total, gridPageSize)
	page = clampPage(page, pages)
	titles, err := s.store.ListTitles(r.Context(), locale, f, gridPageSize, (page-1)*gridPageSize)
	if err != nil {
		return gridPage{}, err
	}
	return gridPage{
		Titles: titles,
		Pager:  buildPager(page, pages, func(n int) template.URL { return template.URL(homeURL(locale, f, n)) }),
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
	locale := localeFromContext(r.Context())
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
	if want := homeURL(locale, f, wantPage); want != r.URL.RequestURI() {
		http.Redirect(w, r, want, http.StatusFound)
		return
	}

	genres, err := s.store.ListGenres(r.Context(), locale)
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
			http.Redirect(w, r, homeURL(locale, f, eff), http.StatusFound)
			return
		}
	} else {
		if data.Rows, err = s.store.ListRows(r.Context(), locale); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Hero billboard carousel: the titles an admin hand-picked as "featured".
		// When none are marked, fall back to the first row's top titles (the Top-10
		// picks when present, else the newest) so the hero is never empty. Reload
		// each in full so the hero can show its backdrop and overview, which the
		// lightweight row summaries omit.
		const heroCount = 5
		heroIDs, err := s.store.FeaturedTitleIDs(r.Context(), heroCount)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(heroIDs) == 0 && len(data.Rows) > 0 {
			for i, sum := range data.Rows[0].Titles {
				if i >= heroCount {
					break
				}
				heroIDs = append(heroIDs, sum.ID)
			}
		}
		for _, id := range heroIDs {
			if t, err := s.store.GetTitle(r.Context(), locale, id); err == nil && t != nil {
				data.Featured = append(data.Featured, t)
			}
		}
	}
	s.render(w, r, s.home, data)
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	locale := localeFromContext(r.Context())
	id, err := parseIDFromSlug(chi.URLParam(r, "id"))
	if err != nil {
		s.renderNotFound(w, r)
		return
	}
	title, err := s.store.GetTitle(r.Context(), locale, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if title == nil {
		s.renderNotFound(w, r)
		return
	}
	if canonical := titlePath(locale, title.ID, title.Title, title.OriginalTitle); canonical != r.URL.Path {
		if r.URL.RawQuery != "" {
			canonical += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, canonical, http.StatusMovedPermanently)
		return
	}
	s.render(w, r, s.detail, title)
}

// bookmarksData drives the "xem sau" list page, which has two modes.
//
// Anonymous: LoggedIn is false and Titles is nil — the template renders an empty
// shell that static/bookmarks.js fills from localStorage, exactly as it did
// before accounts existed.
//
// Signed in: the list comes from the database, so it follows the user across
// devices, and the cards are rendered server-side by the same "card" partial the
// discovery grid uses — which means a saved entry can never show stale metadata.
type bookmarksData struct {
	LoggedIn bool
	Titles   []TitleSummary
}

type cardPayload struct {
	ID       int64   `json:"id"`
	Href     string  `json:"href"`
	Title    string  `json:"title"`
	Original string  `json:"original"`
	Poster   string  `json:"poster"`
	Year     string  `json:"year"`
	Type     string  `json:"type"`
	Score    float64 `json:"score"`
	Subtitle bool    `json:"subtitle"`
}

// handleCatalogCards refreshes anonymous localStorage bookmarks in the current
// language. Only public catalog metadata is returned; account bookmarks remain
// behind their existing authenticated API.
func (s *Server) handleCatalogCards(w http.ResponseWriter, r *http.Request) {
	locale, ok := parseLocale(r.URL.Query().Get("locale"))
	if !ok {
		locale = LocaleVI
	}
	rawIDs := strings.Split(r.URL.Query().Get("ids"), ",")
	ids := make([]int64, 0, len(rawIDs))
	seen := map[int64]bool{}
	for _, raw := range rawIDs {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err == nil && id > 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
		if len(ids) >= 500 {
			break
		}
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"cards": []cardPayload{}})
		return
	}
	titles, err := s.store.TitleCardsByIDs(r.Context(), locale, ids)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not load titles")
		return
	}
	cards := make([]cardPayload, 0, len(titles))
	for _, title := range titles {
		cards = append(cards, cardPayload{
			ID: title.ID, Href: titlePath(locale, title.ID, title.Title, title.OriginalTitle),
			Title: title.Title, Original: title.OriginalTitle,
			Poster: tmdbImageURL("w342", title.PosterPath), Year: yearOf(title.AirDate),
			Type: title.Type, Score: title.VoteAverage, Subtitle: title.HasSubtitle,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"cards": cards})
}

func (s *Server) handleBookmarks(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		s.render(w, r, s.bookmarks, bookmarksData{})
		return
	}
	titles, err := s.store.SavedTitles(r.Context(), localeFromContext(r.Context()), user.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, s.bookmarks, bookmarksData{LoggedIn: true, Titles: titles})
}

// --- Saved-titles API (signed-in visitors only) ------------------------------

// handleListSaved returns the ids the signed-in user has saved. bookmarks.js
// uses it to refresh its in-memory set after a merge.
func (s *Server) handleListSaved(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	ids := make([]int64, 0, len(user.SavedIDs))
	for id := range user.SavedIDs {
		ids = append(ids, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": ids})
}

// handleAddSaved saves a title. Idempotent, so a double-click is harmless.
func (s *Server) handleAddSaved(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "titleID"), 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid title id")
		return
	}
	if err := s.store.AddBookmark(r.Context(), user.ID, id); err != nil {
		log.Printf("bookmarks: add %d for user %d: %v", id, user.ID, err)
		writeJSONError(w, http.StatusInternalServerError, "could not save")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": true})
}

// handleRemoveSaved unsaves a title. Unsaving something never saved is a no-op.
func (s *Server) handleRemoveSaved(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "titleID"), 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid title id")
		return
	}
	if err := s.store.RemoveBookmark(r.Context(), user.ID, id); err != nil {
		log.Printf("bookmarks: remove %d for user %d: %v", id, user.ID, err)
		writeJSONError(w, http.StatusInternalServerError, "could not remove")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": false})
}

// handleClearSaved empties the signed-in user's saved list in one call, backing
// the "Xoá tất cả" button.
func (s *Server) handleClearSaved(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if err := s.store.ClearBookmarks(r.Context(), user.ID); err != nil {
		log.Printf("bookmarks: clear for user %d: %v", user.ID, err)
		writeJSONError(w, http.StatusInternalServerError, "could not clear")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": []int64{}})
}

// handleMergeSaved folds a browser's pre-login localStorage list into the
// account. bookmarks.js calls it on any page load where the visitor is signed in
// AND localStorage still holds entries, so it self-heals on every device without
// needing a "just logged in" marker. The merge is idempotent.
func (s *Server) handleMergeSaved(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := s.store.MergeBookmarks(r.Context(), user.ID, body.IDs); err != nil {
		log.Printf("bookmarks: merge for user %d: %v", user.ID, err)
		writeJSONError(w, http.StatusInternalServerError, "could not merge")
		return
	}
	ids, err := s.store.SavedTitleIDs(r.Context(), user.ID)
	if err != nil {
		log.Printf("bookmarks: reload for user %d: %v", user.ID, err)
		writeJSONError(w, http.StatusInternalServerError, "could not merge")
		return
	}
	out := make([]int64, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": out})
}

// watchData drives the watch page. Videos and subtitles are serialized to JSON
// strings injected via data-* attributes; the page JS adds the chosen source to
// the streamer (via the same-origin prepare endpoint) and streams it.
type watchData struct {
	Heading       string
	Sub           string
	BackHref      string
	OwnerKind     string // "title" | "episode"
	OwnerID       int64
	VideosJSON    string // JSON array, injected via a data-* attribute
	SubtitlesJSON string // JSON array, injected via a data-* attribute
	HasVideo      bool
	// LoginURL is the sign-in link back to this page, "" when the visitor is
	// already signed in or accounts are disabled. It must live here rather than
	// be read off the pageData envelope: layout.html invokes the page body as
	// {{block "content" .Data}}, so inside watch.html neither .User nor
	// $.LoginURL is in scope — the handler's view model is the only contract.
	LoginURL string
	// UpgradeURL is the "buy 4K" link for THIS title, "" when billing is off.
	// It lives here for the same reason LoginURL does, and it is built from the
	// title id rather than OwnerID because OwnerID is the episode id on a series
	// page while entitlements are per title.
	UpgradeURL string
	// Same-season episode navigation (episode watch page only; empty for movies).
	SeasonNumber int
	Episodes     []Episode
}

// lockedResolutions are video qualities reserved for the PAID tier: the viewer
// still lists them (so users can see the source exists), but playing one needs
// either an active time pass or a purchase of that specific title. With billing
// unconfigured nobody can play them at all, which is the pre-billing behaviour.
var lockedResolutions = map[string]bool{"2160p": true}

// memberResolutions are qualities reserved for signed-in ACCOUNTS. Anonymous
// visitors see the chip and are offered sign-in instead — this is the
// registration nudge, not a paywall. Everything not listed here (720p) stays
// freely playable, so the site is still usable without an account.
var memberResolutions = map[string]bool{"1080p": true}

// Why a source is not playable for this visitor. The empty string means it is.
// lockPaid and lockUpgrade are deliberately distinct: lockPaid is "nobody can
// play this yet" (billing unconfigured) and lockUpgrade is "you, specifically,
// could play this if you paid". They read as different copy and different HTTP
// statuses, so the client stays dumb about which is which.
const (
	lockNone    = ""
	lockPaid    = "paid"
	lockMember  = "member"
	lockUpgrade = "upgrade"
)

// titleAccess is the visitor's entitlement snapshot for ONE title. unlimited is
// every-4K access however it was obtained — a paid time pass OR an admin-granted
// comp — and unlocked is the permanent purchase of this title in particular.
// Either one is sufficient.
type titleAccess struct {
	signedIn  bool
	unlimited bool
	unlocked  bool
}

// resolutionLock reports why res is not playable for this visitor, or lockNone.
// It is the single source of truth used by both the watch page (chip + default
// selection) and the prepare endpoint.
//
// It is a method on *Server, not a bare function, because two tiers must switch
// OFF entirely when their feature is unconfigured:
//   - With no GOOGLE_CLIENT_ID there are no /auth/google/* routes and no login
//     button, so gating 1080p would strand every visitor at 720p with no way to
//     unlock it.
//   - With no crypto rail there is nothing to SELL, so 4K falls back to
//     "sắp ra mắt" rather than dangling an upgrade nobody can buy.
//
// Turning either feature's env vars off stays the clean rollback (see
// CLAUDE.md). Test both configs whenever you touch this.
//
// Note the order: an EXISTING entitlement is honoured before the billing-off
// check. That matters, and it was wrong until admin comps arrived — an admin
// grant has nothing to do with whether crypto billing is configured, and a
// permanent title unlock somebody already paid for must not evaporate the day
// the operator unsets the wallet env vars. Billing-off means "you cannot buy",
// never "what you hold is void".
func (s *Server) resolutionLock(res string, a titleAccess) string {
	switch {
	case lockedResolutions[res]:
		switch {
		case a.unlimited || a.unlocked:
			return lockNone // entitled, by whatever route
		case !s.billing.enabled():
			return lockPaid
		case !a.signedIn:
			// Entitlements hang off an account, so sign-in is step one of paying.
			return lockMember
		}
		return lockUpgrade
	case memberResolutions[res] && !a.signedIn && s.google.enabled():
		return lockMember
	}
	return lockNone
}

// hasLockedResolution reports whether any of these sources is paid-gated. It is
// the guard that keeps the entitlement lookup off the hot path: a page with only
// 720p/1080p sources never queries user_title_unlocks at all.
func hasLockedResolution(vs []Video) bool {
	for _, v := range vs {
		if lockedResolutions[v.Resolution] {
			return true
		}
	}
	return false
}

// accessForTitle builds the entitlement snapshot for one title. needsPaid says
// whether a paid-gated source is actually in play; when it is false (the common
// case) this costs nothing beyond reading the already-loaded user.
//
// It takes the request rather than a context so callers need no extra import,
// matching loginURL. A store error degrades to "not entitled" and is logged,
// the same way currentUser degrades to anonymous — a database blip must not
// hand out 4K, but it also must not 500 the whole watch page.
func (s *Server) accessForTitle(r *http.Request, titleID int64, needsPaid bool) titleAccess {
	u := userFrom(r.Context())
	a := titleAccess{signedIn: u != nil}
	// Deliberately NOT gated on s.billing.enabled(): a comp is granted by the
	// admin and an unlock may already have been bought, and neither depends on a
	// crypto rail being configured today.
	if u == nil || !needsPaid {
		return a
	}
	now := time.Now()
	if a.unlimited = u.HasPass(now) || u.HasComp(now); a.unlimited {
		return a // covers every title, so skip the per-title lookup
	}
	if titleID == 0 {
		return a
	}
	unlocked, err := s.store.HasTitleUnlock(r.Context(), u.ID, titleID)
	if err != nil {
		log.Printf("title unlock lookup failed (user %d, title %d): %v", u.ID, titleID, err)
		return a
	}
	a.unlocked = unlocked
	return a
}

// upgradeURL is the "buy 4K" link for a title, "" when billing is off (in which
// case no source is ever marked lockUpgrade, so nothing renders it).
func (s *Server) upgradeURL(locale Locale, titleID int64) string {
	if !s.billing.enabled() {
		return ""
	}
	return localeURL(locale, "/plans") + "?title=" + strconv.FormatInt(titleID, 10)
}

// watchVideo is the browser-facing subset of a Video (no magnet — the viewer
// adds it to the streamer server-side). Available is false for any gated
// source, so the browser can show but not play it; Lock says which gate, so the
// page can offer sign-in for a member source, an upgrade for a payable one, and
// "coming soon" for one nobody can play yet.
type watchVideo struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Resolution string `json:"resolution"`
	FileSize   int64  `json:"file_size"`
	Available  bool   `json:"available"`
	Lock       string `json:"lock"`
}

// watchSubtitle is the browser-facing subset of a Subtitle.
type watchSubtitle struct {
	ID            int64  `json:"id"`
	Language      string `json:"language"`
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	DownloadCount int    `json:"download_count"`
}

func (s *Server) toWatchVideos(vs []Video, a titleAccess) []watchVideo {
	out := make([]watchVideo, 0, len(vs))
	for _, v := range vs {
		lock := s.resolutionLock(v.Resolution, a)
		out = append(out, watchVideo{
			ID: v.ID, Name: v.Name, Resolution: v.Resolution, FileSize: v.FileSize,
			Available: lock == lockNone, Lock: lock,
		})
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
	locale := localeFromContext(r.Context())
	id, err := parseIDFromSlug(chi.URLParam(r, "id"))
	if err != nil {
		s.renderNotFound(w, r)
		return
	}
	title, err := s.store.GetTitle(r.Context(), locale, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if title == nil {
		s.renderNotFound(w, r)
		return
	}
	videos, err := s.store.VideosForTitle(r.Context(), title.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	access := s.accessForTitle(r, title.ID, hasLockedResolution(videos))
	subs, err := s.store.SubtitlesForTitle(r.Context(), title.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := watchData{
		Heading:       title.Title,
		Sub:           yearOf(title.AirDate),
		BackHref:      titlePath(locale, title.ID, title.Title, title.OriginalTitle),
		OwnerKind:     "title",
		OwnerID:       title.ID,
		VideosJSON:    jsonOrEmpty(s.toWatchVideos(videos, access)),
		SubtitlesJSON: jsonOrEmpty(toWatchSubtitles(subs)),
		HasVideo:      len(videos) > 0,
		LoginURL:      s.loginURL(r),
		UpgradeURL:    s.upgradeURL(locale, title.ID),
	}
	s.render(w, r, s.watch, data)
}

func (s *Server) handleWatchEpisode(w http.ResponseWriter, r *http.Request) {
	locale := localeFromContext(r.Context())
	id, err := parseIDFromSlug(chi.URLParam(r, "id"))
	if err != nil {
		s.renderNotFound(w, r)
		return
	}
	ec, err := s.store.GetEpisodeContext(r.Context(), locale, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ec == nil {
		s.renderNotFound(w, r)
		return
	}
	videos, err := s.store.VideosForEpisode(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	access := s.accessForTitle(r, ec.TitleID, hasLockedResolution(videos))
	subs, err := s.store.SubtitlesForEpisode(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	eps, err := s.store.EpisodesInSeasonOf(r.Context(), locale, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sub := tr(locale, "watch.episode_sub", ec.SeasonNumber, ec.EpisodeNumber)
	if ec.EpisodeName != "" {
		sub += ": " + ec.EpisodeName
	}
	data := watchData{
		Heading:       ec.TitleName,
		Sub:           sub,
		BackHref:      titlePath(locale, ec.TitleID, ec.TitleName, ""),
		OwnerKind:     "episode",
		OwnerID:       id,
		VideosJSON:    jsonOrEmpty(s.toWatchVideos(videos, access)),
		SubtitlesJSON: jsonOrEmpty(toWatchSubtitles(subs)),
		HasVideo:      len(videos) > 0,
		LoginURL:      s.loginURL(r),
		UpgradeURL:    s.upgradeURL(locale, ec.TitleID),
		SeasonNumber:  ec.SeasonNumber,
		Episodes:      eps,
	}
	s.render(w, r, s.watch, data)
}

// handlePrepareSource adds the chosen video's torrent to the streamer
// server-to-server and returns the info hash + file index for the browser to
// stream. This is the only path that reaches the streamer's add API.
func (s *Server) handlePrepareSource(w http.ResponseWriter, r *http.Request) {
	locale, ok := parseLocale(r.Header.Get("X-Phimnet-Locale"))
	if !ok {
		locale = LocaleVI
	}
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
	// Entitlements are per TITLE, but this endpoint is handed only a video id, and
	// videos.title_id is NULL for an episode — so resolve the owning title first.
	titleID, err := s.store.TitleIDForVideo(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The chips are rendered client-side and trivially bypassable, so this is the
	// check that actually enforces the tiers. The three statuses are what let the
	// page tell "sign in and you can" from "pay and you can" from "nobody can yet".
	access := s.accessForTitle(r, titleID, lockedResolutions[video.Resolution])
	switch s.resolutionLock(video.Resolution, access) {
	case lockPaid:
		writeJSONError(w, http.StatusForbidden, tr(locale, "watch.source_unavailable"))
		return
	case lockUpgrade:
		writeJSONError(w, http.StatusPaymentRequired, tr(locale, "watch.upgrade_quality", video.Resolution))
		return
	case lockMember:
		writeJSONError(w, http.StatusUnauthorized, tr(locale, "watch.sign_in_quality", video.Resolution))
		return
	}
	infoHash, streamerPublicURL, err := s.manager.addTorrent(r.Context(), video.Magnet, video.TorrentFile)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"infoHash":          infoHash,
		"fileIndex":         video.FileIndex,
		"streamerPublicURL": streamerPublicURL,
	})
}

// handleWatchHeartbeat records that a watch session is still actively watching
// its torrent. The watch page calls it on a short interval; once the heartbeats
// stop (and no leave beacon arrived), the tracker's sweep drops the torrent.
func (s *Server) handleWatchHeartbeat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InfoHash  string `json:"infoHash"`
		SessionID string `json:"sessionID"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	s.watcher.beat(body.SessionID, body.InfoHash)
	w.WriteHeader(http.StatusNoContent)
}

// handleWatchLeave ends a watch session immediately — the watch page beacons it
// on page hide (tab close or navigating to another title) — so the torrent is
// dropped at once if this was its last viewer.
func (s *Server) handleWatchLeave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"sessionID"`
	}
	// Best-effort: a malformed/empty beacon body just leaves the session for the
	// sweep to reap, so a decode error is not worth a 400.
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body)
	s.watcher.leave(body.SessionID)
	w.WriteHeader(http.StatusNoContent)
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
	w.Write(stripASSOverrides(data))
}

// assOverrideRe matches an ASS/SSA override block — a brace group beginning with
// a backslash, e.g. {\an8} (position top-center), {\i1}, {\pos(...)}. These are
// styling/positioning directives that some stored .srt/.vtt files carry inline;
// WebVTT has no notion of them, so the browser renders them as literal cue text.
var assOverrideRe = regexp.MustCompile(`\{\\[^}]*\}`)

// stripASSOverrides removes inline ASS/SSA override tags from subtitle bytes so
// they don't show up as on-screen garbage like "{\an8}". It only touches brace
// groups that start with a backslash, leaving ordinary text (and stray braces)
// untouched.
func stripASSOverrides(b []byte) []byte {
	if !bytesContainsBraceBackslash(b) {
		return b // fast path: nothing to strip, avoid a needless allocation
	}
	return assOverrideRe.ReplaceAll(b, nil)
}

// bytesContainsBraceBackslash reports whether b contains the "{\" that starts an
// ASS override block, so the common (clean) subtitle skips the regex entirely.
func bytesContainsBraceBackslash(b []byte) bool {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == '{' && b[i+1] == '\\' {
			return true
		}
	}
	return false
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
func (s *Server) titleJSONLD(locale Locale, t *Title) template.JS {
	if t == nil {
		return ""
	}
	url := s.abs(titlePath(locale, t.ID, t.Title, t.OriginalTitle))

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
			map[string]any{"@type": "ListItem", "position": 1, "name": tr(locale, "nav.home"), "item": s.abs(localeHome(locale))},
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
func (s *Server) siteJSONLD(locale Locale) template.JS {
	site := map[string]any{
		"@context": "https://schema.org",
		"@type":    "WebSite",
		"name":     "phimnet",
		"url":      s.abs(localeHome(locale)),
		"potentialAction": map[string]any{
			"@type":       "SearchAction",
			"target":      s.abs(localeHome(locale) + "?q={search_term_string}"),
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
	base := s.baseURL(r)
	localized := make(map[Locale][]SitemapEntry, len(supportedLocales))
	byID := make(map[Locale]map[int64]SitemapEntry, len(supportedLocales))
	for _, locale := range supportedLocales {
		entries, err := s.store.SitemapTitles(r.Context(), locale)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		localized[locale] = entries
		byID[locale] = make(map[int64]SitemapEntry, len(entries))
		for _, entry := range entries {
			byID[locale][entry.ID] = entry
		}
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">` + "\n")
	for _, locale := range supportedLocales {
		b.WriteString(fmt.Sprintf("  <url><loc>%s%s</loc>", base, localeHome(locale)))
		for _, alternate := range supportedLocales {
			b.WriteString(fmt.Sprintf(`<xhtml:link rel="alternate" hreflang="%s" href="%s%s"/>`, alternate, base, localeHome(alternate)))
		}
		b.WriteString(fmt.Sprintf(`<xhtml:link rel="alternate" hreflang="x-default" href="%s/"/><changefreq>daily</changefreq><priority>1.0</priority></url>`+"\n", base))
		for _, entry := range localized[locale] {
			b.WriteString(fmt.Sprintf("  <url><loc>%s%s</loc>", base, titlePath(locale, entry.ID, entry.Title, entry.OriginalTitle)))
			for _, alternate := range supportedLocales {
				other, ok := byID[alternate][entry.ID]
				if ok {
					b.WriteString(fmt.Sprintf(`<xhtml:link rel="alternate" hreflang="%s" href="%s%s"/>`, alternate, base, titlePath(alternate, other.ID, other.Title, other.OriginalTitle)))
				}
			}
			b.WriteString(fmt.Sprintf("<lastmod>%s</lastmod><changefreq>weekly</changefreq></url>\n", entry.UpdatedAt.Format("2006-01-02")))
		}
	}
	b.WriteString("</urlset>\n")

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// pageData is the root value every page template is executed with. Data is the
// handler's own view model — layout.html re-scopes the head_title / head_extra /
// content blocks to it, so no page template had to change — while User and
// LoginURL drive the shared header chrome and SavedIDsJSON lets bookmarks.js
// mark the save buttons without a round trip.
type pageData struct {
	Data               any
	Locale             Locale
	HomeURL            string
	AlternateURL       string
	AlternateLocale    Locale
	ClientMessagesJSON string
	// User is nil for an anonymous visitor, which is the common case.
	User *User
	// LoginURL is the sign-in link for THIS page, so a visitor comes back to
	// where they were. Empty when accounts are disabled, which hides the button.
	LoginURL string
	// SavedIDsJSON is the signed-in user's saved title ids as a JSON array, "[]"
	// when anonymous. Rendered onto <body data-bm-ids>.
	SavedIDsJSON string
	// Path is the current request's path+query, so the logout form returns the
	// visitor to where they were.
	Path string
}

// newPageData wraps a handler's view model with the shared per-request chrome.
func (s *Server) newPageData(r *http.Request, data any) pageData {
	locale := localeFromContext(r.Context())
	clientMessages := map[string]string{
		"bookmarkSave":         tr(locale, "bookmark.save"),
		"bookmarkSaved":        tr(locale, "bookmark.saved"),
		"bookmarkRemove":       tr(locale, "bookmark.remove"),
		"bookmarkClearConfirm": tr(locale, "bookmarks.clear_confirm"),
		"typeMovie":            tr(locale, "type.movie"),
		"typeTV":               tr(locale, "type.tv"),
		"subtitleBadge":        tr(locale, "card.subtitle"),
	}
	pd := pageData{
		Data:               data,
		Locale:             locale,
		HomeURL:            localeHome(locale),
		AlternateURL:       alternateLocaleURL(r),
		AlternateLocale:    fallbackLocale(locale),
		ClientMessagesJSON: jsonOrEmpty(clientMessages),
		User:               userFrom(r.Context()),
		SavedIDsJSON:       "[]",
		Path:               r.URL.RequestURI(),
	}
	// Detail pages use locale-specific slugs. Resolve the other translation so
	// hreflang and the language switcher point directly at its canonical URL,
	// rather than relying on a redirect from the current language's slug.
	if title, ok := data.(*Title); ok && title != nil && s.store != nil {
		alternate := fallbackLocale(locale)
		if translated, err := s.store.GetTitle(r.Context(), alternate, title.ID); err == nil && translated != nil {
			pd.AlternateURL = titlePath(alternate, translated.ID, translated.Title, translated.OriginalTitle)
		}
	}
	if pd.User != nil {
		ids := make([]int64, 0, len(pd.User.SavedIDs))
		for id := range pd.User.SavedIDs {
			ids = append(ids, id)
		}
		pd.SavedIDsJSON = jsonOrEmpty(ids)
	}
	pd.LoginURL = s.loginURL(r)
	return pd
}

// loginURL is the sign-in link that returns the visitor to this exact page, or
// "" when they are already signed in or accounts are disabled (which is what
// hides every login affordance on the site).
func (s *Server) loginURL(r *http.Request) string {
	if userFrom(r.Context()) != nil || !s.google.enabled() {
		return ""
	}
	return "/auth/google/start?next=" + url.QueryEscape(r.URL.RequestURI())
}

func (s *Server) renderNotFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	s.render(w, r, s.notFound, nil)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, templates localizedTemplate, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	locale := localeFromContext(r.Context())
	w.Header().Set("Content-Language", string(locale))
	t := templates[locale]
	if t == nil {
		t = templates[LocaleVI]
	}
	if err := t.ExecuteTemplate(w, "layout", s.newPageData(r, data)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

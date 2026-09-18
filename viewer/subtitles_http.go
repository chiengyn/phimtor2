package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The signed-in subtitle-contribution API. Three endpoints, all behind
// requireUser (GET /api/subtitles/{id}/file stays public — reading a saved
// subtitle needs no account, only contributing one does):
//
//	GET  /api/subtitles/search    find candidates at a provider
//	GET  /api/subtitles/download  fetch one as WebVTT, for THIS playback only
//	POST /api/subtitles           save one to the shared catalog, for everyone
//
// The split between the last two is the whole design: applying a subtitle writes
// nothing, so the expensive, permanent, everyone-sees-it action stays an
// explicit second click.

// maxSubtitleSaveBody bounds the save request body. The payload is a handful of
// short strings; anything larger is a client bug or an attack.
const maxSubtitleSaveBody = 1 << 16

// subtitleKeyUnsafe and subtitleStorageKey are ported verbatim from admin so
// both services produce identical key layouts in the SHARED blob store.
var subtitleKeyUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// subtitleStorageKey builds a unique BlobStore key for a saved subtitle, grouped
// by owner. The nanosecond suffix keeps re-saves of the same provider/file from
// sharing one key — and, across services, keeps a viewer save from ever
// clobbering an admin one.
func subtitleStorageKey(titleID, episodeID *int64, provider, fileID, lang, format string) string {
	owner := "misc"
	if titleID != nil {
		owner = fmt.Sprintf("title-%d", *titleID)
	} else if episodeID != nil {
		owner = fmt.Sprintf("episode-%d", *episodeID)
	}
	clean := func(s string) string { return subtitleKeyUnsafe.ReplaceAllString(s, "-") }
	file := fmt.Sprintf("%s-%s-%s-%d.%s", clean(provider), clean(fileID), clean(lang), time.Now().UnixNano(), clean(format))
	return "subtitles/" + owner + "/" + file
}

// apiLocale resolves the locale for a JSON endpoint's user-facing messages. The
// watch page sends X-Phimnet-Locale on every API call (same as the prepare
// endpoint); anything unparseable falls back to Vietnamese.
func apiLocale(r *http.Request) Locale {
	if locale, ok := parseLocale(r.Header.Get("X-Phimnet-Locale")); ok {
		return locale
	}
	return LocaleVI
}

// handleSearchSubtitles proxies a provider search. The API keys never reach the
// browser, and the provider does not allow authenticated cross-origin calls
// anyway, so every query is funneled through here.
func (s *Server) handleSearchSubtitles(w http.ResponseWriter, r *http.Request) {
	locale := apiLocale(r)
	user := userFrom(r.Context())

	if !s.subtitles.search.allow(rateKey(user)) {
		writeJSONError(w, http.StatusTooManyRequests, tr(locale, "watch.subtitle_rate_limited"))
		return
	}

	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("query"))
	if query == "" {
		writeJSONError(w, http.StatusBadRequest, tr(locale, "watch.subtitle_query_required"))
		return
	}
	provider := s.subtitles.provider(q.Get("provider"))
	if provider == nil {
		writeJSONError(w, http.StatusServiceUnavailable, tr(locale, "watch.subtitle_search_unavailable"))
		return
	}

	languages := strings.TrimSpace(q.Get("languages"))
	if languages == "" {
		// Vietnamese by default, unlike admin's "en" — this audience is the
		// reason the feature exists.
		languages = "vi"
	}
	results, err := provider.Search(r.Context(), SearchParams{
		Query:     query,
		Languages: languages,
		Season:    atoiDefault(q.Get("season"), 0),
		Episode:   atoiDefault(q.Get("episode"), 0),
	})
	if err != nil {
		// Deliberately NOT err.Error(): admin can surface upstream text because it
		// sits behind basic auth, but this endpoint is public-facing and provider
		// errors can echo the shared account's quota/billing state.
		log.Printf("subtitles: search via %s: %v", provider.Name(), err)
		writeJSONError(w, http.StatusBadGateway, tr(locale, "watch.subtitle_provider_error"))
		return
	}
	if len(results) > maxSearchResults {
		results = results[:maxSearchResults]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": provider.Name(),
		"results":  results,
	})
}

// handleDownloadSubtitle fetches one result as WebVTT and hands it straight to
// the browser. NOTHING is persisted: this is the ephemeral half, so a visitor can
// try a subtitle without committing it to the catalog for everyone else.
func (s *Server) handleDownloadSubtitle(w http.ResponseWriter, r *http.Request) {
	locale := apiLocale(r)
	user := userFrom(r.Context())

	provider := s.subtitles.provider(r.URL.Query().Get("provider"))
	if provider == nil {
		writeJSONError(w, http.StatusServiceUnavailable, tr(locale, "watch.subtitle_search_unavailable"))
		return
	}
	fileID := strings.TrimSpace(r.URL.Query().Get("file_id"))
	if fileID == "" {
		writeJSONError(w, http.StatusBadRequest, tr(locale, "watch.subtitle_query_required"))
		return
	}
	// This is the call that spends the shared account's daily quota, so it is
	// capped per user AND overall.
	if !s.subtitles.download.allow(rateKey(user)) || !s.subtitles.global.allow("global") {
		writeJSONError(w, http.StatusTooManyRequests, tr(locale, "watch.subtitle_rate_limited"))
		return
	}

	data, format, err := provider.Download(r.Context(), fileID)
	if err != nil {
		log.Printf("subtitles: download %s/%s: %v", provider.Name(), fileID, err)
		writeJSONError(w, http.StatusBadGateway, tr(locale, "watch.subtitle_provider_error"))
		return
	}
	w.Header().Set("Content-Type", subtitleContentType(format))
	w.Write(stripASSOverrides(data))
}

// handleSaveSubtitle downloads a search hit and persists it: file into the
// shared blob store, row into `subtitles` tagged with the contributing account.
// From here on every visitor sees it on this movie/episode.
//
// Step order matters. Everything cheap and rejectable happens BEFORE the
// provider download, so a forged owner id or a duplicate can never spend a unit
// of the shared quota.
func (s *Server) handleSaveSubtitle(w http.ResponseWriter, r *http.Request) {
	locale := apiLocale(r)
	user := userFrom(r.Context())

	var body struct {
		Provider      string `json:"provider"`
		FileID        string `json:"file_id"`
		Language      string `json:"language"`
		Name          string `json:"name"`
		DownloadCount int    `json:"download_count"`
		TitleID       *int64 `json:"title_id"`
		EpisodeID     *int64 `json:"episode_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSubtitleSaveBody)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, tr(locale, "watch.subtitle_save_failed"))
		return
	}
	// Mirrors chk_subtitle_owner: exactly one owner, never both, never neither.
	if (body.TitleID == nil) == (body.EpisodeID == nil) {
		writeJSONError(w, http.StatusBadRequest, tr(locale, "watch.subtitle_save_failed"))
		return
	}
	body.FileID = strings.TrimSpace(body.FileID)
	if body.FileID == "" {
		writeJSONError(w, http.StatusBadRequest, tr(locale, "watch.subtitle_save_failed"))
		return
	}
	provider := s.subtitles.provider(body.Provider)
	if provider == nil {
		writeJSONError(w, http.StatusServiceUnavailable, tr(locale, "watch.subtitle_search_unavailable"))
		return
	}

	// The browser supplies the owner id, so verify it against the catalog rather
	// than trusting it. The FK would catch a bogus id too, but only after the
	// download, and as a 500.
	ok, err := s.ownerExists(r, body.TitleID, body.EpisodeID)
	if err != nil {
		log.Printf("subtitles: owner check for user %d: %v", user.ID, err)
		writeJSONError(w, http.StatusInternalServerError, tr(locale, "watch.subtitle_save_failed"))
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, tr(locale, "watch.subtitle_owner_not_found"))
		return
	}

	// Already saved? Hand back the existing row so the page can just apply it.
	existing, err := s.store.FindSubtitleByProviderFile(r.Context(), body.TitleID, body.EpisodeID, provider.Name(), body.FileID)
	if err != nil {
		log.Printf("subtitles: dedupe check for user %d: %v", user.ID, err)
		writeJSONError(w, http.StatusInternalServerError, tr(locale, "watch.subtitle_save_failed"))
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    tr(locale, "watch.subtitle_already_saved"),
			"subtitle": toWatchSubtitle(*existing),
		})
		return
	}

	if !s.subtitles.download.allow(rateKey(user)) || !s.subtitles.global.allow("global") {
		writeJSONError(w, http.StatusTooManyRequests, tr(locale, "watch.subtitle_rate_limited"))
		return
	}

	data, format, err := provider.Download(r.Context(), body.FileID)
	if err != nil {
		log.Printf("subtitles: download %s/%s for user %d: %v", provider.Name(), body.FileID, user.ID, err)
		writeJSONError(w, http.StatusBadGateway, tr(locale, "watch.subtitle_provider_error"))
		return
	}
	data = stripASSOverrides(data)

	language := strings.TrimSpace(body.Language)
	if language == "" {
		language = "sub"
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = provider.Name() + " " + body.FileID
	}

	store := s.blobs[s.blobPrimary]
	if store == nil {
		log.Printf("subtitles: primary blob store %q not configured", s.blobPrimary)
		writeJSONError(w, http.StatusInternalServerError, tr(locale, "watch.subtitle_save_failed"))
		return
	}
	key := subtitleStorageKey(body.TitleID, body.EpisodeID, provider.Name(), body.FileID, language, format)
	if err := store.Put(r.Context(), key, data, subtitleContentType(format)); err != nil {
		log.Printf("subtitles: put %s: %v", key, err)
		writeJSONError(w, http.StatusInternalServerError, tr(locale, "watch.subtitle_save_failed"))
		return
	}

	sub := Subtitle{
		TitleID:        body.TitleID,
		EpisodeID:      body.EpisodeID,
		Provider:       provider.Name(),
		ProviderFileID: body.FileID,
		Language:       language,
		Name:           clampBytes(name, 512),
		DownloadCount:  body.DownloadCount,
		Format:         format,
		StorageBackend: store.Name(),
		StorageKey:     key,
		AddedByUserID:  &user.ID,
	}
	if err := s.store.AddSubtitle(r.Context(), &sub); err != nil {
		// Roll the blob back, or a failed save leaves a file on a shared volume
		// that nothing references and nobody will ever clean up.
		if delErr := store.Delete(r.Context(), key); delErr != nil {
			log.Printf("subtitles: rollback %s: %v", key, delErr)
		}
		log.Printf("subtitles: insert for user %d: %v", user.ID, err)
		writeJSONError(w, http.StatusInternalServerError, tr(locale, "watch.subtitle_save_failed"))
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"subtitle": toWatchSubtitle(sub)})
}

// ownerExists validates whichever of the two owner ids is set.
func (s *Server) ownerExists(r *http.Request, titleID, episodeID *int64) (bool, error) {
	if titleID != nil {
		return s.store.TitleExists(r.Context(), *titleID)
	}
	return s.store.EpisodeExists(r.Context(), *episodeID)
}

// clampBytes caps a string at n BYTES without splitting a UTF-8 rune, so a long
// release name cannot overflow subtitles.name (VARCHAR(512)) or be stored with a
// mangled trailing character. Distinct from server.go's truncate, which counts
// runes and appends an ellipsis for display; this one is a storage guard.
func clampBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 { // step back over continuation bytes
		n--
	}
	return s[:n]
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

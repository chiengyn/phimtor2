package main

import (
	"context"
	"time"
)

// SubtitleProvider is the common interface over a subtitle source. Providers are
// looked up by name so more platforms can be added without touching the HTTP
// layer. Download returns the file bytes plus its format (e.g. "vtt"); callers
// either hand them straight to the browser or persist both.
//
// This file, opensubtitles.go and subsource.go are DUPLICATED from admin/ rather
// than shared, the same convention this repo applies to models.go, store.go and
// static/mkvplayer.js. Fix a provider bug in one, mirror it to the other.
type SubtitleProvider interface {
	Name() string
	Enabled() bool
	Search(ctx context.Context, p SearchParams) ([]SubtitleResult, error)
	Download(ctx context.Context, fileID string) (data []byte, format string, err error)
}

// SubtitleResult is the trimmed shape of one search hit returned to the browser.
// It carries the Provider so the page can pass it back when applying or saving
// (the saved row in the DB is the separate Subtitle model in models.go).
type SubtitleResult struct {
	Provider string `json:"provider"`
	// FileID is an opaque, provider-defined handle the UI round-trips back to
	// Download. It is a string (not the numeric OpenSubtitles file id) so other
	// providers can encode richer locators — subsource, for instance, packs a
	// subtitle id plus episode into it.
	FileID        string `json:"fileId"`
	Language      string `json:"language"`
	Release       string `json:"release"`
	DownloadCount int    `json:"downloadCount"`
}

type SearchParams struct {
	Query     string
	Languages string
	Season    int
	Episode   int
}

// defaultSubtitleProvider is preferred when a request does not name one.
// subtitleProviderOrder is the order the UI lists providers in.
const defaultSubtitleProvider = "opensubtitles"

var subtitleProviderOrder = []string{"opensubtitles", "subsource"}

// maxSearchResults caps what a single search hands the browser. Providers can
// return hundreds of near-identical releases and the watch page renders every
// row, so the tail is pure weight.
const maxSearchResults = 50

// subtitleService is the provider registry plus the quota guards around it.
//
// It is nil-safe in the same way googleClient and billingService are: when no
// provider key is configured newSubtitleService returns nil, enabled() reports
// false, the routes are never registered and the watch page renders no panel.
// That is the clean rollback for this whole feature.
type subtitleService struct {
	providers map[string]SubtitleProvider

	// The OpenSubtitles daily download quota belongs to ONE shared account — the
	// same API key the admin uses — so a single visitor must not be able to drain
	// it for everybody. search is the cheap call, download the expensive one, and
	// global is the backstop across all users.
	search   *rateLimiter
	download *rateLimiter
	global   *rateLimiter
}

// newSubtitleService registers every provider whose key is set, mirroring
// admin/main.go. Returns nil when none is configured.
func newSubtitleService(cfg Config) *subtitleService {
	providers := map[string]SubtitleProvider{}
	if cfg.OpenSubtitlesAPIKey != "" {
		providers["opensubtitles"] = NewOpenSubtitlesClient(
			cfg.OpenSubtitlesAPIKey, cfg.OpenSubtitlesUserAgent,
			cfg.OpenSubtitlesUsername, cfg.OpenSubtitlesPassword)
	}
	if cfg.SubSourceAPIKey != "" {
		providers["subsource"] = NewSubSourceClient(cfg.SubSourceAPIKey, cfg.SubSourceUserAgent)
	}
	if len(providers) == 0 {
		return nil
	}
	return &subtitleService{
		providers: providers,
		search:    newRateLimiter(cfg.SubtitleSearchPerHour, time.Hour),
		download:  newRateLimiter(cfg.SubtitleDownloadsPerDay, 24*time.Hour),
		global:    newRateLimiter(cfg.SubtitleDownloadsGlobalPerDay, 24*time.Hour),
	}
}

// enabled reports whether any provider is usable. Nil-safe.
func (s *subtitleService) enabled() bool {
	return s != nil && len(s.enabledNames()) > 0
}

// provider returns the named provider. When name is empty it falls back to the
// default provider if enabled, otherwise any enabled one. Returns nil when no
// such (enabled) provider is registered.
func (s *subtitleService) provider(name string) SubtitleProvider {
	if s == nil {
		return nil
	}
	if name != "" {
		if p := s.providers[name]; p != nil && p.Enabled() {
			return p
		}
		return nil
	}
	if p := s.providers[defaultSubtitleProvider]; p != nil && p.Enabled() {
		return p
	}
	for _, n := range s.enabledNames() {
		return s.providers[n]
	}
	return nil
}

// enabledNames lists the registered, enabled providers in a stable display
// order; the watch page uses it to decide whether to show a provider selector.
func (s *subtitleService) enabledNames() []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.providers))
	for _, n := range subtitleProviderOrder {
		if p := s.providers[n]; p != nil && p.Enabled() {
			names = append(names, n)
		}
	}
	return names
}

package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// Each title file is dated by the newest title in it, so a crawler refetches
// only the files that changed; the last file may be short.
func TestSitemapTitleChunks(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	got := sitemapTitleChunks([]time.Time{day(3), day(9), day(1), day(2), day(5)}, 2)
	want := []time.Time{day(9), day(2), day(5)}
	if len(got) != len(want) {
		t.Fatalf("got %d chunks, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("chunk %d lastmod = %v, want %v", i+1, got[i], want[i])
		}
	}
	if got := sitemapTitleChunks(nil, 2); len(got) != 0 {
		t.Errorf("no titles should mean no title files, got %d", len(got))
	}
}

// A file must stay under the protocol's 50,000-entry cap: every title is
// listed once per locale.
func TestSitemapTitleFilesFitTheProtocol(t *testing.T) {
	if n := sitemapTitlesPerFile * len(supportedLocales); n > 50000 {
		t.Fatalf("a title file would hold %d entries; the cap is 50,000 — lower sitemapTitlesPerFile", n)
	}
}

// The static-pages file needs no database: every locale's home, plus the TV
// guide when an APK is published.
func TestSitemapPages(t *testing.T) {
	s := gatedServer(t)
	w := get(s, "/sitemap-pages.xml")
	if w.Code != http.StatusOK {
		t.Fatalf("/sitemap-pages.xml: %d", w.Code)
	}
	body := w.Body.String()
	for _, locale := range supportedLocales {
		if !strings.Contains(body, "<loc>http://viewer.example"+localeHome(locale)+"</loc>") {
			t.Errorf("home for %s missing", locale)
		}
		if !strings.Contains(body, "<loc>http://viewer.example"+localeURL(locale, "/tv-app")+"</loc>") {
			t.Errorf("TV guide for %s missing", locale)
		}
	}
	if strings.Contains(body, "/titles/") {
		t.Error("titles belong in the sitemap-titles-N.xml files")
	}
}

// A malformed page number is a 404 before the store is touched (the test
// server has none, so reaching it would panic into a 500).
func TestSitemapTitlesRejectsBadPages(t *testing.T) {
	s := gatedServer(t)
	for _, path := range []string{"/sitemap-titles-0.xml", "/sitemap-titles--1.xml", "/sitemap-titles-abc.xml", "/sitemap-titles-01.xml"} {
		if w := get(s, path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, w.Code)
		}
	}
}

// The 404s above prove nothing unless the pattern matches at all, so pin that
// a well-formed file name routes to the handler with its page number.
func TestSitemapTitlesRouteMatches(t *testing.T) {
	s := gatedServer(t)
	rctx := chi.NewRouteContext()
	if !s.router.Match(rctx, http.MethodGet, "/sitemap-titles-12.xml") {
		t.Fatal("/sitemap-titles-12.xml does not match any route")
	}
	if got := rctx.URLParam("page"); got != "12" {
		t.Errorf("page param = %q, want 12", got)
	}
}

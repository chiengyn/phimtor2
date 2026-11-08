package main

import (
	"context"
	"net/http/httptest"
	"testing"
)

// TitleNames.For must resolve exactly like localizedTitleExpr: own
// translation, then the fallback locale's (vi for en, en for the rest), then
// the base column, skipping blanks.
func TestTitleNamesFor(t *testing.T) {
	n := TitleNames{
		Title:         "Lãng Lãng",
		OriginalTitle: "Euphoria",
		Translations:  map[Locale]string{LocaleVI: "Lãng Lãng", LocaleEN: "Euphoria", LocaleKO: "  "},
	}
	cases := map[Locale]string{
		LocaleVI:   "Lãng Lãng",
		LocaleEN:   "Euphoria",
		LocaleJA:   "Euphoria", // no ja row → en
		LocaleKO:   "Euphoria", // blank ko row is skipped → en
		LocaleZHCN: "Euphoria",
	}
	for locale, want := range cases {
		if got := n.For(locale); got != want {
			t.Errorf("%s: %q, want %q", locale, got, want)
		}
	}
	// No translations at all: the base column for everyone.
	bare := TitleNames{Title: "Base"}
	if got := bare.For(LocaleEN); got != "Base" {
		t.Errorf("bare en: %q, want Base", got)
	}
	// en falls back to vi, not to the base column.
	viOnly := TitleNames{Title: "Base", Translations: map[Locale]string{LocaleVI: "Tiếng Việt"}}
	if got := viOnly.For(LocaleEN); got != "Tiếng Việt" {
		t.Errorf("en with only vi: %q, want the vi name", got)
	}
}

// A watch page's language menu and hreflang links use each locale's own URL,
// not this locale's slug under another prefix.
func TestWatchPageLanguageLinksUseEachLocalesSlug(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/en/watch/episode/euphoria-episode-1-168", nil)
	r = r.WithContext(context.WithValue(r.Context(), localeContextKey{}, LocaleEN))

	names := TitleNames{Title: "Lãng Lãng", Translations: map[Locale]string{LocaleVI: "Lãng Lãng", LocaleEN: "Euphoria"}}
	urls := map[Locale]string{}
	for _, l := range supportedLocales {
		urls[l] = watchEpisodePath(l, 168, 1, names.For(l))
	}
	pd := s.newPageData(r, watchData{languageURLs: urls})
	got := map[Locale]string{}
	for _, o := range pd.Languages {
		got[o.Locale] = o.URL
	}
	if got[LocaleVI] != "/vi/watch/episode/lang-lang-tap-1-168" {
		t.Errorf("vi link = %q", got[LocaleVI])
	}
	if got[LocaleEN] != "/en/watch/episode/euphoria-episode-1-168" {
		t.Errorf("en link = %q", got[LocaleEN])
	}
	if got[LocaleJA] != "/ja/watch/episode/euphoria-episode-1-168" {
		t.Errorf("ja link = %q", got[LocaleJA])
	}

	// No URLs (a failed lookup): the prefix swap still applies.
	pd = s.newPageData(r, watchData{})
	for _, o := range pd.Languages {
		if o.Locale == LocaleVI && o.URL != "/vi/watch/episode/euphoria-episode-1-168" {
			t.Errorf("fallback vi link = %q, want the prefix swap", o.URL)
		}
	}
}

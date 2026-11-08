package main

import (
	"regexp"
	"testing"
)

// Episode watch URLs use the page locale's word for "episode", never the
// Vietnamese "tap" outside /vi.
func TestWatchEpisodePathIsLocalized(t *testing.T) {
	cases := []struct {
		locale Locale
		title  string
		want   string
	}{
		{LocaleVI, "Euphoria", "/vi/watch/episode/euphoria-tap-1-168"},
		{LocaleEN, "Euphoria", "/en/watch/episode/euphoria-episode-1-168"},
		{LocaleJA, "Euphoria", "/ja/watch/episode/euphoria-episode-1-168"},
		// An unsluggable (CJK-only) title still gets a readable word.
		{LocaleZHCN, "亢奋", "/zh-cn/watch/episode/episode-1-168"},
	}
	for _, tc := range cases {
		if got := watchEpisodePath(tc.locale, 168, 1, tc.title); got != tc.want {
			t.Errorf("%s %q: got %q, want %q", tc.locale, tc.title, got, tc.want)
		}
	}
}

// Every locale's word must survive slugify unchanged, or its URLs would carry
// characters the rest of the slug scheme never produces.
func TestEpisodeSlugWordsAreSlugSafe(t *testing.T) {
	safe := regexp.MustCompile(`^[a-z0-9]+$`)
	for _, locale := range supportedLocales {
		if w := episodeSlugWord(locale); !safe.MatchString(w) {
			t.Errorf("%s: episode slug word %q is not ASCII lowercase", locale, w)
		}
	}
}

// Links minted before the word was localized ("…-tap-1-168" on /en) must keep
// resolving: only the trailing id is parsed.
func TestOldEpisodeSlugsStillResolve(t *testing.T) {
	for _, seg := range []string{"euphoria-tap-1-168", "euphoria-episode-1-168", "tap-1-168", "168"} {
		if id, err := parseIDFromSlug(seg); err != nil || id != 168 {
			t.Errorf("%q: id %d, err %v; want 168", seg, id, err)
		}
	}
}

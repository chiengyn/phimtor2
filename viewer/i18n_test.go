package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTranslationCatalogsMatch(t *testing.T) {
	if err := validateMessages(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalizedRouter(t *testing.T) {
	s := &Server{}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	s.setupRouter()

	request := func(target, language string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		if language != "" {
			r.Header.Set("Accept-Language", language)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	if got := request("/", "en-US").Header().Get("Location"); got != "/en/" {
		t.Fatalf("root location = %q, want /en/", got)
	}
	if got := request("/", "zh-TW,zh;q=0.9").Header().Get("Location"); got != "/zh-tw/" {
		t.Fatalf("Traditional Chinese root location = %q, want /zh-tw/", got)
	}
	if got := request("/?q=dune", "en-US").Header().Get("Location"); got != "/vi/?q=dune" {
		t.Fatalf("legacy query location = %q, want Vietnamese compatibility URL", got)
	}
	if got := request("/titles/dune-42", "").Header().Get("Location"); got != "/vi/titles/dune-42" {
		t.Fatalf("legacy title location = %q", got)
	}
	missing := request("/en/missing", "")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "We could not find") {
		t.Fatalf("English 404 = %d %q", missing.Code, missing.Body.String())
	}
}

func TestLocalizedTemplatesParse(t *testing.T) {
	s := &Server{}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}

	pages := []struct {
		name      string
		templates localizedTemplate
		data      any
	}{
		{"home", s.home, homeData{}},
		{"detail", s.detail, &Title{ID: 1, Title: "Example", Type: "movie"}},
		{"watch", s.watch, watchData{Heading: "Example", VideosJSON: "[]", SubtitlesJSON: "[]", HasVideo: true}},
		{"bookmarks", s.bookmarks, bookmarksData{}},
		{"not-found", s.notFound, nil},
		{"plans", s.plans, plansData{}},
		{"invoice", s.invoice, invoiceData{}},
	}
	for _, locale := range supportedLocales {
		for _, page := range pages {
			t.Run(string(locale)+"/"+page.name, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodGet, localeHome(locale), nil)
				r = r.WithContext(context.WithValue(r.Context(), localeContextKey{}, locale))
				var output bytes.Buffer
				if err := page.templates[locale].ExecuteTemplate(&output, "layout", s.newPageData(r, page.data)); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(output.String(), `lang="`+string(locale)+`"`) {
					t.Fatalf("rendered page does not declare locale %q", locale)
				}
				if !strings.Contains(output.String(), `class="language-menu"`) ||
					!strings.Contains(output.String(), `class="language-option is-active"`) {
					t.Fatal("language dropdown was not rendered")
				}
				if got := strings.Count(output.String(), `class="language-option`); got != len(supportedLocales) {
					t.Fatalf("rendered %d language options, want %d", got, len(supportedLocales))
				}
				if locale == LocaleEN && page.name == "watch" && !strings.Contains(output.String(), `signInQuality: "Sign in to watch in %s"`) {
					t.Fatal("watch translations were not emitted as JavaScript string literals")
				}
			})
		}
	}
}

func TestPreferredLocale(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Language", "en-US,en;q=0.9,vi;q=0.8")
	if got := preferredLocale(r); got != LocaleEN {
		t.Fatalf("preferredLocale = %q, want en", got)
	}
	r.AddCookie(&http.Cookie{Name: "phimnet_locale", Value: "vi"})
	if got := preferredLocale(r); got != LocaleVI {
		t.Fatalf("cookie should win: preferredLocale = %q, want vi", got)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Language", "vi;q=0.2,en-AU;q=0.9")
	if got := preferredLocale(r); got != LocaleEN {
		t.Fatalf("quality weights should win: preferredLocale = %q, want en", got)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Language", "zh-Hant-HK,zh-Hans;q=0.8,en;q=0.7")
	if got := preferredLocale(r); got != LocaleZHTW {
		t.Fatalf("Chinese script should be preserved: preferredLocale = %q, want zh-tw", got)
	}
}

func TestParseLocale(t *testing.T) {
	tests := map[string]Locale{
		"zh": LocaleZHCN, "zh-CN": LocaleZHCN, "zh-Hans": LocaleZHCN,
		"zh-SG": LocaleZHCN, "zh-TW": LocaleZHTW, "zh-Hant": LocaleZHTW,
		"zh-HK": LocaleZHTW, "ko-KR": LocaleKO, "ja-JP": LocaleJA,
	}
	for input, want := range tests {
		if got, ok := parseLocale(input); !ok || got != want {
			t.Errorf("parseLocale(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
}

func TestLocalizedPaths(t *testing.T) {
	if got, want := titlePath(LocaleEN, 42, "Dune: Part Two", ""), "/en/titles/dune-part-two-42"; got != want {
		t.Fatalf("titlePath = %q, want %q", got, want)
	}
	if got, want := homeURL(LocaleVI, TitleFilter{Type: "movie"}, 2), "/vi/?page=2&type=movie"; got != want {
		t.Fatalf("homeURL = %q, want %q", got, want)
	}
}

func TestLanguageOptionsPreservePageAndQuery(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/en/?genre=28&type=movie", nil)
	r = r.WithContext(context.WithValue(r.Context(), localeContextKey{}, LocaleEN))
	options := requestLanguageOptions(r)
	if len(options) != len(supportedLocales) {
		t.Fatalf("got %d language options, want %d", len(options), len(supportedLocales))
	}
	if !options[1].Current || options[1].Name != "English" || options[1].URL != "/en/?genre=28&type=movie" {
		t.Fatalf("English option = %#v", options[1])
	}
	if options[0].Current || options[0].Name != "Tiếng Việt" || options[0].URL != "/vi/?genre=28&type=movie" {
		t.Fatalf("Vietnamese option = %#v", options[0])
	}
}

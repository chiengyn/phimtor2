package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A realistic tag: its escaped slashes must reach the page byte for byte.
const testAdScript = `<script>var s = "\/\/ads.example\/zone?a=1&b=2";</script>`

func adTestServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{ads: newAdCache(func(context.Context) (string, error) { return testAdScript, nil })}
	s.ads.refresh(context.Background())
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	return s
}

func renderedHasAd(t *testing.T, s *Server, templates localizedTemplate, data any, user *User) bool {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, localeHome(LocaleEN), nil)
	ctx := context.WithValue(r.Context(), localeContextKey{}, LocaleEN)
	if user != nil {
		ctx = context.WithValue(ctx, userCtxKey, user)
	}
	r = r.WithContext(ctx)
	var out bytes.Buffer
	if err := templates[LocaleEN].ExecuteTemplate(&out, "layout", s.newPageData(r, data)); err != nil {
		t.Fatal(err)
	}
	return strings.Contains(out.String(), testAdScript)
}

func TestAdScriptSkipsEntitledUsers(t *testing.T) {
	s := adTestServer(t)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	cases := []struct {
		name string
		user *User
		want bool
	}{
		{"anonymous", nil, true},
		{"signed in, unpaid", &User{ID: 1}, true},
		{"expired pass", &User{ID: 1, PlanExpiresAt: &past}, true},
		{"expired comp", &User{ID: 1, CompExpiresAt: &past}, true},
		{"active pass", &User{ID: 1, PlanExpiresAt: &future}, false},
		{"active comp", &User{ID: 1, CompExpiresAt: &future}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderedHasAd(t, s, s.notFound, nil, c.user); got != c.want {
				t.Fatalf("ad rendered = %v, want %v", got, c.want)
			}
		})
	}
}

// People pay, sign in or approve a TV on the last four pages, so they must never
// carry a third-party script, whoever is looking.
func TestAdScriptPerPage(t *testing.T) {
	s := adTestServer(t)
	pages := []struct {
		name      string
		templates localizedTemplate
		data      any
		want      bool
	}{
		{"home", s.home, homeData{}, true},
		{"detail", s.detail, &Title{ID: 1, Title: "Example", Type: "movie"}, true},
		{"watch", s.watch, watchData{Heading: "Example", VideosJSON: "[]", SubtitlesJSON: "[]", HasVideo: true}, true},
		{"bookmarks", s.bookmarks, bookmarksData{}, true},
		{"not-found", s.notFound, nil, true},
		{"tv-app", s.tvApp, tvAppData{}, true},
		{"plans", s.plans, plansData{}, false},
		{"invoice", s.invoice, invoiceData{}, false},
		{"login", s.login, loginData{}, false},
		{"link", s.link, linkData{SignedIn: true}, false},
	}
	for _, p := range pages {
		t.Run(p.name, func(t *testing.T) {
			if got := renderedHasAd(t, s, p.templates, p.data, nil); got != p.want {
				t.Fatalf("ad rendered = %v, want %v", got, p.want)
			}
		})
	}
}

// Ads off in the database reach the page as nothing, and a failed read keeps the
// last known value rather than starting or stopping ads.
func TestAdCacheRefresh(t *testing.T) {
	script, err := "<script></script>", error(nil)
	c := newAdCache(func(context.Context) (string, error) { return script, err })
	ctx := context.Background()

	if got := c.script(); got != "" {
		t.Fatalf("before the first load = %q, want empty", got)
	}
	c.refresh(ctx)
	if got := c.script(); string(got) != script {
		t.Fatalf("after load = %q", got)
	}

	err = errors.New("database down")
	want := script
	script = "<script>new</script>"
	c.refresh(ctx)
	if got := c.script(); string(got) != want {
		t.Fatalf("after a failed read = %q, want the last known %q", got, want)
	}

	script, err = "", nil
	c.refresh(ctx)
	if got := c.script(); got != "" {
		t.Fatalf("after switching off = %q, want empty", got)
	}

	var none *adCache
	if got := none.script(); got != "" {
		t.Fatalf("nil cache = %q, want empty", got)
	}
}

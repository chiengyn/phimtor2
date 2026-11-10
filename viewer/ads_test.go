package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// A realistic tag: its escaped slashes must reach the page byte for byte.
const testAdScript = `<script>var s = "\/\/ads.example\/zone?a=1&b=2";</script>`

// testSlotCode is a distinct banner per slot, so a test can tell which landed.
func testSlotCode(key string) string { return `<script>banner("` + key + `")</script>` }

func adTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := AdConfig{Script: testAdScript, Slots: map[string]AdSlotCode{}}
	for _, key := range adSlotKeys {
		cfg.Slots[key] = AdSlotCode{Desktop: testSlotCode(key)}
	}
	s := &Server{ads: newAdCache(func(context.Context) (AdConfig, error) { return cfg, nil })}
	s.ads.refresh(context.Background())
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	return s
}

func renderedHasAd(t *testing.T, s *Server, templates localizedTemplate, data any, user *User) bool {
	t.Helper()
	return strings.Contains(renderAdPage(t, s, templates, data, user), testAdScript)
}

func renderAdPage(t *testing.T, s *Server, templates localizedTemplate, data any, user *User) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, localeHome(LocaleEN), nil)
	ctx := context.WithValue(r.Context(), localeContextKey{}, LocaleEN)
	if user != nil {
		ctx = context.WithValue(ctx, userCtxKey, user)
	}
	r = r.WithContext(ctx)
	var out bytes.Buffer
	if err := s.renderPage(&out, r, templates, data); err != nil {
		t.Fatal(err)
	}
	return out.String()
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

// Every slot lands on the page it was designed for, wrapped, and nowhere else.
// Ad-free visitors and no-ads pages get no slot and no stray marker either.
func TestAdSlotsPerPage(t *testing.T) {
	s := adTestServer(t)
	rows := []Row{{Key: "a"}, {Key: "b"}, {Key: "c"}, {Key: "d"}}
	future := time.Now().Add(time.Hour)
	pages := []struct {
		name      string
		templates localizedTemplate
		data      any
		user      *User
		want      []string
	}{
		{"home browse", s.home, homeData{Rows: rows}, nil, []string{"home_top", "home_feed", "footer"}},
		{"home few rows", s.home, homeData{Rows: rows[:2]}, nil, []string{"home_top", "footer"}},
		{"home filtered", s.home, homeData{Filtered: true}, nil, []string{"home_top", "home_feed", "footer"}},
		{"detail", s.detail, &Title{ID: 1, Title: "Example", Type: "movie"}, nil, []string{"detail", "footer"}},
		{"watch", s.watch, watchData{Heading: "Example", VideosJSON: "[]", SubtitlesJSON: "[]", HasVideo: true}, nil, []string{"watch", "footer"}},
		{"bookmarks", s.bookmarks, bookmarksData{}, nil, []string{"footer"}},
		{"watch, pass holder", s.watch, watchData{Heading: "Example", VideosJSON: "[]", SubtitlesJSON: "[]", HasVideo: true}, &User{ID: 1, PlanExpiresAt: &future}, nil},
		{"plans", s.plans, plansData{}, nil, nil},
		{"invoice", s.invoice, invoiceData{}, nil, nil},
		{"login", s.login, loginData{}, nil, nil},
		{"link", s.link, linkData{SignedIn: true}, nil, nil},
	}
	for _, p := range pages {
		t.Run(p.name, func(t *testing.T) {
			out := renderAdPage(t, s, p.templates, p.data, p.user)
			if strings.Contains(out, "phimnet-ad:") {
				t.Fatal("an unfilled slot marker reached the page")
			}
			for _, key := range adSlotKeys {
				got := strings.Count(out, testSlotCode(key))
				want := 0
				if slices.Contains(p.want, key) {
					want = 1
				}
				if got != want {
					t.Errorf("slot %s rendered %d times, want %d", key, got, want)
				}
			}
			if len(p.want) > 0 && !strings.Contains(out, `<div class="ad-slot" data-ad-slot="`+p.want[0]+`" data-ad-device="all">`+testSlotCode(p.want[0])+`</div>`) {
				t.Errorf("slot %s is not wrapped in its ad-slot div", p.want[0])
			}
		})
	}
}

// A slot with no code leaves no empty box behind, and an unknown key in a
// template is an error rather than a silently missing banner.
func TestAdSlotEmptyAndUnknown(t *testing.T) {
	snap := newAdSnapshot(AdConfig{Slots: map[string]AdSlotCode{"watch": {Desktop: "<b>x</b>"}}})
	page := adMarker("home_top") + "|" + adMarker("watch")
	if got := snap.fill.Replace(page); got != `|<div class="ad-slot" data-ad-slot="watch" data-ad-device="all"><b>x</b></div>` {
		t.Fatalf("filled page = %q", got)
	}
	if _, err := adSlotMarker("sidebar"); err == nil {
		t.Fatal("unknown slot key accepted")
	}
}

// With a mobile code the slot becomes a script that writes exactly one variant.
// Its two string literals must decode to the wrapped codes, and a </script> in
// the ad code must not close the switch early.
func TestAdSlotMobileVariant(t *testing.T) {
	desktop := `<script>atOptions={key:"d"}</script><script src="//ads.example/d.js"></script>`
	mobile := `<script>document.write('<a href="/x?a=1&b=2">m</a>')</script>`
	cases := []struct {
		name                 string
		code                 AdSlotCode
		wantMobile, wantDesk string
	}{
		{"both", AdSlotCode{Desktop: desktop, Mobile: mobile},
			`<div class="ad-slot" data-ad-slot="watch" data-ad-device="mobile">` + mobile + `</div>`,
			`<div class="ad-slot" data-ad-slot="watch" data-ad-device="desktop">` + desktop + `</div>`},
		{"mobile only", AdSlotCode{Mobile: mobile},
			`<div class="ad-slot" data-ad-slot="watch" data-ad-device="mobile">` + mobile + `</div>`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := adSlotMarkup("watch", c.code)
			if n := strings.Count(got, "</script>"); n != 1 {
				t.Fatalf("markup holds %d </script>, want only its own closing tag: %s", n, got)
			}
			rest, ok := strings.CutPrefix(got, `<script>document.write(matchMedia("`+adMobileQuery+`").matches?`)
			if !ok {
				t.Fatalf("unexpected markup: %s", got)
			}
			dec := json.NewDecoder(strings.NewReader(rest))
			var m, d string
			if err := dec.Decode(&m); err != nil {
				t.Fatal(err)
			}
			rest = rest[dec.InputOffset():]
			if !strings.HasPrefix(rest, ":") {
				t.Fatalf("no ternary separator: %s", rest)
			}
			dec = json.NewDecoder(strings.NewReader(rest[1:]))
			if err := dec.Decode(&d); err != nil {
				t.Fatal(err)
			}
			if tail := rest[1+dec.InputOffset():]; tail != ")</script>" {
				t.Fatalf("trailing markup = %q", tail)
			}
			if m != c.wantMobile || d != c.wantDesk {
				t.Fatalf("mobile = %q\ndesktop = %q", m, d)
			}
		})
	}
	if got := adSlotMarkup("watch", AdSlotCode{}); got != "" {
		t.Fatalf("a slot with no code = %q, want nothing", got)
	}
}

// Ads off in the database reach the page as nothing, and a failed read keeps the
// last known value rather than starting or stopping ads.
func TestAdCacheRefresh(t *testing.T) {
	script, err := "<script></script>", error(nil)
	c := newAdCache(func(context.Context) (AdConfig, error) { return AdConfig{Script: script}, err })
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

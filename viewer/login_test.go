package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// gatedServer is a production-shaped deploy: accounts on (so watching needs
// sign-in), and a published TV app so the TV guide is routed too.
func gatedServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		google:   newGoogleClient("client-id", "secret", "http://localhost/cb"),
		tvApkDir: t.TempDir(),
	}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	s.setupRouter()
	return s
}

// Browsing is public: a signed-out visitor reaches every catalog page, the
// watch page included (resolutionLock is what stops them playing). Only /link,
// which acts on an account, still redirects to /login.
//
// Most catalog handlers need the database, so the routing is checked by
// middleware count: every catalog route must carry exactly the middleware the
// public TV guide does, and /link one more (requireLogin).
func TestCatalogIsPublicWhileSignedOut(t *testing.T) {
	s := gatedServer(t)
	middlewares := map[string]int{}
	err := chi.Walk(s.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		middlewares[method+" "+route] = len(mws)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	public, ok := middlewares["GET /{locale}/tv-app"]
	if !ok {
		t.Fatal("/{locale}/tv-app not routed")
	}
	for _, route := range []string{
		"GET /{locale}/",
		"GET /{locale}/titles/{id}",
		"GET /{locale}/bookmarks",
		"GET /{locale}/watch/movie/{id}",
		"GET /{locale}/watch/episode/{id}",
		"GET /api/catalog/cards",
	} {
		got, ok := middlewares[route]
		if !ok {
			t.Errorf("%s not routed", route)
			continue
		}
		// /api/catalog/cards sits outside the /{locale} subrouter, so it lacks
		// requireLocale; it must carry no extra gate either.
		want := public
		if route == "GET /api/catalog/cards" {
			want = public - 1
		}
		if got != want {
			t.Errorf("%s carries %d middlewares, want %d (is it behind requireLogin?)", route, got, want)
		}
	}
	if got := middlewares["GET /{locale}/link"]; got != public+1 {
		t.Errorf("/{locale}/link carries %d middlewares, want %d (requireLogin)", got, public+1)
	}

	// The pages that need no database, end to end.
	w := get(s, "/en/bookmarks")
	if w.Code != http.StatusOK {
		t.Fatalf("/en/bookmarks signed out: %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `href="/en/?type=movie"`) {
		t.Error("a signed-out visitor is not shown the catalog nav")
	}
	if w := get(s, "/api/catalog/cards?ids="); w.Code != http.StatusOK {
		t.Errorf("/api/catalog/cards signed out: %d, want 200", w.Code)
	}
}

// /link approves a TV pairing for an account, so a signed-out visitor is sent
// through /login and back, never cached.
func TestLinkPageRedirectsToLogin(t *testing.T) {
	w := get(gatedServer(t), "/en/link?code=ACDE-FGHJ")
	if w.Code != http.StatusFound {
		t.Fatalf("/en/link: status %d, want 302", w.Code)
	}
	if got, want := w.Header().Get("Location"), "/en/login?next=%2Fen%2Flink%3Fcode%3DACDE-FGHJ"; got != want {
		t.Errorf("/en/link: Location %q, want %q", got, want)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("/en/link: Cache-Control %q, want no-store", got)
	}
}

// The sign-in page and the TV guide render for a signed-out visitor.
func TestLoginPageIsPublic(t *testing.T) {
	s := gatedServer(t)

	w := get(s, "/en/login?next=%2Fen%2Ftitles%2Fdune-42")
	if w.Code != http.StatusOK {
		t.Fatalf("/en/login: %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/auth/google/start?next=%2fen%2ftitles%2fdune-42"`) &&
		!strings.Contains(body, `href="/auth/google/start?next=%2Fen%2Ftitles%2Fdune-42"`) {
		t.Error("the Google button does not carry the page the visitor was headed to")
	}

	if w := get(s, "/en/tv-app"); w.Code != http.StatusOK {
		t.Errorf("/en/tv-app should stay public, got %d", w.Code)
	}
}

// A failed attempt is explained on the page, never as raw query text.
func TestLoginPageShowsAFailedAttempt(t *testing.T) {
	body := get(gatedServer(t), "/en/login?login=unverified").Body.String()
	if !strings.Contains(body, "not verified") {
		t.Error("an unverified-email failure is not explained")
	}
	if body := get(gatedServer(t), "/en/login?login=<script>").Body.String(); strings.Contains(body, "login-error") {
		t.Error("an unknown reason rendered an error box")
	}
}

// Signing in from the front door goes back to where the visitor was headed;
// an already-signed-in visitor is sent straight there. Only site-relative
// targets are honoured (safeNext), so the page is no open redirect.
func TestLoginPageRedirectsTheSignedIn(t *testing.T) {
	s := gatedServer(t)
	visit := func(target string) string {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r = r.WithContext(context.WithValue(r.Context(), userCtxKey, &User{ID: 1}))
		r = r.WithContext(context.WithValue(r.Context(), localeContextKey{}, LocaleEN))
		w := httptest.NewRecorder()
		s.handleLoginPage(w, r)
		if w.Code != http.StatusFound {
			t.Fatalf("%s: status %d, want 302", target, w.Code)
		}
		return w.Header().Get("Location")
	}
	if got := visit("/en/login?next=%2Fen%2Ftitles%2Fdune-42"); got != "/en/titles/dune-42" {
		t.Errorf("signed-in visitor sent to %q", got)
	}
	if got := visit("/en/login"); got != "/en/" {
		t.Errorf("signed-in visitor with no next sent to %q, want the home page", got)
	}
	if got := visit("/en/login?next=%2F%2Fevil.example"); got != "/en/" {
		t.Errorf("open redirect: sent to %q", got)
	}
}

// A failed Google round trip lands back on the sign-in page, with the reason
// and the original destination intact, so the visitor can simply try again.
func TestAuthErrorReturnsToLogin(t *testing.T) {
	if got, want := authErrorURL("/en/titles/dune-42", "error"), "/en/login?login=error&next=%2Fen%2Ftitles%2Fdune-42"; got != want {
		t.Fatalf("authErrorURL = %q, want %q", got, want)
	}
	if got, want := authErrorURL("/vi/", "unverified"), "/vi/login?login=unverified"; got != want {
		t.Fatalf("authErrorURL = %q, want %q", got, want)
	}
	if got, want := authErrorURL("/vi/", "blocked"), "/vi/login?login=blocked"; got != want {
		t.Fatalf("authErrorURL = %q, want %q", got, want)
	}
}

// An account the admin has blocked is told so on the front door, in the page's
// language, rather than shown the generic failure.
func TestLoginPageExplainsBlockedAccount(t *testing.T) {
	s := gatedServer(t)
	if body := get(s, "/en/login?login=blocked").Body.String(); !strings.Contains(body, "suspended") {
		t.Error("/en: a blocked account is not explained")
	}
	if body := get(s, "/vi/login?login=blocked").Body.String(); !strings.Contains(body, "bị khoá") {
		t.Error("/vi: a blocked account is not explained")
	}
}

// Without accounts there is no way to sign in, so /login is not routed.
func TestLoginPageNeedsAccounts(t *testing.T) {
	s := &Server{}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	s.setupRouter()
	if w := get(s, "/en/login"); w.Code != http.StatusNotFound {
		t.Errorf("/en/login without accounts: %d, want 404", w.Code)
	}
	if found := routesOf(t, &Server{}); found["GET /{locale}/login"] {
		t.Error("/{locale}/login must NOT be registered without accounts")
	}
}

// Production without sign-in would let anyone watch, so it is refused at boot
// rather than discovered later.
func TestProductionRequiresAccounts(t *testing.T) {
	cfg := Config{
		PublicURL:          "https://phimnet.example",
		SessionSecret:      strings.Repeat("s", minSecretLen),
		SubtitleStorageDir: t.TempDir(),
	}
	if _, err := NewServer(nil, cfg); err == nil || !strings.Contains(err.Error(), "watching requires sign-in") {
		t.Fatalf("NewServer without accounts in production: err = %v, want the sign-in refusal", err)
	}
}

// resolutionLock is what the prepare endpoint enforces for both the web page
// and the TV app, and now that browsing is public it is the ONLY thing that
// makes watching need an account: with accounts on an anonymous caller plays
// NOTHING — not even 720p — while a signed-in visitor keeps every tier they had.
func TestResolutionLockNeedsSignIn(t *testing.T) {
	gated := &Server{google: newGoogleClient("client-id", "secret", "http://localhost/cb")}
	anon := titleAccess{}
	member := titleAccess{signedIn: true}
	entitled := titleAccess{signedIn: true, unlimited: true}

	for _, res := range []string{"720p", "1080p", "2160p", ""} {
		if got := gated.resolutionLock(res, anon); got != lockMember {
			t.Errorf("anonymous %q = %q, want %q", res, got, lockMember)
		}
	}
	// What a signed-out watch page (and an unpaired TV) is handed: every source
	// listed, none playable, each one asking for sign-in.
	for _, v := range gated.toWatchVideos([]Video{{ID: 1, Resolution: "720p"}, {ID: 2, Resolution: "1080p"}}, anon) {
		if v.Available || v.Lock != lockMember {
			t.Errorf("anonymous watch video %+v, want unavailable with lock %q", v, lockMember)
		}
	}
	for _, res := range []string{"720p", "1080p"} {
		if got := gated.resolutionLock(res, member); got != lockNone {
			t.Errorf("signed-in %q = %q, want playable", res, got)
		}
	}
	// Billing is off here, so 4K is "coming soon" for the unentitled — and an
	// existing entitlement still plays.
	if got := gated.resolutionLock("2160p", member); got != lockPaid {
		t.Errorf("signed-in 2160p without billing = %q, want %q", got, lockPaid)
	}
	if got := gated.resolutionLock("2160p", entitled); got != lockNone {
		t.Errorf("entitled 2160p = %q, want playable", got)
	}

	// Accounts off (local dev): there is no way to sign in, so the lock stands down.
	open := &Server{}
	if got := open.resolutionLock("1080p", anon); got != lockNone {
		t.Errorf("accounts off, 1080p = %q, want playable", got)
	}
	if got := open.resolutionLock("2160p", anon); got != lockPaid {
		t.Errorf("accounts off, 2160p = %q, want %q", got, lockPaid)
	}
}

// The TV app opens on its pair-first screen only when the server says sign-in
// is required to browse. Browsing is public, so /me says false in both configs
// and an unpaired TV opens on Home; playing still asks it to pair.
func TestTVMeStatesWhetherSignInIsRequired(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *Server
		want bool
	}{
		{"accounts on", &Server{google: newGoogleClient("client-id", "secret", "http://localhost/cb")}, false},
		{"accounts off", &Server{}, false},
	} {
		w := httptest.NewRecorder()
		tc.s.handleTVMe(w, httptest.NewRequest(http.MethodGet, "/api/tv/v1/me", nil))
		var me struct {
			SignedIn       bool `json:"signed_in"`
			SignInRequired bool `json:"sign_in_required"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if me.SignedIn || me.SignInRequired != tc.want {
			t.Errorf("%s: /me = %+v, want signed_in false, sign_in_required %v", tc.name, me, tc.want)
		}
	}
}

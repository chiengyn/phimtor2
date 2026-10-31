package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gatedServer is a members-only deploy: accounts on, and a published TV app so
// the one public content-free page is routed too.
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

// A signed-out visitor reaches no catalog page: each one redirects to the
// localized front door, remembering the page so sign-in can return there.
func TestMembersOnlyPagesRedirectToLogin(t *testing.T) {
	s := gatedServer(t)
	cases := map[string]string{
		"/en/":                    "/en/login",
		"/vi/?type=movie":         "/vi/login?next=%2Fvi%2F%3Ftype%3Dmovie",
		"/en/titles/dune-42":      "/en/login?next=%2Fen%2Ftitles%2Fdune-42",
		"/ko/watch/movie/42":      "/ko/login?next=%2Fko%2Fwatch%2Fmovie%2F42",
		"/ja/watch/episode/7":     "/ja/login?next=%2Fja%2Fwatch%2Fepisode%2F7",
		"/en/bookmarks":           "/en/login?next=%2Fen%2Fbookmarks",
		"/en/link?code=ACDE-FGHJ": "/en/login?next=%2Fen%2Flink%3Fcode%3DACDE-FGHJ",
	}
	for path, want := range cases {
		w := get(s, path)
		if w.Code != http.StatusFound {
			t.Errorf("%s: status %d, want 302", path, w.Code)
			continue
		}
		if got := w.Header().Get("Location"); got != want {
			t.Errorf("%s: Location %q, want %q", path, got, want)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control %q, want no-store", path, got)
		}
	}
}

// The browser API that reads the catalog answers 401 rather than redirecting —
// a fetch() cannot follow a redirect to a login page usefully.
func TestMembersOnlyCatalogAPIIsUnauthorized(t *testing.T) {
	w := get(gatedServer(t), "/api/catalog/cards?ids=1")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/api/catalog/cards: %d, want 401", w.Code)
	}
}

// The front door and the TV guide are the pages a signed-out visitor can see,
// and neither offers catalog nav that would only bounce back to /login.
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
	if strings.Contains(body, `href="/en/bookmarks"`) || strings.Contains(body, `href="/en/?type=movie"`) {
		t.Error("a signed-out visitor is shown catalog nav")
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

// A failed Google round trip lands back on the front door, with the reason and
// the original destination intact, rather than on a members-only page that
// would redirect again and lose both.
func TestAuthErrorReturnsToLogin(t *testing.T) {
	if got, want := authErrorURL("/en/titles/dune-42", "error"), "/en/login?login=error&next=%2Fen%2Ftitles%2Fdune-42"; got != want {
		t.Fatalf("authErrorURL = %q, want %q", got, want)
	}
	if got, want := authErrorURL("/vi/", "unverified"), "/vi/login?login=unverified"; got != want {
		t.Fatalf("authErrorURL = %q, want %q", got, want)
	}
}

// Without accounts there is no way through the gate, so it stands down (local
// dev only — NewServer refuses this in production) and /login is not routed.
func TestMembersOnlyGateNeedsAccounts(t *testing.T) {
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

// Production without sign-in would publish the whole catalog to anyone, so it
// is refused at boot rather than discovered later.
func TestProductionRequiresAccounts(t *testing.T) {
	cfg := Config{
		PublicURL:          "https://phimnet.example",
		SessionSecret:      strings.Repeat("s", minSecretLen),
		SubtitleStorageDir: t.TempDir(),
	}
	if _, err := NewServer(nil, cfg); err == nil || !strings.Contains(err.Error(), "members-only") {
		t.Fatalf("NewServer without accounts in production: err = %v, want the members-only refusal", err)
	}
}

// resolutionLock is what the prepare endpoint enforces for both the web page
// and the TV app, so with accounts on an anonymous caller plays NOTHING — not
// even 720p — while a signed-in visitor keeps every tier they had.
func TestResolutionLockIsMembersOnly(t *testing.T) {
	gated := &Server{google: newGoogleClient("client-id", "secret", "http://localhost/cb")}
	anon := titleAccess{}
	member := titleAccess{signedIn: true}
	entitled := titleAccess{signedIn: true, unlimited: true}

	for _, res := range []string{"720p", "1080p", "2160p", ""} {
		if got := gated.resolutionLock(res, anon); got != lockMember {
			t.Errorf("anonymous %q = %q, want %q", res, got, lockMember)
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

	// Accounts off (local dev): the gate stands down, exactly as requireLogin does.
	open := &Server{}
	if got := open.resolutionLock("1080p", anon); got != lockNone {
		t.Errorf("accounts off, 1080p = %q, want playable", got)
	}
	if got := open.resolutionLock("2160p", anon); got != lockPaid {
		t.Errorf("accounts off, 2160p = %q, want %q", got, lockPaid)
	}
}

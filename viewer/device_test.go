package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// testSigner builds a signer from a caller-chosen secret, so a test can make
// two signers that genuinely disagree (i.e. a rotated SESSION_SECRET).
func testSigner(t *testing.T, secret string) *sessionSigner {
	t.Helper()
	signer, _, err := newSessionSigner(strings.Repeat(secret, minSecretLen), false)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func bearerRequest(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/tv/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestDeviceTokenRoundTrip(t *testing.T) {
	signer := testSigner(t, "k")
	token := signer.mintDeviceToken(42, 7)

	userID, deviceID, ok := signer.readBearer(bearerRequest(token))
	if !ok {
		t.Fatal("freshly minted token did not verify")
	}
	if userID != 42 || deviceID != 7 {
		t.Fatalf("got user %d device %d, want 42/7", userID, deviceID)
	}
}

// The two credentials share a secret and a MAC construction, so the ONLY thing
// keeping them apart is the payload version. If that ever stops holding, a
// stolen session cookie becomes a TV token that no device row can revoke — so
// assert the separation in both directions.
func TestDeviceTokenAndSessionCookieAreNotInterchangeable(t *testing.T) {
	signer := testSigner(t, "k")

	t.Run("session payload rejected as bearer", func(t *testing.T) {
		// Exactly what setSession signs, presented in the Authorization header.
		sessionPayload := signer.sign(fmt.Sprintf("v1:%d:%d", 42, time.Now().Add(time.Hour).Unix()))
		if _, _, ok := signer.readBearer(bearerRequest(sessionPayload)); ok {
			t.Fatal("a session cookie payload was accepted as a TV bearer token")
		}
	})

	t.Run("device payload rejected as session", func(t *testing.T) {
		token := signer.mintDeviceToken(42, 7)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
		if _, ok := signer.readSession(r); ok {
			t.Fatal("a TV bearer token was accepted as a session cookie")
		}
	})
}

func TestReadBearerRejectsBadTokens(t *testing.T) {
	signer := testSigner(t, "k")
	other := testSigner(t, "rotated") // a different key, i.e. a rotated SESSION_SECRET

	expired := signer.sign(fmt.Sprintf("v1tv:%d:%d:%d", 42, 7, time.Now().Add(-time.Minute).Unix()))
	tampered := signer.mintDeviceToken(42, 7)
	tampered = strings.Replace(tampered, "v1tv:42:", "v1tv:43:", 1)

	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"garbage", "not-a-token"},
		{"expired", expired},
		{"tampered user id", tampered},
		{"signed with another key", other.mintDeviceToken(42, 7)},
		{"zero user", signer.sign(fmt.Sprintf("v1tv:0:7:%d", time.Now().Add(time.Hour).Unix()))},
		{"zero device", signer.sign(fmt.Sprintf("v1tv:42:0:%d", time.Now().Add(time.Hour).Unix()))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := signer.readBearer(bearerRequest(tc.token)); ok {
				t.Fatalf("accepted %s token", tc.name)
			}
		})
	}
}

func TestReadBearerIgnoresNonBearerHeaders(t *testing.T) {
	signer := testSigner(t, "k")
	token := signer.mintDeviceToken(42, 7)

	for _, header := range []string{"", token, "Basic " + token, "Bearer", "Bearer "} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if _, _, ok := signer.readBearer(r); ok {
			t.Fatalf("accepted Authorization header %q", header)
		}
	}

	// The scheme is case-insensitive per RFC 7235, and real clients vary.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "bearer "+token)
	if _, _, ok := signer.readBearer(r); !ok {
		t.Fatal("rejected a lower-case bearer scheme")
	}
}

func TestUserCodeAlphabetIsUnambiguous(t *testing.T) {
	// The whole point of this alphabet is that a code read off a television
	// across a room transcribes correctly the first time, so every character
	// that is easily confused with another must stay out of it.
	for _, r := range "OI L015 8BSUV" {
		if r == ' ' {
			continue // the groups above are just for reading
		}
		if strings.ContainsRune(userCodeAlphabet, r) {
			t.Fatalf("ambiguous character %q is in the user-code alphabet", r)
		}
	}
	if len(userCodeAlphabet) < 20 {
		t.Fatalf("alphabet is down to %d characters — codes are getting guessable", len(userCodeAlphabet))
	}
}

func TestRandomUserCodeShape(t *testing.T) {
	for i := 0; i < 64; i++ {
		code, err := randomUserCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != userCodeLen {
			t.Fatalf("code %q has length %d, want %d", code, len(code), userCodeLen)
		}
		for _, r := range code {
			if !strings.ContainsRune(userCodeAlphabet, r) {
				t.Fatalf("code %q contains %q, which is outside the alphabet", code, r)
			}
		}
	}
}

func TestNormalizeUserCode(t *testing.T) {
	// Whatever the person types on their phone must resolve to what the
	// television is showing: the display hyphen, spaces and case are all noise.
	cases := map[string]string{
		"ACDE-FGHJ": "ACDEFGHJ",
		"acde-fghj": "ACDEFGHJ",
		"ACDE FGHJ": "ACDEFGHJ",
		" acdefghj": "ACDEFGHJ",
		"ACDEFGHJ":  "ACDEFGHJ",
	}
	for in, want := range cases {
		if got := normalizeUserCode(in); got != want {
			t.Fatalf("normalizeUserCode(%q) = %q, want %q", in, got, want)
		}
	}

	// Characters outside the alphabet are dropped rather than passed through,
	// so a rejected code can never reach the query as a partial match.
	if got := normalizeUserCode("ACDE-FGH!"); got != "ACDEFGH" {
		t.Fatalf("normalizeUserCode did not drop an out-of-alphabet character: %q", got)
	}
}

func TestFormatUserCode(t *testing.T) {
	if got := formatUserCode("ACDEFGHJ"); got != "ACDE-FGHJ" {
		t.Fatalf("formatUserCode = %q, want ACDE-FGHJ", got)
	}
	// A wrong-length code is returned untouched rather than mangled.
	if got := formatUserCode("ACD"); got != "ACD" {
		t.Fatalf("formatUserCode mangled a short code: %q", got)
	}
}

// A code the user typed must survive the display round trip unchanged.
func TestUserCodeRoundTripsThroughDisplay(t *testing.T) {
	for i := 0; i < 32; i++ {
		code, err := randomUserCode()
		if err != nil {
			t.Fatal(err)
		}
		if got := normalizeUserCode(formatUserCode(code)); got != code {
			t.Fatalf("round trip changed %q to %q", code, got)
		}
	}
}

// routesOf walks a configured router and returns "METHOD /pattern" for every
// registered route, so a test can assert on registration without a database.
func routesOf(t *testing.T, s *Server) map[string]bool {
	t.Helper()
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	s.setupRouter()
	found := map[string]bool{}
	err := chi.Walk(s.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		found[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// The TV read API is always registered — browsing without an account is
// supported and the app degrades to 720p exactly as an anonymous browser does.
// The PAIRING routes follow /auth/google/*: without accounts there is nothing to
// pair to, and registering them would dangle a flow that could only ever fail.
//
// This is the documented rollback (GOOGLE_CLIENT_ID=""), so pin it.
func TestTVRouteRegistration(t *testing.T) {
	readRoutes := []string{
		"GET /api/tv/v1/home",
		"GET /api/tv/v1/titles",
		"GET /api/tv/v1/titles/{id}",
		"GET /api/tv/v1/genres",
		"GET /api/tv/v1/watch/movie/{id}",
		"GET /api/tv/v1/watch/episode/{id}",
		"GET /api/tv/v1/me",
	}
	pairingRoutes := []string{
		"POST /api/tv/v1/device/code",
		"POST /api/tv/v1/device/token",
		"POST /api/tv/v1/device/approve",
		"POST /api/tv/v1/device/logout",
		"GET /api/tv/v1/devices",
		"DELETE /api/tv/v1/devices/{id}",
	}

	t.Run("accounts off", func(t *testing.T) {
		found := routesOf(t, &Server{})
		for _, route := range readRoutes {
			if !found[route] {
				t.Errorf("%s should be registered without accounts, but is not", route)
			}
		}
		for _, route := range pairingRoutes {
			if found[route] {
				t.Errorf("%s must NOT be registered without accounts", route)
			}
		}
		// The page lives in the shared locale subtree, so chi reports it once
		// under the {locale} pattern rather than per language.
		if found["GET /{locale}/link"] {
			t.Error("/{locale}/link must NOT be registered without accounts")
		}
	})

	t.Run("accounts on", func(t *testing.T) {
		found := routesOf(t, &Server{google: newGoogleClient("client-id", "secret", "http://localhost/cb")})
		for _, route := range append(append([]string{}, readRoutes...), pairingRoutes...) {
			if !found[route] {
				t.Errorf("%s should be registered with accounts, but is not", route)
			}
		}
		if !found["GET /{locale}/link"] {
			t.Error("/{locale}/link should be registered with accounts, but is not")
		}
	})
}

// The QR code the television shows opens /link with ?code= already set. The page
// must pre-fill the NORMALISED code — and anything that is not a well-formed code
// must be dropped rather than echoed, since it arrives from a query string.
func TestLinkPagePrefillsScannedCode(t *testing.T) {
	s := &Server{google: newGoogleClient("client-id", "secret", "http://localhost/cb")}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}

	// codeInput returns just the <input id="link-code" ...> tag. The rest of the
	// page has its own value= attributes (the signed-in header's logout form), so
	// asserting on the whole body would be testing the layout, not this input.
	codeInput := func(query string) string {
		t.Helper()
		// Signed in, so the form (and its input) actually renders.
		r := httptest.NewRequest(http.MethodGet, "/vi/link"+query, nil)
		r = r.WithContext(context.WithValue(r.Context(), userCtxKey, &User{ID: 1}))
		w := httptest.NewRecorder()
		s.handleLinkPage(w, r)
		body := w.Body.String()
		start := strings.Index(body, `id="link-code"`)
		if start < 0 {
			t.Fatalf("query %q: code input not rendered", query)
		}
		end := strings.Index(body[start:], ">")
		return body[start : start+end]
	}

	if in := codeInput("?code=acde-fghj"); !strings.Contains(in, `value="ACDE-FGHJ"`) {
		t.Fatalf("a scanned code was not pre-filled in normalised form: %s", in)
	}
	for _, junk := range []string{"?code=ACD", `?code="><script>`, "?code=OOOO-0000", ""} {
		if in := codeInput(junk); strings.Contains(in, "value=") {
			t.Fatalf("query %q pre-filled something it should have dropped: %s", junk, in)
		}
	}
}

// The pairing link is shown on a television and encoded in a QR code for a phone,
// so it must be absolute — even on a deploy without VIEWER_PUBLIC_URL, where the
// SEO helper s.abs deliberately returns a bare path. (Found on the emulator: the
// TV displayed "/en/link", which no phone could open.)
func TestPairingLinkIsAlwaysAbsolute(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://tv-host.example:8082/api/tv/v1/device/code", nil)

	unset := &Server{}
	if got, want := unset.absoluteFor(r, "/en/link"), "http://tv-host.example:8082/en/link"; got != want {
		t.Fatalf("without a public URL: got %q, want %q", got, want)
	}

	r.Header.Set("X-Forwarded-Proto", "https")
	if got, want := unset.absoluteFor(r, "/en/link"), "https://tv-host.example:8082/en/link"; got != want {
		t.Fatalf("behind a TLS proxy: got %q, want %q", got, want)
	}

	configured := &Server{publicURL: "https://phimnet.online"}
	if got, want := configured.absoluteFor(r, "/en/link"), "https://phimnet.online/en/link"; got != want {
		t.Fatalf("with a public URL: got %q, want %q", got, want)
	}
}

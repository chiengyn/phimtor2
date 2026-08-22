package main

// Signed-cookie sessions.
//
// There is deliberately no sessions table: the cookie IS the session. It carries
// the user id plus an expiry, authenticated with HMAC-SHA256 over SESSION_SECRET,
// so a logged-in page view costs one user lookup and no session read. The
// trade-off is that the only way to revoke everything is to rotate
// SESSION_SECRET, which logs every user out at once.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookieName = "phimnet_session"
	stateCookieName   = "phimnet_oauth"

	sessionTTL = 30 * 24 * time.Hour
	stateTTL   = 10 * time.Minute

	// minSecretLen is the shortest SESSION_SECRET we accept, matching the
	// `openssl rand -hex 32` the docs prescribe.
	minSecretLen = 32
)

// sessionSigner mints and verifies the two signed cookies this service uses: the
// long-lived session cookie and the short-lived OAuth state cookie.
type sessionSigner struct {
	key []byte
	// secure sets the Secure cookie attribute. Off in local dev, where the site
	// is served over plain http and browsers would silently drop the cookie.
	secure bool
}

// newSessionSigner builds the signer from the configured secret. An empty secret
// is allowed only in local dev (secure == false), where it generates an
// ephemeral key so sign-in works with no setup — sessions then do not survive a
// restart, which the caller warns about.
func newSessionSigner(secret string, secure bool) (*sessionSigner, bool, error) {
	if secret == "" {
		if secure {
			return nil, false, errors.New("SESSION_SECRET is required when VIEWER_PUBLIC_URL is set (generate one with: openssl rand -hex 32)")
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, false, err
		}
		return &sessionSigner{key: key, secure: secure}, true, nil
	}
	if len(secret) < minSecretLen {
		return nil, false, fmt.Errorf("SESSION_SECRET must be at least %d characters (generate one with: openssl rand -hex 32)", minSecretLen)
	}
	return &sessionSigner{key: []byte(secret), secure: secure}, false, nil
}

// sign returns "<payload>.<base64url(mac)>".
func (s *sessionSigner) sign(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify checks the MAC in constant time and returns the payload. It splits on
// the LAST dot, because a payload may itself contain dots (the OAuth state
// cookie carries a return path).
func (s *sessionSigner) verify(token string) (string, bool) {
	i := strings.LastIndexByte(token, '.')
	if i <= 0 {
		return "", false
	}
	payload, sig := token[:i], token[i+1:]
	want := hmac.New(sha256.New, s.key)
	want.Write([]byte(payload))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return "", false
	}
	if subtle.ConstantTimeCompare(got, want.Sum(nil)) != 1 {
		return "", false
	}
	return payload, true
}

// setCookie writes one of our signed cookies. SameSite=Lax is required rather
// than Strict: the OAuth callback arrives as a top-level cross-site GET
// navigation from accounts.google.com, and Strict would withhold the state
// cookie there so every login would fail. Lax still withholds cookies from
// cross-site POSTs, which is what protects the bookmark write API.
func (s *sessionSigner) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl / time.Second),
	})
}

func (s *sessionSigner) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// setSession logs the user in for sessionTTL.
func (s *sessionSigner) setSession(w http.ResponseWriter, userID int64) {
	payload := fmt.Sprintf("v1:%d:%d", userID, time.Now().Add(sessionTTL).Unix())
	s.setCookie(w, sessionCookieName, s.sign(payload), sessionTTL)
}

func (s *sessionSigner) clearSession(w http.ResponseWriter) {
	s.clearCookie(w, sessionCookieName)
}

// readSession returns the user id carried by a valid, unexpired session cookie.
// The MAC is verified BEFORE the payload is parsed — unauthenticated bytes are
// never interpreted.
func (s *sessionSigner) readSession(r *http.Request) (int64, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return 0, false
	}
	payload, ok := s.verify(c.Value)
	if !ok {
		return 0, false
	}
	parts := strings.Split(payload, ":")
	if len(parts) != 3 || parts[0] != "v1" {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return 0, false
	}
	return id, true
}

// setState stores the OAuth CSRF nonce together with the path to return to after
// login. Keeping the return path in the (signed, HttpOnly) cookie rather than in
// the state query parameter means it never round-trips through Google, so there
// is no open-redirect surface in the URL we hand out.
func (s *sessionSigner) setState(w http.ResponseWriter, nonce, next string) {
	payload := fmt.Sprintf("v1:%s:%d:%s", nonce, time.Now().Add(stateTTL).Unix(), next)
	s.setCookie(w, stateCookieName, s.sign(payload), stateTTL)
}

func (s *sessionSigner) clearState(w http.ResponseWriter) {
	s.clearCookie(w, stateCookieName)
}

// readState returns the nonce and return path from a valid, unexpired state
// cookie. The path is split into 4 so a return path containing ":" survives.
func (s *sessionSigner) readState(r *http.Request) (nonce, next string, ok bool) {
	c, err := r.Cookie(stateCookieName)
	if err != nil {
		return "", "", false
	}
	payload, ok := s.verify(c.Value)
	if !ok {
		return "", "", false
	}
	parts := strings.SplitN(payload, ":", 4)
	if len(parts) != 4 || parts[0] != "v1" {
		return "", "", false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return "", "", false
	}
	return parts[1], parts[3], true
}

// randomToken returns n bytes of crypto-random data, URL-safe base64 encoded.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// safeNext sanitises a post-login return path. Only a site-relative path is
// allowed: anything absolute, protocol-relative ("//evil.com", which a browser
// treats as a host) or over-long falls back to the home page. This is the
// open-redirect guard.
func safeNext(next string) string {
	if next == "" || len(next) > 512 || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	// A backslash is normalised to "/" by some browsers, so "/\evil.com" would
	// escape the site the same way "//evil.com" does.
	if strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return next
}

// secureEqual compares two tokens in constant time, so a mismatch reveals
// nothing about how far it matched.
func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

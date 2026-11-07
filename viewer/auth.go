package main

// Request-scoped identity: the middleware that turns a session cookie into a
// *User on the context, the gate for the write APIs, and the sign-in / sign-out
// HTTP handlers.

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// ctxKey is a private type so nothing outside this file can collide with our
// context keys.
type ctxKey int

const (
	userCtxKey ctxKey = iota
	// deviceCtxKey carries the tv_devices id when — and only when — the request
	// authenticated with a TV bearer token. A cookie request never has one, which
	// is what lets handleDeviceLogout tell "this television" from "some browser".
	deviceCtxKey
)

// userFrom returns the signed-in user for this request, or nil for an anonymous
// visitor. Safe to call from any handler behind the currentUser middleware.
func userFrom(ctx context.Context) *User {
	u, _ := ctx.Value(userCtxKey).(*User)
	return u
}

// deviceFrom returns the paired television this request came from, or 0 when it
// was not a TV bearer request (a browser, or anonymous).
func deviceFrom(ctx context.Context) int64 {
	id, _ := ctx.Value(deviceCtxKey).(int64)
	return id
}

// currentUser resolves the caller's identity and, when there is one, loads the
// user (and their saved-title set) onto the request context. It is applied
// GLOBALLY so every page can render its own header state.
//
// Two credentials are accepted, in this order:
//
//  1. the signed session cookie — every browser request, and
//  2. an Authorization: Bearer token — the Android TV client.
//
// They are the same HMAC construction over the same SESSION_SECRET, differing
// only in payload version and in how they travel, so everything downstream
// (requireUser, accessForTitle, resolutionLock) is identical for both and the
// entitlement rules have exactly one implementation.
//
// Anonymous requests — the overwhelming majority, including all crawler traffic
// — carry neither and return before touching the database, so the public site
// pays nothing for this.
//
// A cookie that no longer resolves to a row (user deleted or blocked, or
// SESSION_SECRET rotated) is cleared, so a stale cookie costs one lookup rather
// than one per page for the next 30 days.
func (s *Server) currentUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.sess == nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		id, ok := s.sess.readSession(r)
		if !ok {
			// No cookie: this may still be the TV client. The bearer path costs
			// one extra lookup (the pairing row) that the cookie path does not,
			// which is the price of being able to revoke one television without
			// rotating the secret for everybody.
			var deviceID int64
			id, deviceID, ok = s.deviceUser(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			ctx = context.WithValue(ctx, deviceCtxKey, deviceID)
		}
		user, err := s.store.UserByID(r.Context(), id)
		if err != nil {
			// A DB error must not blank the page: serve it anonymously. This is
			// also what keeps the site usable if the viewer is ever deployed
			// ahead of the migration that creates the users table.
			log.Printf("currentUser: load %d: %v", id, err)
			next.ServeHTTP(w, r)
			return
		}
		if user == nil {
			s.sess.clearSession(w)
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, userCtxKey, user)))
	})
}

// deviceUser resolves an Authorization: Bearer token to a user id, verifying
// that the television it was issued to is still paired.
//
// That second check is the entire reason tv_devices exists. A session cookie is
// stateless by design (see session.go) and the only way to revoke one is to
// rotate SESSION_SECRET, which signs every user out on every device. That trade
// is fine for a browser somebody controls and wrong for a television in a shared
// room, so a TV token names its pairing row and pays one indexed lookup per
// request to make "sign out this TV" a real lever.
//
// A store error degrades to anonymous and is logged, exactly as the cookie path
// does: a database blip must not hand out access, but it must not 500 either.
func (s *Server) deviceUser(r *http.Request) (userID, deviceID int64, ok bool) {
	userID, deviceID, ok = s.sess.readBearer(r)
	if !ok {
		return 0, 0, false
	}
	active, err := s.store.DeviceActive(r.Context(), deviceID, userID)
	if err != nil {
		log.Printf("deviceUser: device %d: %v", deviceID, err)
		return 0, 0, false
	}
	if !active {
		return 0, 0, false
	}
	return userID, deviceID, true
}

// requireLogin is the members-only gate in front of every catalog page and the
// browser APIs that read the catalog. A page redirects to the localized /login,
// carrying where the visitor was headed; an API answers 401 JSON, exactly as
// requireUser does.
//
// With accounts off it stands down: there would be no way through it. NewServer
// refuses that config in production, so this only ever opens a laptop.
func (s *Server) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r.Context()) != nil || !s.google.enabled() {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// The redirect depends on the cookie, so no cache may replay it to a
		// visitor who has since signed in.
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, loginPageURL(localeFromContext(r.Context()), r.URL.RequestURI(), ""), http.StatusFound)
	})
}

// loginPageURL is the localized front door, remembering where to go afterwards
// and, after a failed attempt, why it failed. A next of the locale's own home
// is left off, so the plain front door keeps a clean URL.
func loginPageURL(locale Locale, next, reason string) string {
	values := url.Values{}
	if next = safeNext(next); next != "/" && next != localeHome(locale) {
		values.Set("next", next)
	}
	if reason != "" {
		values.Set("login", reason)
	}
	return localeQueryURL(locale, "/login", values)
}

// loginData is the view model for templates/login.html.
type loginData struct {
	// GoogleURL starts the OAuth flow and returns to Next afterwards.
	GoogleURL string
	// Error is the localized reason the last attempt failed, or "".
	Error string
}

// handleLoginPage is the front door of the members-only site. There is no
// separate registration: a first Google sign-in creates the account
// (UpsertGoogleUser), so the one button serves both.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	locale := localeFromContext(r.Context())
	next := safeNext(r.URL.Query().Get("next"))
	if next == "/" {
		next = localeHome(locale)
	}
	if userFrom(r.Context()) != nil {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	data := loginData{GoogleURL: "/auth/google/start?next=" + url.QueryEscape(next)}
	switch r.URL.Query().Get("login") {
	case "error":
		data.Error = tr(locale, "login.error")
	case "unverified":
		data.Error = tr(locale, "login.unverified")
	case "blocked":
		data.Error = tr(locale, "login.blocked")
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, s.login, data)
}

// requireUser gates the write APIs. Read pages use requireLogin instead, which
// redirects a signed-out visitor to /login rather than answering 401.
func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r.Context()) == nil {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleGoogleStart kicks off the OAuth flow: mint a CSRF nonce, remember it
// (and where to come back to) in a short-lived signed cookie, then bounce the
// browser to Google's consent screen.
func (s *Server) handleGoogleStart(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if userFrom(r.Context()) != nil { // already signed in
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	nonce, err := randomToken(32)
	if err != nil {
		log.Printf("auth: nonce: %v", err)
		http.Redirect(w, r, authErrorURL(next, "error"), http.StatusFound)
		return
	}
	s.sess.setState(w, nonce, next)
	http.Redirect(w, r, s.google.authCodeURL(nonce), http.StatusFound)
}

// handleGoogleCallback completes the flow. Every failure path lands the visitor
// back on /login with ?login=error rather than on an error page — a failed
// login should never be a dead end — and never echoes anything Google returned.
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	nonce, next, ok := s.sess.readState(r)
	s.sess.clearState(w) // single use, whatever happens next

	if !ok || nonce == "" || !secureEqual(nonce, r.URL.Query().Get("state")) {
		log.Printf("auth: state mismatch from %s", r.RemoteAddr)
		http.Redirect(w, r, authErrorURL(next, "error"), http.StatusFound)
		return
	}
	next = safeNext(next)

	// The user pressed "Cancel" on the consent screen. Not an error — just put
	// them back where they were.
	if r.URL.Query().Get("error") != "" {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, authErrorURL(next, "error"), http.StatusFound)
		return
	}

	id, err := s.google.exchange(r.Context(), code)
	if err != nil {
		log.Printf("auth: exchange: %v", err)
		http.Redirect(w, r, authErrorURL(next, "error"), http.StatusFound)
		return
	}
	// Google verifies email for accounts.google.com logins, but be explicit: an
	// unverified address must never become an identity we might later key a
	// payment or a support request on.
	if !id.EmailVerified {
		log.Printf("auth: unverified email for sub %s", id.Sub)
		http.Redirect(w, r, authErrorURL(next, "unverified"), http.StatusFound)
		return
	}

	user, err := s.store.UpsertGoogleUser(r.Context(), id)
	if err != nil {
		log.Printf("auth: upsert: %v", err)
		http.Redirect(w, r, authErrorURL(next, "error"), http.StatusFound)
		return
	}
	// The upsert reads the row back through UserByID, which hides a blocked
	// account (is_blocked, set by the admin on /users). Say so plainly rather
	// than issuing a session that the very next request would discard.
	if user == nil {
		log.Printf("auth: blocked account signed in (sub %s)", id.Sub)
		http.Redirect(w, r, authErrorURL(next, "blocked"), http.StatusFound)
		return
	}
	s.sess.setSession(w, user.ID)
	http.Redirect(w, r, next, http.StatusFound)
}

func authErrorURL(next, reason string) string {
	next = safeNext(next)
	return loginPageURL(localeFromRequestPath(next), next, reason)
}

// handleLogout clears the session. POST-only (the header renders it as a form)
// so a prefetched or embedded GET cannot sign anyone out, and SameSite=Lax keeps
// the cookie off cross-site POSTs.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.sess != nil {
		s.sess.clearSession(w)
	}
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

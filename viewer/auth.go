package main

// Request-scoped identity: the middleware that turns a session cookie into a
// *User on the context, the gate for the write APIs, and the sign-in / sign-out
// HTTP handlers.

import (
	"context"
	"log"
	"net/http"
)

// ctxKey is a private type so nothing outside this file can collide with our
// context keys.
type ctxKey int

const userCtxKey ctxKey = iota

// userFrom returns the signed-in user for this request, or nil for an anonymous
// visitor. Safe to call from any handler behind the currentUser middleware.
func userFrom(ctx context.Context) *User {
	u, _ := ctx.Value(userCtxKey).(*User)
	return u
}

// currentUser decodes the signed session cookie and, when it is valid, loads the
// user (and their saved-title set) onto the request context. It is applied
// GLOBALLY so every page can render its own header state.
//
// Anonymous requests — the overwhelming majority, including all crawler traffic
// — carry no cookie and return before touching the database, so the public site
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
		id, ok := s.sess.readSession(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
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
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, user)))
	})
}

// requireUser gates the write APIs. Read pages never use it — they degrade to
// the anonymous rendering instead of redirecting.
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
		http.Redirect(w, r, "/?login=error", http.StatusFound)
		return
	}
	s.sess.setState(w, nonce, next)
	http.Redirect(w, r, s.google.authCodeURL(nonce), http.StatusFound)
}

// handleGoogleCallback completes the flow. Every failure path lands the visitor
// back on the site with ?login=error rather than on an error page — a failed
// login should never be a dead end — and never echoes anything Google returned.
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	nonce, next, ok := s.sess.readState(r)
	s.sess.clearState(w) // single use, whatever happens next

	if !ok || nonce == "" || !secureEqual(nonce, r.URL.Query().Get("state")) {
		log.Printf("auth: state mismatch from %s", r.RemoteAddr)
		http.Redirect(w, r, "/?login=error", http.StatusFound)
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
		http.Redirect(w, r, "/?login=error", http.StatusFound)
		return
	}

	id, err := s.google.exchange(r.Context(), code)
	if err != nil {
		log.Printf("auth: exchange: %v", err)
		http.Redirect(w, r, "/?login=error", http.StatusFound)
		return
	}
	// Google verifies email for accounts.google.com logins, but be explicit: an
	// unverified address must never become an identity we might later key a
	// payment or a support request on.
	if !id.EmailVerified {
		log.Printf("auth: unverified email for sub %s", id.Sub)
		http.Redirect(w, r, "/?login=unverified", http.StatusFound)
		return
	}

	user, err := s.store.UpsertGoogleUser(r.Context(), id)
	if err != nil {
		log.Printf("auth: upsert: %v", err)
		http.Redirect(w, r, "/?login=error", http.StatusFound)
		return
	}
	s.sess.setSession(w, user.ID)
	http.Redirect(w, r, next, http.StatusFound)
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

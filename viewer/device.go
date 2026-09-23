package main

// Device-code pairing for televisions.
//
// A TV has no keyboard worth typing a password on and no browser to hand an
// OAuth callback to, so it cannot run the Google redirect flow in auth.go.
// Instead it shows a short code, the user approves that code on a phone that is
// already signed in, and the TV trades its long code for a bearer token.
//
// The shape follows RFC 8628 (the OAuth 2.0 Device Authorization Grant) —
// including its error strings, authorization_pending / slow_down /
// expired_token — because it is a protocol TV clients and their authors already
// know. It is NOT an OAuth server: no Google token ever comes near the
// television, which is deliberate. googleauth.go's decodeIDToken skips signature
// verification (sound only because exchange fetches the token server-side over
// TLS), and any flow that let a device present a Google token to us would turn
// that shortcut into an auth bypass. Pairing sidesteps the question entirely.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	// devicePairingTTL is how long an unapproved code lives. Long enough to find
	// your phone, short enough that a code read off a screen is not left standing.
	devicePairingTTL = 15 * time.Minute

	// devicePollInterval is what we ask the TV to wait between token polls. It is
	// advisory (the client obeys it), and the rate limiter is what enforces it.
	devicePollInterval = 5 * time.Second

	// userCodeLen is the length of the short code, excluding the separator.
	userCodeLen = 8

	// userCodeAlphabet omits every character pair that is ambiguous on a
	// television across a room: no O/0, no I/1/L, no S/5, no B/8, no U/V. What is
	// left is read correctly the first time, which matters more here than
	// entropy — the code is short-lived, single-use and rate limited, and the
	// long device_code is the actual credential.
	userCodeAlphabet = "ACDEFGHJKMNPQRTWXY34679"

	// userCodeAttempts is how many times we retry on a unique-key collision
	// before giving up. Collisions are expected (small alphabet, short code) and
	// ordinary — see CreateDevicePairing.
	userCodeAttempts = 8
)

// deviceApprovalsPerHour bounds guessing at the short code. It is keyed by
// ACCOUNT, not by code: an attacker must be signed in to submit a guess at all,
// so this caps how fast any one account can sweep the code space.
const deviceApprovalsPerHour = 20

// randomUserCode returns a short, human-transcribable code from the unambiguous
// alphabet. It reuses randomToken's crypto/rand source rather than math/rand —
// the code is low-entropy by design, but it must still be unguessable within its
// 15-minute life.
func randomUserCode() (string, error) {
	raw, err := randomToken(userCodeLen)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i := 0; i < userCodeLen; i++ {
		b.WriteByte(userCodeAlphabet[int(raw[i])%len(userCodeAlphabet)])
	}
	return b.String(), nil
}

// normalizeUserCode makes the phone-side input forgiving: case, spaces and the
// display hyphen are all stripped before lookup, so "abcd-efgh", "ABCD EFGH"
// and "ABCDEFGH" are the same code.
func normalizeUserCode(in string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(in)) {
		if strings.ContainsRune(userCodeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// formatUserCode groups the code for display, so it reads in two chunks from
// across a room.
func formatUserCode(code string) string {
	if len(code) != userCodeLen {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// handleDeviceCode opens a pairing. Anonymous by design — the whole point is
// that the television has no identity yet.
func (s *Server) handleDeviceCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceName string `json:"device_name"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body)
	name := strings.TrimSpace(body.DeviceName)
	if len(name) > 128 {
		name = name[:128]
	}

	deviceCode, err := randomToken(32)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not start pairing")
		return
	}

	// Retry on a duplicate short code. The alphabet is small on purpose, so a
	// collision is an ordinary outcome under concurrency rather than a failure —
	// the same reasoning (and the same isDuplicateKey helper) as the billing
	// amount reservation.
	var userCode string
	for attempt := 0; attempt < userCodeAttempts; attempt++ {
		userCode, err = randomUserCode()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not start pairing")
			return
		}
		_, err = s.store.CreateDevicePairing(r.Context(), deviceCode, userCode, name, devicePairingTTL)
		if err == nil {
			break
		}
		if !isDuplicateKey(err) {
			log.Printf("device: create pairing: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "could not start pairing")
			return
		}
	}
	if err != nil {
		log.Printf("device: exhausted user code attempts")
		writeJSONError(w, http.StatusServiceUnavailable, "could not start pairing")
		return
	}

	verification := s.absoluteFor(r, localeURL(tvLocale(r), "/link"))
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":      deviceCode,
		"user_code":        formatUserCode(userCode),
		"verification_uri": verification,
		// verification_uri_complete is RFC 8628's "the code is already in the
		// link" variant. The TV renders it as a QR code, so scanning it lands on
		// /link with the box pre-filled — the person still has to press Connect,
		// which is the confirmation step §3.3.1 asks for. Without it, a QR code
		// saves nothing over reading the URL off the screen.
		"verification_uri_complete": verification + "?code=" + url.QueryEscape(formatUserCode(userCode)),
		"expires_in":                int(devicePairingTTL.Seconds()),
		"interval":                  int(devicePollInterval.Seconds()),
	})
}

// absoluteFor builds an ABSOLUTE URL for a path, even when VIEWER_PUBLIC_URL is
// unset. s.abs deliberately falls back to a bare path there, which is right for
// the SEO tags it serves and useless here: the pairing link is read off a
// television and scanned as a QR code by a phone, and neither can do anything
// with "/en/link".
//
// Without a configured origin it describes the host the television itself just
// reached. Trusting Host and X-Forwarded-Proto is safe for this one purpose: the
// URL only ever goes back to the same caller, which already knows the address.
func (s *Server) absoluteFor(r *http.Request, path string) string {
	if s.publicURL != "" {
		return s.abs(path)
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

// handleDeviceToken is the TV's poll. It answers with RFC 8628's error strings
// so the client can tell "keep waiting" from "start over".
//
// Every not-yet-approved outcome returns the same authorization_pending, and an
// unknown code is indistinguishable from an expired one: neither tells a caller
// anything about codes it does not hold.
func (s *Server) handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body); err != nil || body.DeviceCode == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	pairing, err := s.store.DevicePairing(r.Context(), body.DeviceCode)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if pairing == nil || pairing.Status == "revoked" {
		writeJSONError(w, http.StatusBadRequest, "expired_token")
		return
	}
	if pairing.Status == "pending" {
		if pairing.Expired {
			writeJSONError(w, http.StatusBadRequest, "expired_token")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "authorization_pending")
		return
	}

	// Approved. Mint the bearer token naming this pairing row, so the grant can
	// later be revoked on its own without touching anybody else's session.
	user, err := s.store.UserByID(r.Context(), pairing.UserID)
	if err != nil || user == nil {
		writeJSONError(w, http.StatusBadRequest, "expired_token")
		return
	}
	if err := s.store.TouchDevice(r.Context(), pairing.ID); err != nil {
		log.Printf("device: touch %d: %v", pairing.ID, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": s.sess.mintDeviceToken(user.ID, pairing.ID),
		"token_type":   "Bearer",
		"expires_in":   int(deviceTokenTTL.Seconds()),
		"user": map[string]any{
			"name":   user.DisplayName(),
			"email":  user.Email,
			"avatar": user.AvatarURL,
		},
	})
}

// handleDeviceApprove binds a code to the caller's account. Behind requireUser:
// the phone doing the approving is the thing that holds the identity.
func (s *Server) handleDeviceApprove(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if !s.deviceApprovals.allow(rateKey(u)) {
		writeJSONError(w, http.StatusTooManyRequests, tr(tvLocale(r), "link.too_many"))
		return
	}
	var body struct {
		UserCode string `json:"user_code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	code := normalizeUserCode(body.UserCode)
	if len(code) != userCodeLen {
		writeJSONError(w, http.StatusBadRequest, tr(tvLocale(r), "link.bad_code"))
		return
	}
	ok, err := s.store.ApproveDevicePairing(r.Context(), code, u.ID)
	if err != nil {
		log.Printf("device: approve: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "could not link device")
		return
	}
	if !ok {
		// Unknown, already used or expired all collapse to one answer — there is
		// no useful distinction for the person holding the phone, and telling a
		// guesser which of the three they hit would help them.
		writeJSONError(w, http.StatusNotFound, tr(tvLocale(r), "link.bad_code"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"linked": true})
}

// handleDeviceList and handleDeviceRevoke back the account page's "your
// televisions" section. Revocation is scoped by user id in the query itself, so
// a forged device id can only ever unpair one of the caller's own.
func (s *Server) handleDeviceList(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	devices, err := s.store.ListDevices(r.Context(), u.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not load devices")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func (s *Server) handleDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid device id")
		return
	}
	if err := s.store.RevokeDevice(r.Context(), id, u.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not unlink device")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeviceLogout is the television signing ITSELF out. It revokes the
// pairing row the caller's own bearer token names, so the token stops working
// immediately rather than living out its 180 days on a device someone has
// "signed out" of.
//
// Without this, sign-out on the TV could only forget the token locally, and the
// grant would stay live until somebody found the television in their account
// page. Only a bearer request carries a device id, so a browser session calling
// this gets a 400 rather than silently revoking nothing.
func (s *Server) handleDeviceLogout(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	deviceID := deviceFrom(r.Context())
	if deviceID == 0 {
		writeJSONError(w, http.StatusBadRequest, "not a device session")
		return
	}
	if err := s.store.RevokeDevice(r.Context(), deviceID, u.ID); err != nil {
		log.Printf("device: logout %d: %v", deviceID, err)
		writeJSONError(w, http.StatusInternalServerError, "could not sign out")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// linkData is the /link page's view model. It carries LoginURL itself rather
// than reading it off the pageData envelope, for the reason spelled out on
// watchData: layout.html invokes the body as {{block "content" .Data}}, so
// neither .User nor $.LoginURL is in scope inside the page template.
type linkData struct {
	SignedIn bool
	LoginURL string
	// Code pre-fills the box when the visitor arrived by scanning the QR code
	// the television shows (verification_uri_complete). It is normalised and
	// re-formatted here, so whatever rides in the query string can only ever
	// render as characters from the code alphabet.
	Code string
}

// handleLinkPage is where someone types the code their television is showing.
//
// An anonymous visitor who scanned the QR code is sent through sign-in and back
// here; the pre-filled code survives that round trip because loginURL returns
// to the full request URI, query string included.
func (s *Server) handleLinkPage(w http.ResponseWriter, r *http.Request) {
	code := normalizeUserCode(r.URL.Query().Get("code"))
	if len(code) != userCodeLen {
		code = ""
	}
	s.render(w, r, s.link, linkData{
		SignedIn: userFrom(r.Context()) != nil,
		LoginURL: s.loginURL(r),
		Code:     formatUserCode(code),
	})
}

// devicePairingSweep is how often abandoned pairings are deleted. Far slower
// than the watch-session sweep because nothing breaks if an expired code lingers
// a few minutes: it is already unusable (every query guards on expires_at), and
// the only cost of keeping it is that it occupies one code in a small alphabet.
const devicePairingSweep = 10 * time.Minute

// reapDevicePairings deletes pending pairings nobody ever approved, until ctx is
// cancelled. Started from main alongside the watch-session sweeper, and only
// when accounts are configured — with no accounts, no pairing is ever created.
func (s *Server) reapDevicePairings(ctx context.Context) {
	if s.google == nil || !s.google.enabled() {
		return
	}
	ticker := time.NewTicker(devicePairingSweep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.store.ReapDevicePairings(ctx); err != nil {
				log.Printf("device: reap pairings: %v", err)
			}
		}
	}
}

package main

// Google sign-in (OAuth2 authorization-code flow + OIDC).
//
// Hand-written rather than pulling in golang.org/x/oauth2, matching how every
// other external service in this repo is talked to (manager.go here, tmdb.go /
// opensubtitles.go in admin). We never call a Google API after login, so there
// is no access token to refresh and nothing to store — the ID token is decoded
// once for its claims and discarded.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleScopes   = "openid email profile"
)

// googleClient is the server-side half of the sign-in flow. A zero value (empty
// clientID) means accounts are disabled and the /auth routes are not registered.
type googleClient struct {
	clientID     string
	clientSecret string
	// redirectURL must match an "Authorized redirect URI" in the Google Cloud
	// Console byte for byte — scheme, host, port, path, no trailing slash.
	// A mismatch is by far the most common setup failure (redirect_uri_mismatch).
	redirectURL string
	http        *http.Client
}

func newGoogleClient(clientID, clientSecret, redirectURL string) *googleClient {
	return &googleClient{
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURL:  redirectURL,
		http:         &http.Client{Timeout: 10 * time.Second},
	}
}

func (g *googleClient) enabled() bool { return g != nil && g.clientID != "" }

// authCodeURL builds the consent-screen redirect. state is the opaque nonce we
// also store (signed) in a cookie, and is the CSRF defence for the callback.
func (g *googleClient) authCodeURL(state string) string {
	q := url.Values{
		"client_id":     {g.clientID},
		"redirect_uri":  {g.redirectURL},
		"response_type": {"code"},
		"scope":         {googleScopes},
		"state":         {state},
		// We never call Google APIs on the user's behalf, so we want no refresh
		// token at all.
		"access_type": {"online"},
		// Always let the user pick which Google account, rather than silently
		// reusing whichever one the browser is already signed into.
		"prompt": {"select_account"},
	}
	return googleAuthURL + "?" + q.Encode()
}

// googleIdentity is the subset of the ID token claims we keep. Sub is the stable
// per-account identifier and is the real identity — Email can change.
type googleIdentity struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	Locale        string `json:"locale"`

	Aud string `json:"aud"`
	Iss string `json:"iss"`
	Exp int64  `json:"exp"`
}

// exchange trades the authorization code for an ID token and returns its claims.
func (g *googleClient) exchange(ctx context.Context, code string) (*googleIdentity, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"redirect_uri":  {g.redirectURL},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// Google echoes the client_id in error bodies, so log the status and the
		// short error code only — never the whole body — and never surface it.
		return nil, fmt.Errorf("google token endpoint: %s", resp.Status)
	}

	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("google token endpoint: decode: %w", err)
	}
	if tok.IDToken == "" {
		return nil, fmt.Errorf("google token endpoint: no id_token in response")
	}

	id, err := decodeIDToken(tok.IDToken)
	if err != nil {
		return nil, err
	}
	if id.Aud != g.clientID {
		return nil, fmt.Errorf("id token: aud mismatch")
	}
	if id.Iss != "accounts.google.com" && id.Iss != "https://accounts.google.com" {
		return nil, fmt.Errorf("id token: unexpected issuer %q", id.Iss)
	}
	if id.Exp <= time.Now().Unix() {
		return nil, fmt.Errorf("id token: expired")
	}
	if id.Sub == "" {
		return nil, fmt.Errorf("id token: no subject")
	}
	return id, nil
}

// decodeIDToken JSON-decodes the payload of a compact JWS WITHOUT verifying its
// signature.
//
// That is correct here, and ONLY here: OIDC Core 3.1.3.7 clause 6 permits
// skipping signature validation for an ID token fetched by the client itself
// directly from the token endpoint over a TLS-verified channel, which is exactly
// what exchange does. We still check aud/iss/exp above as cheap defence in depth.
//
// DANGER: if this is ever reused for a token that arrives from the BROWSER —
// Google One Tap, an implicit flow, a client-supplied credential — it becomes a
// complete authentication bypass, because anyone can mint an unsigned JWT. Any
// such change must verify the signature against Google's JWKS first.
func decodeIDToken(idToken string) (*googleIdentity, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("id token: malformed")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("id token: decode payload: %w", err)
	}
	var id googleIdentity
	if err := json.Unmarshal(payload, &id); err != nil {
		return nil, fmt.Errorf("id token: decode claims: %w", err)
	}
	return &id, nil
}

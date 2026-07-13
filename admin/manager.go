package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// errMetainfoNotReady means the owning streamer hasn't resolved the torrent's
// metadata yet, so there is nothing to persist — the harvest/fetch should retry.
var errMetainfoNotReady = errors.New("torrent metadata not resolved yet")

// maxMetainfoBytes bounds a .torrent read from the manager (they are small even
// for large season packs).
const maxMetainfoBytes = 32 << 20

// managerClient is the admin's server-to-server client for the streamer manager.
// The browser never calls the manager directly: it calls this admin server (same
// origin, behind basic auth), which proxies the control-plane ops to the manager.
// The manager returns each torrent's owning streamer public URL, which the
// browser then uses for stats/stream directly.
type managerClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newManagerClient(internalURL, token string) *managerClient {
	return &managerClient{
		baseURL: strings.TrimRight(internalURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *managerClient) authReq(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// proxy forwards the incoming request to the manager at managerPath, attaching
// the internal token, and copies the manager's status, content type, and body
// back to the client. Used for add/list/get/delete.
func (c *managerClient) proxy(w http.ResponseWriter, r *http.Request, managerPath string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, c.baseURL+managerPath, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	c.authReq(req)

	resp, err := c.http.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "manager unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// postBody POSTs a caller-constructed body to the manager at managerPath (with
// the internal token) and copies the manager's status, content type, and body
// back to the client. Unlike proxy — which streams the incoming request straight
// through — this lets the add handler rewrite a magnet POST into a multipart
// upload that carries the stored .torrent bytes.
func (c *managerClient) postBody(w http.ResponseWriter, ctx context.Context, managerPath, contentType string, body []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+managerPath, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", contentType)
	c.authReq(req)

	resp, err := c.http.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "manager unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// addTorrent places a magnet on a streamer via the manager (server-to-server),
// used by the manual .torrent backfill to make a streamer start resolving a
// source's metadata. Idempotent on the manager side — a tracked magnet returns
// its existing placement.
func (c *managerClient) addTorrent(ctx context.Context, magnet string) error {
	body, err := json.Marshal(map[string]string{"magnet": magnet})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/torrents", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authReq(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("manager add torrent: %s", resp.Status)
	}
	return nil
}

// getMetainfo fetches a torrent's resolved .torrent bytes from the manager (which
// routes to the owning streamer). errMetainfoNotReady means the streamer hasn't
// resolved the metadata yet — the caller should retry later; any other non-2xx is
// returned as a generic error.
func (c *managerClient) getMetainfo(ctx context.Context, infoHash string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/torrents/"+url.PathEscape(infoHash)+"/metainfo", nil)
	if err != nil {
		return nil, err
	}
	c.authReq(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		io.Copy(io.Discard, resp.Body)
		return nil, errMetainfoNotReady
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("manager metainfo: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMetainfoBytes))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("manager returned empty metainfo")
	}
	return data, nil
}

// instanceStatus mirrors the manager's /admin/instances entries for the
// Streamers dashboard.
type instanceStatus struct {
	ID          string           `json:"id"`
	InternalURL string           `json:"internalURL"`
	PublicURL   string           `json:"publicURL"`
	Healthy     bool             `json:"healthy"`
	Torrents    []managerTorrent `json:"torrents"`

	// Version and Settings are what the streamer self-reported on register. The
	// settings map is opaque (whatever knobs that streamer build sends), rendered
	// generically as key:value chips.
	Version  string         `json:"version"`
	Settings map[string]any `json:"settings"`
}

// managerTorrent is the subset of a streamer torrent the dashboard renders.
type managerTorrent struct {
	InfoHash       string `json:"infoHash"`
	Name           string `json:"name"`
	TotalBytes     int64  `json:"totalBytes"`
	BytesCompleted int64  `json:"bytesCompleted"`
}

// instances fetches the manager's instance/torrent status for the dashboard.
func (c *managerClient) instances(ctx context.Context) ([]instanceStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/admin/instances", nil)
	if err != nil {
		return nil, err
	}
	c.authReq(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manager status: %s", resp.Status)
	}
	var out []instanceStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// enrollment mirrors the manager's /admin/enrollments entries: a streamer's
// allow-list record plus whether it is currently registered and healthy.
type enrollment struct {
	ID              string `json:"id"`
	Approved        bool   `json:"approved"`
	Registered      bool   `json:"registered"`
	Healthy         bool   `json:"healthy"`
	LastInternalURL string `json:"lastInternalURL"`
	LastPublicURL   string `json:"lastPublicURL"`
	LastVersion     string `json:"lastVersion"`
}

// enrollments fetches the streamer enrollment allow-list (pending + approved).
func (c *managerClient) enrollments(ctx context.Context) ([]enrollment, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/admin/enrollments", nil)
	if err != nil {
		return nil, err
	}
	c.authReq(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manager enrollments: %s", resp.Status)
	}
	var out []enrollment
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// approveEnrollment approves a pending streamer by id.
func (c *managerClient) approveEnrollment(ctx context.Context, id string) error {
	return c.enrollmentAction(ctx, http.MethodPost, "/admin/enrollments/"+id+"/approve")
}

// revokeEnrollment removes a streamer's enrollment and drops its live instance.
func (c *managerClient) revokeEnrollment(ctx context.Context, id string) error {
	return c.enrollmentAction(ctx, http.MethodDelete, "/admin/enrollments/"+id)
}

func (c *managerClient) enrollmentAction(ctx context.Context, method, path string) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	c.authReq(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("manager enrollment action: %s", resp.Status)
	}
	return nil
}

// buildTorrentMultipart builds a multipart/form-data body carrying the raw
// .torrent bytes in a "torrent" file part plus the magnet in a "magnet" field —
// the shape the streamer's add API accepts. The streamer prefers the file part
// (skipping DHT metadata resolution); the magnet rides along so the manager can
// read the infohash off it to dedupe placement.
func buildTorrentMultipart(magnet string, torrentFile []byte) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if magnet != "" {
		if err = mw.WriteField("magnet", magnet); err != nil {
			return nil, "", err
		}
	}
	fw, err := mw.CreateFormFile("torrent", "source.torrent")
	if err != nil {
		return nil, "", err
	}
	if _, err = fw.Write(torrentFile); err != nil {
		return nil, "", err
	}
	if err = mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

var btihHexRe = regexp.MustCompile(`(?i)urn:btih:([0-9a-f]{40})`)

// infohashFromMagnet pulls a lowercase hex infohash from a magnet URI, or "" if
// absent or base32-encoded. Used to look up a pasted magnet's stored .torrent
// bytes; a miss just means the add falls back to the plain magnet. Mirrors the
// manager's helper of the same name.
func infohashFromMagnet(magnet string) string {
	if magnet == "" {
		return ""
	}
	if u, err := url.Parse(magnet); err == nil {
		for _, xt := range u.Query()["xt"] {
			if m := btihHexRe.FindStringSubmatch(xt); m != nil {
				return strings.ToLower(m[1])
			}
		}
	}
	if m := btihHexRe.FindStringSubmatch(magnet); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

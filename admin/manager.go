package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

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

// instanceStatus mirrors the manager's /admin/instances entries for the
// Streamers dashboard.
type instanceStatus struct {
	ID          string           `json:"id"`
	InternalURL string           `json:"internalURL"`
	PublicURL   string           `json:"publicURL"`
	Healthy     bool             `json:"healthy"`
	Torrents    []managerTorrent `json:"torrents"`
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

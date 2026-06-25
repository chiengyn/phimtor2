package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Instance is one registered streamer. It carries both URLs: InternalURL for the
// manager's server-to-server control calls, PublicURL for the browser-direct
// stats/stream (handed back to callers so the browser hits the owning streamer).
type Instance struct {
	ID          string
	InternalURL string
	PublicURL   string

	lastSeen atomic.Int64 // unix nano of the last register/heartbeat
	http     *http.Client
}

func newInstance(id, internalURL, publicURL string, timeout time.Duration) *Instance {
	in := &Instance{
		ID:          id,
		InternalURL: strings.TrimRight(internalURL, "/"),
		PublicURL:   strings.TrimRight(publicURL, "/"),
		http:        &http.Client{Timeout: timeout},
	}
	in.touch()
	return in
}

func (in *Instance) touch() { in.lastSeen.Store(time.Now().UnixNano()) }

func (in *Instance) healthy(ttl time.Duration) bool {
	return time.Since(time.Unix(0, in.lastSeen.Load())) <= ttl
}

// do issues a control-plane request to this streamer's internal API, attaching
// the shared bearer token. The caller owns closing the response body.
func (in *Instance) do(ctx context.Context, method, path, token, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, in.InternalURL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return in.http.Do(req)
}

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	// ControlToken is the streamer's self-generated identity token, presented by
	// the manager on every control call (set on register). SessionToken is the
	// manager-minted bearer the streamer presents on heartbeat/deregister.
	ControlToken string
	SessionToken string

	lastSeen atomic.Int64 // unix nano of the last register/heartbeat

	// selfReport is what the streamer said about itself on its last register
	// (build version + operational settings), displayed on the admin Streamers
	// dashboard. Atomic because registers and dashboard reads race.
	selfReport atomic.Pointer[InstanceMeta]

	// egressSpeed is the streamer's last-polled viewer egress rate in bytes/sec
	// (HTTP bytes served to browsers, refreshed by the registry's reconcile loop),
	// the signal the least-bandwidth placer minimizes.
	egressSpeed atomic.Int64

	http *http.Client
}

// InstanceMeta is a streamer's self-reported build version and operational
// settings, sent in its register payload. The manager never interprets the
// settings — they pass through opaquely to the admin dashboard, so a new
// streamer setting needs no manager change to become visible.
type InstanceMeta struct {
	Version  string
	Settings map[string]any
}

func (in *Instance) setMeta(m *InstanceMeta) { in.selfReport.Store(m) }
func (in *Instance) meta() *InstanceMeta     { return in.selfReport.Load() }

func (in *Instance) setEgress(bytesPerSec int64) { in.egressSpeed.Store(bytesPerSec) }

// egress is the instance's last-polled viewer egress rate in bytes/sec, used by
// the least-bandwidth placer.
func (in *Instance) egress() int64 { return in.egressSpeed.Load() }

func newInstance(id, internalURL, publicURL, controlToken string, timeout time.Duration) *Instance {
	in := &Instance{
		ID:           id,
		InternalURL:  strings.TrimRight(internalURL, "/"),
		PublicURL:    strings.TrimRight(publicURL, "/"),
		ControlToken: controlToken,
		SessionToken: newRandomToken(),
		http:         &http.Client{Timeout: timeout},
	}
	in.touch()
	return in
}

// newRandomToken returns a 256-bit cryptographically random hex token, used for
// the per-instance session token the manager mints for each streamer.
func newRandomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (in *Instance) touch() { in.lastSeen.Store(time.Now().UnixNano()) }

func (in *Instance) healthy(ttl time.Duration) bool {
	return time.Since(time.Unix(0, in.lastSeen.Load())) <= ttl
}

// do issues a control-plane request to this streamer's internal API, attaching
// the instance's control token (the streamer's own identity, pinned on approval).
// The caller owns closing the response body.
func (in *Instance) do(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, in.InternalURL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if in.ControlToken != "" {
		req.Header.Set("Authorization", "Bearer "+in.ControlToken)
	}
	return in.http.Do(req)
}

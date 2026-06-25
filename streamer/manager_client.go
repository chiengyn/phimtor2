package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

// heartbeatInterval is how often a registered streamer pings the manager. It
// must stay comfortably below the manager's MANAGER_HEARTBEAT_TTL (default 30s)
// so a single dropped ping doesn't expire the instance.
const heartbeatInterval = 10 * time.Second

// managerClient registers this streamer with the manager and heartbeats until
// shutdown. When MANAGER_URL is empty it is inert, so single-streamer dev keeps
// working with no manager at all.
type managerClient struct {
	cfg  Config
	http *http.Client

	stop   chan struct{}
	wg     sync.WaitGroup
	closed bool
}

type registerPayload struct {
	ID          string `json:"id"`
	InternalURL string `json:"internalURL"`
	PublicURL   string `json:"publicURL"`
}

func newManagerClient(cfg Config) *managerClient {
	return &managerClient{
		cfg:  cfg,
		http: &http.Client{Timeout: 10 * time.Second},
		stop: make(chan struct{}),
	}
}

func (m *managerClient) enabled() bool { return m.cfg.ManagerURL != "" }

// Start registers immediately and then heartbeats on an interval. It returns
// right away; the work runs in a background goroutine.
func (m *managerClient) Start() {
	if !m.enabled() {
		return
	}
	if m.cfg.AdvertiseInternalURL == "" || m.cfg.AdvertisePublicURL == "" {
		log.Printf("manager registration disabled: set STREAMER_ADVERTISE_INTERNAL_URL and STREAMER_ADVERTISE_PUBLIC_URL")
		return
	}

	m.wg.Add(1)
	go m.loop()
}

func (m *managerClient) loop() {
	defer m.wg.Done()

	// A registered instance is unknown to the manager until the first register
	// succeeds; heartbeats fall back to re-registering if the manager forgot us
	// (e.g. it restarted).
	registered := m.register()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			if !registered {
				registered = m.register()
				continue
			}
			if !m.heartbeat() {
				registered = m.register()
			}
		}
	}
}

func (m *managerClient) register() bool {
	body := registerPayload{
		ID:          m.cfg.InstanceID,
		InternalURL: m.cfg.AdvertiseInternalURL,
		PublicURL:   m.cfg.AdvertisePublicURL,
	}
	if err := m.post("/api/instances/register", body); err != nil {
		log.Printf("manager register failed: %v", err)
		return false
	}
	log.Printf("registered with manager %s as %q", m.cfg.ManagerURL, m.cfg.InstanceID)
	return true
}

func (m *managerClient) heartbeat() bool {
	if err := m.post("/api/instances/heartbeat", registerPayload{ID: m.cfg.InstanceID}); err != nil {
		log.Printf("manager heartbeat failed: %v", err)
		return false
	}
	return true
}

// Stop deregisters (best-effort) and waits for the heartbeat goroutine to exit.
func (m *managerClient) Stop() {
	if !m.enabled() || m.closed {
		return
	}
	m.closed = true
	close(m.stop)
	m.wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.postCtx(ctx, "/api/instances/deregister", registerPayload{ID: m.cfg.InstanceID}); err != nil {
		log.Printf("manager deregister failed: %v", err)
	}
}

func (m *managerClient) post(path string, body any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.postCtx(ctx, path, body)
}

func (m *managerClient) postCtx(ctx context.Context, path string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.ManagerURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.cfg.RegisterToken != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.RegisterToken)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{status: resp.StatusCode}
	}
	return nil
}

type httpStatusError struct{ status int }

func (e *httpStatusError) Error() string {
	return "unexpected status " + http.StatusText(e.status)
}

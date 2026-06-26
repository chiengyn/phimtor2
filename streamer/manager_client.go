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
// shutdown. On register it presents the shared join token plus its self-generated
// control token, and receives a per-instance session token used for the
// heartbeat/deregister calls. An unapproved streamer is parked as pending and
// keeps retrying until an operator approves it.
type managerClient struct {
	cfg          Config
	controlToken string
	http         *http.Client

	// sessionToken is set on a successful register and used for heartbeat/
	// deregister. Only the loop goroutine touches it; Stop reads it after the loop
	// has exited (wg.Wait), so no lock is needed.
	sessionToken string

	stop   chan struct{}
	wg     sync.WaitGroup
	closed bool
}

type registerPayload struct {
	ID           string `json:"id"`
	InternalURL  string `json:"internalURL,omitempty"`
	PublicURL    string `json:"publicURL,omitempty"`
	ControlToken string `json:"controlToken,omitempty"`
}

func newManagerClient(cfg Config, controlToken string) *managerClient {
	return &managerClient{
		cfg:          cfg,
		controlToken: controlToken,
		http:         &http.Client{Timeout: 10 * time.Second},
		stop:         make(chan struct{}),
	}
}

// Start registers immediately and then heartbeats on an interval. It returns
// right away; the work runs in a background goroutine.
func (m *managerClient) Start() {
	m.wg.Add(1)
	go m.loop()
}

func (m *managerClient) loop() {
	defer m.wg.Done()

	// A registered instance is unknown to the manager until the first register
	// succeeds; heartbeats fall back to re-registering if the manager forgot us
	// (e.g. it restarted) or the streamer is still awaiting approval.
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

// register sends id + advertised URLs + the control token, gated by the shared
// join token. On success it stores the manager-issued session token. A 403 means
// the streamer is pending operator approval — not an error, just retry.
func (m *managerClient) register() bool {
	body := registerPayload{
		ID:           m.cfg.InstanceID,
		InternalURL:  m.cfg.AdvertiseInternalURL,
		PublicURL:    m.cfg.AdvertisePublicURL,
		ControlToken: m.controlToken,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		log.Printf("manager register: marshal: %v", err)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.ManagerURL+"/api/instances/register", bytes.NewReader(buf))
	if err != nil {
		log.Printf("manager register: %v", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if m.cfg.RegisterToken != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.RegisterToken)
	}

	resp, err := m.http.Do(req)
	if err != nil {
		log.Printf("manager register failed: %v", err)
		return false
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			SessionToken string `json:"sessionToken"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.SessionToken == "" {
			log.Printf("manager register: missing session token")
			return false
		}
		m.sessionToken = out.SessionToken
		log.Printf("registered with manager %s as %q", m.cfg.ManagerURL, m.cfg.InstanceID)
		return true
	case http.StatusForbidden:
		log.Printf("manager: instance %q is awaiting operator approval", m.cfg.InstanceID)
		return false
	default:
		log.Printf("manager register failed: unexpected status %s", resp.Status)
		return false
	}
}

func (m *managerClient) heartbeat() bool {
	if err := m.post("/api/instances/heartbeat", registerPayload{ID: m.cfg.InstanceID}, m.sessionToken); err != nil {
		log.Printf("manager heartbeat failed: %v", err)
		return false
	}
	return true
}

// Stop deregisters (best-effort) and waits for the heartbeat goroutine to exit.
func (m *managerClient) Stop() {
	if m.closed {
		return
	}
	m.closed = true
	close(m.stop)
	m.wg.Wait()

	if m.sessionToken == "" {
		return // never got approved, nothing to deregister
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.postCtx(ctx, "/api/instances/deregister", registerPayload{ID: m.cfg.InstanceID}, m.sessionToken); err != nil {
		log.Printf("manager deregister failed: %v", err)
	}
}

func (m *managerClient) post(path string, body any, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.postCtx(ctx, path, body, token)
}

func (m *managerClient) postCtx(ctx context.Context, path string, body any, token string) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.ManagerURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
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

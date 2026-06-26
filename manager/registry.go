package main

import (
	"context"
	"crypto/subtle"
	"log"
	"sync"
	"time"
)

// Registry holds the live streamer instances and the infohash→instance owner
// map. The owner map is a cache: it is rebuilt from ground truth by reconcile
// and self-heals on 404 (see router.go), so a stale entry is never fatal.
type Registry struct {
	cfg        Config
	ttl        time.Duration
	fwdTimeout time.Duration
	placer     Placer
	enroll     *EnrollmentStore

	mu        sync.RWMutex
	instances map[string]*Instance // by instance ID
	owners    map[string]*Instance // infohash → owning instance
}

func NewRegistry(cfg Config, enroll *EnrollmentStore) *Registry {
	return &Registry{
		cfg:        cfg,
		ttl:        time.Duration(cfg.HeartbeatTTLSec) * time.Second,
		fwdTimeout: time.Duration(cfg.ForwardTimeoutSec) * time.Second,
		placer:     newPlacer(cfg.LBStrategy),
		enroll:     enroll,
		instances:  make(map[string]*Instance),
		owners:     make(map[string]*Instance),
	}
}

// Register upserts an instance and refreshes its heartbeat, returning the
// session token the streamer must present on heartbeat/deregister. Re-registering
// with the same URLs and control token keeps the existing session token (so a
// heartbeat in flight stays valid); anything changed mints a fresh instance.
func (r *Registry) Register(id, internalURL, publicURL, controlToken string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if in, ok := r.instances[id]; ok &&
		in.InternalURL == trimSlash(internalURL) &&
		in.PublicURL == trimSlash(publicURL) &&
		in.ControlToken == controlToken {
		in.touch()
		return in.SessionToken
	}
	in := newInstance(id, internalURL, publicURL, controlToken, r.fwdTimeout)
	r.instances[id] = in
	log.Printf("instance registered: %s (internal=%s public=%s)", id, internalURL, publicURL)
	return in.SessionToken
}

// Heartbeat refreshes an instance's last-seen, but only if the presented session
// token matches. It returns false if the instance is unknown (e.g. the manager
// restarted) or the token is wrong, prompting the streamer to re-register.
func (r *Registry) Heartbeat(id, sessionToken string) bool {
	r.mu.RLock()
	in, ok := r.instances[id]
	r.mu.RUnlock()
	if !ok || subtle.ConstantTimeCompare([]byte(in.SessionToken), []byte(sessionToken)) != 1 {
		return false
	}
	in.touch()
	return true
}

// DeregisterWithToken drops an instance only when the presented session token
// matches, so one streamer can't deregister another. Returns false on a miss.
func (r *Registry) DeregisterWithToken(id, sessionToken string) bool {
	r.mu.RLock()
	in, ok := r.instances[id]
	r.mu.RUnlock()
	if !ok || subtle.ConstantTimeCompare([]byte(in.SessionToken), []byte(sessionToken)) != 1 {
		return false
	}
	r.Deregister(id)
	return true
}

func (r *Registry) Deregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.instances, id)
	for hash, in := range r.owners {
		if in.ID == id {
			delete(r.owners, hash)
		}
	}
	log.Printf("instance deregistered: %s", id)
}

// healthyInstances returns the instances that heartbeated within the TTL.
func (r *Registry) healthyInstances() []*Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Instance, 0, len(r.instances))
	for _, in := range r.instances {
		if in.healthy(r.ttl) {
			out = append(out, in)
		}
	}
	return out
}

// allInstances returns every registered instance (healthy or not) for the
// dashboard, which wants to show stale instances too.
func (r *Registry) allInstances() []*Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Instance, 0, len(r.instances))
	for _, in := range r.instances {
		out = append(out, in)
	}
	return out
}

func (r *Registry) instanceByID(id string) (*Instance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	in, ok := r.instances[id]
	return in, ok
}

func (r *Registry) owner(hash string) (*Instance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	in, ok := r.owners[hash]
	return in, ok
}

func (r *Registry) setOwner(hash string, in *Instance) {
	r.mu.Lock()
	r.owners[hash] = in
	r.mu.Unlock()
}

func (r *Registry) clearOwner(hash string) {
	r.mu.Lock()
	delete(r.owners, hash)
	r.mu.Unlock()
}

// ownerCounts returns the number of torrents mapped to each instance ID, used by
// the least-torrents placer.
func (r *Registry) ownerCounts() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	counts := make(map[string]int, len(r.instances))
	for _, in := range r.owners {
		counts[in.ID]++
	}
	return counts
}

// pickInstance chooses a healthy instance for a new torrent via the configured
// strategy. Returns nil if no instance is available.
func (r *Registry) pickInstance() *Instance {
	healthy := r.healthyInstances()
	if len(healthy) == 0 {
		return nil
	}
	return r.placer.Pick(healthy, r.ownerCounts())
}

// Run starts the background loops (heartbeat-expiry sweep + owner-map reconcile)
// and blocks until ctx is cancelled.
func (r *Registry) Run(ctx context.Context) {
	sweep := time.NewTicker(r.ttl)
	defer sweep.Stop()
	reconcile := time.NewTicker(time.Duration(r.cfg.ReconcileIntervalSec) * time.Second)
	defer reconcile.Stop()

	r.reconcileOnce(ctx) // rebuild the owner map at startup

	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			r.expireStale()
		case <-reconcile.C:
			r.reconcileOnce(ctx)
		}
	}
}

// expireStale drops instances that have not heartbeated within the TTL and prunes
// any owner entries pointing at them.
func (r *Registry) expireStale() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, in := range r.instances {
		if !in.healthy(r.ttl) {
			delete(r.instances, id)
			for hash, owner := range r.owners {
				if owner.ID == id {
					delete(r.owners, hash)
				}
			}
			log.Printf("instance expired (no heartbeat): %s", id)
		}
	}
}

// reconcileOnce fans out a list to every healthy instance and refreshes the
// owner map from the result. This catches reaper evictions, out-of-band adds,
// and torrents that moved between instances.
func (r *Registry) reconcileOnce(ctx context.Context) {
	for _, in := range r.healthyInstances() {
		hashes, err := r.listInfohashes(ctx, in)
		if err != nil {
			log.Printf("reconcile: list %s failed: %v", in.ID, err)
			continue
		}
		for _, h := range hashes {
			r.setOwner(h, in)
		}
	}
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// watchSession is one browser tab actively watching a torrent: the infohash it
// last reported and when we last heard from it.
type watchSession struct {
	infoHash string
	lastSeen time.Time
}

// watchTracker reference-counts active watch sessions per torrent so a torrent is
// dropped the moment its last viewer leaves — freeing the streamer's peers, cache
// and disk immediately instead of waiting for the streamer's idle reaper (tens of
// minutes later).
//
// Each watch-page tab sends a periodic heartbeat (and a best-effort leave beacon
// on page hide). A session is keyed by a browser-generated id; the tracker maps
// each session to the torrent it watches and keeps a per-torrent session count.
// When a session leaves (explicit beacon) or goes silent past the TTL (the
// sweep), and it was the last viewer of its torrent, the torrent is dropped via
// the manager. The reference count is what makes one user leaving safe while
// others keep watching the same torrent.
// dropGrace is how long the tracker waits after a torrent's last viewer leaves
// before dropping it. A leave beacon immediately followed by a heartbeat — a brief
// mobile background, a flaky-network blip, or an in-page navigation race — cancels
// the pending drop, so a viewer who is really still watching never has the torrent
// yanked out from under them. Kept short so a genuine departure still frees the
// streamer's resources promptly.
const dropGrace = 5 * time.Second

type watchTracker struct {
	mu       sync.Mutex
	sessions map[string]*watchSession // sessionID → session
	counts   map[string]int           // infoHash → live session count
	pending  map[string]*time.Timer   // infoHash → scheduled drop, cancelable

	ttl   time.Duration
	grace time.Duration
	drop  func(ctx context.Context, infoHash string) error
}

func newWatchTracker(ttl time.Duration, drop func(context.Context, string) error) *watchTracker {
	return &watchTracker{
		sessions: map[string]*watchSession{},
		counts:   map[string]int{},
		pending:  map[string]*time.Timer{},
		ttl:      ttl,
		grace:    dropGrace,
		drop:     drop,
	}
}

// beat records a heartbeat from sessionID for infoHash. If the session was
// watching a different torrent (the user switched sources), the previous torrent
// loses this viewer and is dropped when its count falls to zero.
func (t *watchTracker) beat(sessionID, infoHash string) {
	if sessionID == "" || infoHash == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sessions[sessionID]; ok {
		if s.infoHash == infoHash {
			s.lastSeen = time.Now()
			return
		}
		t.scheduleDrop(t.release(s.infoHash)) // source switch: drop the old torrent
	}
	t.sessions[sessionID] = &watchSession{infoHash: infoHash, lastSeen: time.Now()}
	t.counts[infoHash]++
	t.cancelPending(infoHash) // a viewer is (re)watching this torrent — abort any pending drop
}

// leave ends a session (the page-hide beacon), scheduling its torrent's drop if it
// was the last viewer — after a short grace so an immediately-following heartbeat
// can cancel it.
func (t *watchTracker) leave(sessionID string) {
	if sessionID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.sessions[sessionID]
	if !ok {
		return
	}
	delete(t.sessions, sessionID)
	t.scheduleDrop(t.release(s.infoHash))
}

// sweep ends sessions we haven't heard from within the TTL — a crashed tab,
// killed mobile app, or dead network that never sent a leave beacon — and drops
// any torrent left with no viewers. Called on a ticker by run.
func (t *watchTracker) sweep() {
	cutoff := time.Now().Add(-t.ttl)
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, s := range t.sessions {
		if s.lastSeen.Before(cutoff) {
			delete(t.sessions, id)
			t.scheduleDrop(t.release(s.infoHash))
		}
	}
}

// release decrements a torrent's viewer count and returns its infohash if that
// was the last viewer (so the caller can drop it after unlocking), else "". The
// caller must hold t.mu.
func (t *watchTracker) release(infoHash string) string {
	if t.counts[infoHash] <= 1 {
		delete(t.counts, infoHash)
		return infoHash
	}
	t.counts[infoHash]--
	return ""
}

// scheduleDrop arranges to drop a torrent that lost its last viewer after the
// grace period, unless a viewer reappears first — cancelPending stops it when a
// heartbeat re-acquires the torrent, and the timer rechecks the live count before
// firing in case a viewer returned without us cancelling. The grace absorbs a
// leave beacon immediately followed by a heartbeat (a brief mobile background) so a
// returning viewer never has the torrent yanked away. Caller holds t.mu.
func (t *watchTracker) scheduleDrop(infoHash string) {
	if infoHash == "" {
		return
	}
	if t.grace <= 0 {
		go t.dropNow(infoHash) // grace disabled: drop immediately
		return
	}
	if old := t.pending[infoHash]; old != nil {
		old.Stop()
	}
	t.pending[infoHash] = time.AfterFunc(t.grace, func() {
		t.mu.Lock()
		delete(t.pending, infoHash)
		reappeared := t.counts[infoHash] > 0
		t.mu.Unlock()
		if reappeared {
			return // a viewer came back during the grace window
		}
		t.dropNow(infoHash)
	})
}

// cancelPending stops a scheduled drop for infoHash (a viewer is watching it
// again). Caller holds t.mu.
func (t *watchTracker) cancelPending(infoHash string) {
	if tm := t.pending[infoHash]; tm != nil {
		tm.Stop()
		delete(t.pending, infoHash)
	}
}

// dropNow drops a torrent via the manager, off the request path (the page-hide
// beacon's request context is already cancelled) with its own timeout.
func (t *watchTracker) dropNow(infoHash string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := t.drop(ctx, infoHash); err != nil {
		log.Printf("watch: drop torrent %s: %v", infoHash, err)
	}
}

// run sweeps idle sessions on a ticker until ctx is canceled.
func (t *watchTracker) run(ctx context.Context) {
	tick := max(t.ttl/2, time.Second)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sweep()
		}
	}
}

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
type watchTracker struct {
	mu       sync.Mutex
	sessions map[string]*watchSession // sessionID → session
	counts   map[string]int           // infoHash → live session count

	ttl  time.Duration
	drop func(ctx context.Context, infoHash string) error
}

func newWatchTracker(ttl time.Duration, drop func(context.Context, string) error) *watchTracker {
	return &watchTracker{
		sessions: map[string]*watchSession{},
		counts:   map[string]int{},
		ttl:      ttl,
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
	var orphan string
	t.mu.Lock()
	if s, ok := t.sessions[sessionID]; ok {
		if s.infoHash == infoHash {
			s.lastSeen = time.Now()
			t.mu.Unlock()
			return
		}
		orphan = t.release(s.infoHash) // source switch: drop the old torrent
	}
	t.sessions[sessionID] = &watchSession{infoHash: infoHash, lastSeen: time.Now()}
	t.counts[infoHash]++
	t.mu.Unlock()

	t.dropOrphan(orphan)
}

// leave ends a session immediately (the page-hide beacon), dropping its torrent
// if it was the last viewer.
func (t *watchTracker) leave(sessionID string) {
	if sessionID == "" {
		return
	}
	t.mu.Lock()
	s, ok := t.sessions[sessionID]
	if !ok {
		t.mu.Unlock()
		return
	}
	delete(t.sessions, sessionID)
	orphan := t.release(s.infoHash)
	t.mu.Unlock()

	t.dropOrphan(orphan)
}

// sweep ends sessions we haven't heard from within the TTL — a crashed tab,
// killed mobile app, or dead network that never sent a leave beacon — and drops
// any torrent left with no viewers. Called on a ticker by run.
func (t *watchTracker) sweep() {
	cutoff := time.Now().Add(-t.ttl)
	var orphans []string
	t.mu.Lock()
	for id, s := range t.sessions {
		if s.lastSeen.Before(cutoff) {
			delete(t.sessions, id)
			if h := t.release(s.infoHash); h != "" {
				orphans = append(orphans, h)
			}
		}
	}
	t.mu.Unlock()

	for _, h := range orphans {
		t.dropOrphan(h)
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

// dropOrphan drops a torrent that lost its last viewer. It runs off the request
// path (the page-hide beacon's request context is already canceled, and a source
// switch shouldn't block the heartbeat response) with its own timeout.
func (t *watchTracker) dropOrphan(infoHash string) {
	if infoHash == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := t.drop(ctx, infoHash); err != nil {
			log.Printf("watch: drop torrent %s: %v", infoHash, err)
		}
	}()
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

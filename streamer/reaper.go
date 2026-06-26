package main

import (
	"log"
	"time"

	"github.com/anacrolix/torrent"
)

// torrentActivity records streaming usage for one torrent so the reaper can tell
// idle torrents apart from in-use ones. readers is the number of stream readers
// currently open; lastUsed advances whenever a reader opens or closes. A torrent
// with no open readers and no use within idleTTL is reaped (dropped) to free its
// disk blobs and peer connections.
type torrentActivity struct {
	readers  int
	lastUsed time.Time
}

// touchActivity records that a torrent exists / was just used, starting (or
// restarting) its idle clock. Called on add and whenever a reader opens/closes.
func (m *TorrentManager) touchActivity(infoHash string) {
	m.activityMu.Lock()
	a := m.activity[infoHash]
	if a == nil {
		a = &torrentActivity{}
		m.activity[infoHash] = a
	}
	a.lastUsed = time.Now()
	m.activityMu.Unlock()
}

func (m *TorrentManager) markReaderOpened(infoHash string) {
	m.activityMu.Lock()
	a := m.activity[infoHash]
	if a == nil {
		a = &torrentActivity{}
		m.activity[infoHash] = a
	}
	a.readers++
	a.lastUsed = time.Now()
	m.activityMu.Unlock()
}

func (m *TorrentManager) markReaderClosed(infoHash string) {
	m.activityMu.Lock()
	if a := m.activity[infoHash]; a != nil {
		if a.readers > 0 {
			a.readers--
		}
		a.lastUsed = time.Now() // start the idle clock from when streaming stopped
	}
	m.activityMu.Unlock()
}

// runReaper periodically drops torrents that have gone unstreamed for idleTTL.
// The check interval is a fraction of the TTL, clamped to a sane range. Stops
// when bgDone is closed (manager Close).
func (m *TorrentManager) runReaper() {
	interval := m.idleTTL / 4
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	if interval > 10*time.Minute {
		interval = 10 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-m.bgDone:
			return
		case now := <-t.C:
			m.reapIdle(now)
		}
	}
}

// reapIdle drops every torrent with no open readers that hasn't been used within
// idleTTL. Candidates are collected under the activity lock, then removed without
// it (RemoveTorrent does its own locking and filesystem work). The brief gap
// between snapshot and removal is harmless given the TTL is minutes: a viewer
// that starts in that window simply keeps a freshly re-added torrent — at worst
// one reap is a no-op.
func (m *TorrentManager) reapIdle(now time.Time) {
	var idle []string
	m.activityMu.Lock()
	for ih, a := range m.activity {
		if a.readers == 0 && now.Sub(a.lastUsed) > m.idleTTL {
			idle = append(idle, ih)
		}
	}
	m.activityMu.Unlock()

	for _, ih := range idle {
		if err := m.RemoveTorrent(ih); err != nil {
			log.Printf("reap idle torrent %s: %v", ih, err)
		} else {
			log.Printf("reaped idle torrent %s (no viewers for >%s)", ih, m.idleTTL)
		}
	}
}

// stallProgress is the last observed download position for one torrent, used by
// the stall checker to measure how long a watched torrent has been stuck.
type stallProgress struct {
	bytes int64
	at    time.Time
}

// runStallChecker periodically drops torrents a viewer is waiting on that cannot
// make download progress — incomplete, no new bytes for stallTimeout, and no
// connected seeders to ever supply the missing pieces (a dead/unreachable
// swarm). Such a torrent otherwise pins a peer slot and an open reader forever
// while the browser spins. A torrent that downloads anything, completes, or
// still has a seeder connected (merely slow, or paused) resets its clock and is
// left alone. Runs on a fraction of stallTimeout; stops when bgDone is closed.
func (m *TorrentManager) runStallChecker() {
	interval := m.stallTimeout / 3
	if interval < 15*time.Second {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	seen := make(map[string]stallProgress)
	for {
		select {
		case <-m.bgDone:
			return
		case now := <-t.C:
			m.checkStalls(now, seen)
		}
	}
}

// checkStalls is one pass of the stall checker. seen carries each watched
// torrent's last-progress sample across passes and is pruned of torrents that
// have gone away or stopped being watched.
func (m *TorrentManager) checkStalls(now time.Time, seen map[string]stallProgress) {
	m.mu.RLock()
	torrents := make(map[string]*torrent.Torrent, len(m.torrents))
	for ih, t := range m.torrents {
		torrents[ih] = t
	}
	m.mu.RUnlock()

	var stalled []string
	for ih, t := range torrents {
		// Only a torrent someone is actively trying to watch can "stall"; idle
		// ones are the reaper's job.
		m.activityMu.Lock()
		readers := 0
		if a := m.activity[ih]; a != nil {
			readers = a.readers
		}
		m.activityMu.Unlock()
		if readers == 0 {
			delete(seen, ih)
			continue
		}

		if t.Info() == nil {
			continue // metadata not ready yet (no reader could be open anyway)
		}
		completed := t.BytesCompleted()
		if completed >= t.Length() {
			delete(seen, ih) // fully downloaded: never stuck
			continue
		}

		prev, ok := seen[ih]
		if !ok || completed > prev.bytes {
			seen[ih] = stallProgress{bytes: completed, at: now} // progress (or first sight): reset clock
			continue
		}
		if now.Sub(prev.at) < m.stallTimeout {
			continue // stuck, but not for long enough yet
		}
		// No progress for stallTimeout. Leave it alone if a seeder is still
		// connected — it may just be slow (or paused with a full buffer); only a
		// torrent with nobody to download from is truly stuck.
		if t.Stats().ConnectedSeeders > 0 {
			seen[ih] = stallProgress{bytes: completed, at: now}
			continue
		}
		stalled = append(stalled, ih)
	}

	// Forget torrents that disappeared (removed/reaped) so seen doesn't grow.
	for ih := range seen {
		if _, ok := torrents[ih]; !ok {
			delete(seen, ih)
		}
	}

	for _, ih := range stalled {
		delete(seen, ih)
		if err := m.RemoveTorrent(ih); err != nil {
			log.Printf("drop stalled torrent %s: %v", ih, err)
		} else {
			log.Printf("dropped stalled torrent %s (viewer waiting, no progress for >%s, no seeders)", ih, m.stallTimeout)
		}
	}
}

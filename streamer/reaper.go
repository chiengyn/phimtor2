package main

import (
	"log"
	"time"
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
// when reaperDone is closed (manager Close).
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
		case <-m.reaperDone:
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

package main

import (
	"os"
	"strings"
	"sync"
)

// fdCache is a small bounded LRU of read-only *os.File handles keyed by path. The
// blob-per-piece storages write one file per piece and, without this, every
// ReadAt would open+close that blob — a lot of syscall churn when many viewers
// are streaming at once. Caching the open handle turns repeat reads of the same
// piece into a bare pread.
//
// Concurrency: os.File.ReadAt uses pread (no shared offset) so a cached handle is
// safe to share across readers. Handles are reference-counted: eviction (or an
// explicit drop) removes the entry from the map immediately but only closes the
// file once no read is in flight — closing an in-use handle would fail that read
// with "file already closed", and under download-all's hash storms (hundreds of
// pieces hashing concurrently, far more hot blobs than the cap) that race went
// from rare to constant: every failed hash discarded the piece and re-downloaded
// it, so nothing past the first `max` pieces ever verified. Blobs are only
// unlinked after their handle is dropped, and on Linux an unlinked-but-open file
// still reads correctly.
type fdCache struct {
	mu    sync.Mutex
	max   int
	clock uint64
	m     map[string]*fdEntry
}

type fdEntry struct {
	f    *os.File
	used uint64 // last-access tick, for LRU eviction
	refs int    // in-flight reads holding this handle
	dead bool   // evicted/dropped; close once refs reaches 0
}

func newFDCache(max int) *fdCache {
	return &fdCache{max: max, m: make(map[string]*fdEntry)}
}

// readAt reads from path's cached handle, opening (and caching) it on first use.
// The handle is pinned (refcounted) for the duration of the pread, so a
// concurrent eviction can't close it mid-read.
func (c *fdCache) readAt(path string, b []byte, off int64) (int, error) {
	c.mu.Lock()
	e := c.m[path]
	if e == nil {
		f, err := os.Open(path)
		if err != nil {
			c.mu.Unlock()
			return 0, err
		}
		e = &fdEntry{f: f}
		c.m[path] = e
	}
	// Touch and pin BEFORE evicting: a fresh entry starts at used=0, and evicting
	// first would make the entry we just inserted the LRU victim every time the
	// cache is full — a hard failure wall at exactly `max` distinct blobs.
	c.clock++
	e.used = c.clock
	e.refs++
	c.evictLocked()
	c.mu.Unlock()

	n, err := e.f.ReadAt(b, off)

	c.mu.Lock()
	e.refs--
	if e.dead && e.refs == 0 {
		_ = e.f.Close()
	}
	c.mu.Unlock()
	return n, err
}

// retireLocked removes an entry from the map and closes its file, deferring the
// close to the last in-flight reader if the handle is currently pinned. Caller
// holds c.mu.
func (c *fdCache) retireLocked(path string, e *fdEntry) {
	delete(c.m, path)
	if e.refs == 0 {
		_ = e.f.Close()
	} else {
		e.dead = true
	}
}

// drop closes and forgets path's handle (call when its blob is evicted or
// rewritten so later reads re-open the fresh file).
func (c *fdCache) drop(path string) {
	c.mu.Lock()
	if e := c.m[path]; e != nil {
		c.retireLocked(path, e)
	}
	c.mu.Unlock()
}

// dropPrefix closes and forgets every handle whose path starts with prefix (call
// before deleting a torrent's blob directory so its fds are released, not just
// left dangling on unlinked files).
func (c *fdCache) dropPrefix(prefix string) {
	c.mu.Lock()
	for path, e := range c.m {
		if strings.HasPrefix(path, prefix) {
			c.retireLocked(path, e)
		}
	}
	c.mu.Unlock()
}

// evictLocked retires least-recently-used unpinned handles until under the cap.
// Caller holds c.mu. The cap is small so the O(n) scan is cheap. Pinned entries
// (a read in flight) are skipped — if everything is pinned the map briefly
// overshoots the cap, bounded by read concurrency, and shrinks on later calls.
func (c *fdCache) evictLocked() {
	for len(c.m) > c.max {
		var oldKey string
		var oldE *fdEntry
		oldUsed := ^uint64(0)
		for k, e := range c.m {
			if e.refs > 0 {
				continue
			}
			if e.used < oldUsed {
				oldKey, oldE, oldUsed = k, e, e.used
			}
		}
		if oldE == nil {
			return
		}
		c.retireLocked(oldKey, oldE)
	}
}

func (c *fdCache) closeAll() {
	c.mu.Lock()
	for k, e := range c.m {
		c.retireLocked(k, e)
	}
	c.mu.Unlock()
}

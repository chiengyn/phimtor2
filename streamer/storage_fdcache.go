package main

import (
	"os"
	"sync"
)

// fdCache is a small bounded LRU of read-only *os.File handles keyed by path. The
// prefix-cache storage writes one blob file per piece and, without this, every
// ReadAt would open+close that blob — a lot of syscall churn when many viewers
// are streaming at once. Caching the open handle turns repeat reads of the same
// piece into a bare pread.
//
// Concurrency: os.File.ReadAt uses pread (no shared offset) so a cached handle is
// safe to share across readers. Go's *os.File reference-counts internally, so a
// drop/Close racing an in-flight ReadAt is safe (the read just returns an error
// rather than crashing). Blobs are only unlinked after the cached handle is
// dropped, and on Linux an unlinked-but-open file still reads correctly.
type fdCache struct {
	mu    sync.Mutex
	max   int
	clock uint64
	m     map[string]*fdEntry
}

type fdEntry struct {
	f    *os.File
	used uint64 // last-access tick, for LRU eviction
}

func newFDCache(max int) *fdCache {
	return &fdCache{max: max, m: make(map[string]*fdEntry)}
}

// readAt reads from path's cached handle, opening (and caching) it on first use.
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
		c.evictLocked()
	}
	c.clock++
	e.used = c.clock
	f := e.f
	c.mu.Unlock()
	return f.ReadAt(b, off)
}

// drop closes and forgets path's handle (call when its blob is evicted or
// rewritten so later reads re-open the fresh file).
func (c *fdCache) drop(path string) {
	c.mu.Lock()
	if e := c.m[path]; e != nil {
		delete(c.m, path)
		_ = e.f.Close()
	}
	c.mu.Unlock()
}

// evictLocked closes least-recently-used handles until under the cap. Caller
// holds c.mu. The cap is small so the O(n) scan is cheap.
func (c *fdCache) evictLocked() {
	for len(c.m) > c.max {
		var oldKey string
		var oldUsed = ^uint64(0)
		for k, e := range c.m {
			if e.used < oldUsed {
				oldKey, oldUsed = k, e.used
			}
		}
		e := c.m[oldKey]
		delete(c.m, oldKey)
		_ = e.f.Close()
	}
}

func (c *fdCache) closeAll() {
	c.mu.Lock()
	for k, e := range c.m {
		_ = e.f.Close()
		delete(c.m, k)
	}
	c.mu.Unlock()
}

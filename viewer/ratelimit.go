package main

import (
	"strconv"
	"sync"
	"time"
)

// maxRateLimitKeys bounds the map before a full sweep. Well above any plausible
// concurrent-signed-in-user count, so the sweep is a safety valve rather than a
// hot path.
const maxRateLimitKeys = 4096

// rateLimiter is a sliding-window counter: at most limit events per key per
// window.
//
// In-memory and per-process, deliberately. It guards a shared third-party quota,
// not money or entitlements, so forgiving a few calls across a restart costs
// nothing — and a shared store would be a new dependency for no real gain at one
// replica. Unlike watchTracker there is no background goroutine: stale stamps
// for a key are pruned when that key is next touched, and the whole map is swept
// only when it grows past maxRateLimitKeys.
//
// A nil *rateLimiter allows everything, so an unconfigured limit is simply off.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

// newRateLimiter returns nil when limit <= 0, which disables the check.
func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	if limit <= 0 {
		return nil
	}
	return &rateLimiter{limit: limit, window: window, hits: map[string][]time.Time{}}
}

// allow records an event for key and reports whether it stayed within the limit.
// A rejected call is NOT recorded, so hammering a limiter cannot extend the
// window past the point where it would otherwise reopen.
func (l *rateLimiter) allow(key string) bool {
	if l == nil {
		return true
	}
	now := time.Now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.hits) > maxRateLimitKeys {
		l.sweepLocked(cutoff)
	}

	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// sweepLocked drops keys whose every stamp has aged out. Caller holds l.mu.
func (l *rateLimiter) sweepLocked(cutoff time.Time) {
	for k, stamps := range l.hits {
		live := stamps[:0]
		for _, t := range stamps {
			if t.After(cutoff) {
				live = append(live, t)
			}
		}
		if len(live) == 0 {
			delete(l.hits, k)
			continue
		}
		l.hits[k] = live
	}
}

// rateKey identifies a user for the limiters. Keyed by account id rather than IP
// because every rate-limited route sits behind requireUser — an id is stable
// across networks and cannot be shared by a whole NAT.
func rateKey(u *User) string {
	if u == nil {
		return "anon"
	}
	return "u" + strconv.FormatInt(u.ID, 10)
}

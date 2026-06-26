package main

import "sync/atomic"

// Placer chooses which streamer a new torrent lands on.
type Placer interface {
	// Pick selects from the healthy instances given the current per-instance
	// torrent counts (instance ID → count). instances is never empty.
	Pick(instances []*Instance, counts map[string]int) *Instance
}

func newPlacer(strategy string) Placer {
	switch strategy {
	case LBRoundRobin:
		return &roundRobin{}
	case LBLeastBandwidth:
		return leastBandwidth{}
	default:
		return leastTorrents{}
	}
}

// leastBandwidth sends each new torrent to the instance serving the least viewer
// egress right now (HTTP bytes/sec to browsers, polled from each streamer), a
// truer load signal than torrent count: a streamer with a few actively-watched
// torrents serves far more than one holding many idle ones. Ties — notably a cold
// fleet where every rate is zero — fall back to the fewest-torrents count, then
// instance ID, so an idle fleet still spreads evenly and deterministically.
type leastBandwidth struct{}

func (leastBandwidth) Pick(instances []*Instance, counts map[string]int) *Instance {
	best := instances[0]
	for _, in := range instances[1:] {
		if lessLoaded(in, best, counts) {
			best = in
		}
	}
	return best
}

// lessLoaded reports whether a is the better placement target than b: lower live
// egress, then fewer torrents, then lower ID.
func lessLoaded(a, b *Instance, counts map[string]int) bool {
	ae, be := a.egress(), b.egress()
	if ae != be {
		return ae < be
	}
	ac, bc := counts[a.ID], counts[b.ID]
	if ac != bc {
		return ac < bc
	}
	return a.ID < b.ID
}

// leastTorrents sends each new torrent to the instance currently owning the
// fewest, a cheap proxy for load (each torrent reserves peers + cache). Ties
// break by instance ID for determinism.
type leastTorrents struct{}

func (leastTorrents) Pick(instances []*Instance, counts map[string]int) *Instance {
	best := instances[0]
	bestCount := counts[best.ID]
	for _, in := range instances[1:] {
		c := counts[in.ID]
		if c < bestCount || (c == bestCount && in.ID < best.ID) {
			best, bestCount = in, c
		}
	}
	return best
}

type roundRobin struct{ n atomic.Uint64 }

func (rr *roundRobin) Pick(instances []*Instance, _ map[string]int) *Instance {
	i := rr.n.Add(1) - 1
	return instances[int(i%uint64(len(instances)))]
}

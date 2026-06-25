package main

import "sync/atomic"

// Placer chooses which streamer a new torrent lands on.
type Placer interface {
	// Pick selects from the healthy instances given the current per-instance
	// torrent counts (instance ID → count). instances is never empty.
	Pick(instances []*Instance, counts map[string]int) *Instance
}

func newPlacer(strategy string) Placer {
	if strategy == LBRoundRobin {
		return &roundRobin{}
	}
	return leastTorrents{}
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

package main

import (
	"context"
	"errors"
	"log"
	"time"
)

// harvester backfills stored .torrent bytes for magnet-only sources. Rather than
// resolving magnets itself (which would spend DHT lookups), it harvests the
// metainfo streamers have *already* resolved for torrents real viewers loaded:
// each tick it lists the live torrents across the fleet, keeps the ones whose
// source still has no stored file, and pulls their metainfo through the manager.
// This covers both viewer and admin plays and, together with the watch-page fast
// path, makes the next play of a once-magnet-only source skip the DHT wait.
type harvester struct {
	store    *Store
	manager  *managerClient
	interval time.Duration
}

func newHarvester(store *Store, manager *managerClient, interval time.Duration) *harvester {
	return &harvester{store: store, manager: manager, interval: interval}
}

// run ticks until ctx is cancelled. A non-positive interval disables it.
func (h *harvester) run(ctx context.Context) {
	if h.interval <= 0 {
		return
	}
	log.Printf("torrent-file harvester running every %s", h.interval)
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.tick(ctx)
		}
	}
}

// tick harvests one round. Best-effort throughout: any failure is logged and the
// next tick retries, so a transient manager/streamer hiccup never wedges it.
func (h *harvester) tick(ctx context.Context) {
	insts, err := h.manager.instances(ctx)
	if err != nil {
		log.Printf("harvest: list instances: %v", err)
		return
	}
	// Unique infohashes currently loaded across the fleet (a torrent can be listed
	// once per instance in principle; dedupe so we probe each only once).
	seen := map[string]struct{}{}
	var hashes []string
	for _, in := range insts {
		for _, t := range in.Torrents {
			if t.InfoHash == "" {
				continue
			}
			if _, dup := seen[t.InfoHash]; dup {
				continue
			}
			seen[t.InfoHash] = struct{}{}
			hashes = append(hashes, t.InfoHash)
		}
	}
	need, err := h.store.MagnetOnlyInfoHashes(ctx, hashes)
	if err != nil {
		log.Printf("harvest: filter magnet-only: %v", err)
		return
	}
	for _, hash := range need {
		data, err := h.manager.getMetainfo(ctx, hash)
		if err != nil {
			// Not resolved yet is the common case (a later tick catches it); only
			// surface the unexpected errors.
			if !errors.Is(err, errMetainfoNotReady) {
				log.Printf("harvest: metainfo %s: %v", hash, err)
			}
			continue
		}
		if err := h.store.BackfillTorrentFile(ctx, hash, data); err != nil {
			log.Printf("harvest: store %s: %v", hash, err)
			continue
		}
		log.Printf("harvest: backfilled .torrent for %s (%d bytes)", hash, len(data))
	}
}

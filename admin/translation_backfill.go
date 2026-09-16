package main

import (
	"context"
	"log"
	"time"
)

// runTranslationBackfill gradually refreshes pre-existing catalog rows so the
// additive translation tables fill without a one-time burst of TMDB requests.
// New imports already persist both configured languages and are skipped.
func runTranslationBackfill(ctx context.Context, store *Store, tmdb *TMDBClient, interval time.Duration) {
	if interval <= 0 {
		return
	}
	var cursor int64
	process := func() {
		item, err := store.NextMissingTranslation(ctx, canonicalLocale(tmdb.fallbackLang), cursor)
		if err != nil {
			log.Printf("translation backfill: select: %v", err)
			return
		}
		if item == nil {
			cursor = 0
			return
		}
		cursor = item.ID
		title, err := tmdb.FetchTitle(ctx, item.Type, item.TMDBID)
		if err != nil {
			log.Printf("translation backfill: fetch %s/%d: %v", item.Type, item.TMDBID, err)
			return
		}
		if err := store.UpsertTitle(ctx, title); err != nil {
			log.Printf("translation backfill: save %s/%d: %v", item.Type, item.TMDBID, err)
			return
		}
		log.Printf("translation backfill: refreshed %s/%d", item.Type, item.TMDBID)
	}

	process()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			process()
		}
	}
}

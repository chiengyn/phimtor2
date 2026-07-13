package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg := loadConfig()

	if cfg.TMDBAPIKey == "" {
		log.Fatal("TMDB_API_KEY is required")
	}
	if cfg.AdminPassword == "" {
		log.Fatal("ADMIN_PASSWORD is required")
	}

	store, err := NewStore(cfg.mysqlDSN())
	if err != nil {
		log.Fatalf("connect mysql: %v", err)
	}
	defer store.Close()

	if err := store.Migrate(context.Background()); err != nil {
		log.Fatalf("migrate schema: %v", err)
	}

	tmdb := NewTMDBClient(cfg.TMDBAPIKey, cfg.TMDBLanguage, cfg.TMDBFallbackLang)
	yts := NewYTSClient(cfg.YTSBaseURL)

	osc := NewOpenSubtitlesClient(
		cfg.OpenSubtitlesAPIKey,
		cfg.OpenSubtitlesUserAgent,
		cfg.OpenSubtitlesUsername,
		cfg.OpenSubtitlesPassword,
	)
	providers := map[string]SubtitleProvider{osc.Name(): osc}

	if cfg.SubSourceAPIKey != "" {
		ssc := NewSubSourceClient(cfg.SubSourceAPIKey, cfg.SubSourceUserAgent)
		providers[ssc.Name()] = ssc
	}

	blobs, blobPrimary, err := newBlobStores(cfg)
	if err != nil {
		log.Fatalf("subtitle storage: %v", err)
	}

	manager := newManagerClient(cfg.ManagerInternalURL, cfg.ManagerInternalToken)
	server := NewServer(store, tmdb, yts, providers, blobs, blobPrimary, manager, cfg.AdminUser, cfg.AdminPassword)

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: server,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Background backfill of .torrent files for magnet-only sources, from the
	// metainfo streamers have already resolved for live torrents.
	harvester := newHarvester(store, manager, time.Duration(cfg.HarvestIntervalMin)*time.Minute)
	go harvester.run(ctx)

	go func() {
		log.Printf("phimtor2-admin listening on http://%s", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
	_ = os.Stdout.Sync()
}

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

	osc := NewOpenSubtitlesClient(
		cfg.OpenSubtitlesAPIKey,
		cfg.OpenSubtitlesUserAgent,
		cfg.OpenSubtitlesUsername,
		cfg.OpenSubtitlesPassword,
	)

	server := NewServer(store, tmdb, osc, cfg.StreamerURL, cfg.AdminUser, cfg.AdminPassword)

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: server,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

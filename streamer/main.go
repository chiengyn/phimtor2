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

	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	manager, err := NewTorrentManager(StorageConfig{
		Mode:        cfg.StorageMode,
		DataDir:     cfg.DataDir,
		PrefixBytes: int64(cfg.PrefixMB) << 20,
		CacheBytes:  int64(cfg.CacheMB) << 20,
	}, int64(cfg.ReadaheadMB)<<20)
	if err != nil {
		log.Fatalf("create torrent manager: %v", err)
	}
	defer manager.Close()

	osc := NewOpenSubtitlesClient(
		cfg.OpenSubtitlesAPIKey,
		cfg.OpenSubtitlesUserAgent,
		cfg.OpenSubtitlesUsername,
		cfg.OpenSubtitlesPassword,
	)

	server := NewServer(manager, osc)

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: server,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("phimtor2 listening on http://%s", addr)
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
}

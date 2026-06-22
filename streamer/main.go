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

	server := NewServer(manager)

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
	// Stop trapping signals so a second Ctrl-C / SIGTERM force-quits immediately
	// instead of being swallowed while we drain.
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Streaming responses (handleStream) stay open for the whole playback, so a
	// graceful Shutdown blocks until they finish or the timeout fires. When that
	// happens, force-close the remaining connections so the process can exit
	// instead of being SIGQUIT-killed (which dumps every torrent goroutine).
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown timed out (%v); forcing close", err)
		_ = httpServer.Close()
	}
}

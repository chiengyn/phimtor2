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

	// The streamer no longer runs standalone: it must register with a manager.
	if cfg.ManagerURL == "" || cfg.RegisterToken == "" ||
		cfg.AdvertiseInternalURL == "" || cfg.AdvertisePublicURL == "" {
		log.Fatalf("MANAGER_URL, MANAGER_REGISTER_TOKEN, STREAMER_ADVERTISE_INTERNAL_URL and STREAMER_ADVERTISE_PUBLIC_URL are all required")
	}

	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	// The streamer's persistent identity = its control-plane credential. The
	// manager pins this token's fingerprint when an operator approves the streamer.
	controlToken, err := loadOrCreateControlToken(cfg.DataDir)
	if err != nil {
		log.Fatalf("load identity token: %v", err)
	}

	manager, err := NewTorrentManager(StorageConfig{
		Mode:        cfg.StorageMode,
		DataDir:     cfg.DataDir,
		PrefixBytes: int64(cfg.PrefixMB) << 20,
		SuffixBytes: int64(cfg.SuffixMB) << 20,
		CacheBytes:  int64(cfg.CacheMB) << 20,
		RetainHot:   cfg.RetainHot,
	}, int64(cfg.ReadaheadMB)<<20, cfg.MaxConns, time.Duration(cfg.IdleTTLMin)*time.Minute, int64(cfg.MaxUnverifiedMB)<<20, time.Duration(cfg.StallTimeoutSec)*time.Second)
	if err != nil {
		log.Fatalf("create torrent manager: %v", err)
	}
	defer manager.Close()

	server := NewServer(manager, controlToken)

	// Register with the manager and heartbeat until shutdown. The manager parks an
	// unknown streamer as pending until an operator approves it.
	reg := newManagerClient(cfg, controlToken)
	reg.Start()
	defer reg.Stop()

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: server,
		// Bound slow/idle clients so many concurrent connections can't exhaust
		// resources. No WriteTimeout: stream responses are deliberately long-lived
		// and a write deadline would cut playback off mid-file.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
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

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

	store, err := NewStore(cfg.mysqlDSN())
	if err != nil {
		log.Fatalf("connect mysql: %v", err)
	}
	defer store.Close()

	// Note: this is a read-only, public service. It never runs migrations —
	// the admin service is the sole owner of the schema.

	server, err := NewServer(store, cfg)
	if err != nil {
		log.Fatalf("build server: %v", err)
	}

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: server,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Reap watch sessions whose heartbeats stopped, dropping their torrents.
	go server.watcher.run(ctx)

	// Flush watch history on an interval. run does a final flush when ctx is
	// cancelled and only then returns; the wait after Shutdown below makes sure
	// that flush lands before the process exits, so a deploy loses nothing.
	watchLogDone := make(chan struct{})
	go func() {
		server.watchLog.run(ctx)
		close(watchLogDone)
	}()

	// Delete pairing codes nobody ever approved. A no-op without accounts.
	go server.reapDevicePairings(ctx)

	// Watch the configured chains for incoming payments and credit them. A no-op
	// when no crypto rail is configured, or when the poll interval is 0.
	go server.billing.run(ctx)

	go func() {
		log.Printf("phimtor2-viewer listening on http://%s", addr)
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
	select {
	case <-watchLogDone:
	case <-time.After(watchLogFinalTimeout + time.Second):
		log.Println("shutdown: watch history final flush did not finish")
	}
	_ = os.Stdout.Sync()
}

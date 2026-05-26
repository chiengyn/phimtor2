package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

type Config struct {
	Port        int
	DataDir     string
	ReadaheadMB int
}

func loadConfig() Config {
	cfg := Config{
		Port:        envInt("PORT", 8080),
		DataDir:     envStr("DATA_DIR", "./data"),
		ReadaheadMB: envInt("READAHEAD_MB", 16),
	}

	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Torrent data directory")
	flag.IntVar(&cfg.ReadaheadMB, "readahead", cfg.ReadaheadMB, "Streaming readahead in MB")
	flag.Parse()

	return cfg
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	cfg := loadConfig()

	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	manager, err := NewTorrentManager(cfg.DataDir, int64(cfg.ReadaheadMB)<<20)
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
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

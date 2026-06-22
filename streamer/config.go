package main

import (
	"flag"
	"os"
	"strconv"
)

type Config struct {
	Port        int
	DataDir     string
	ReadaheadMB int
	StorageMode string
	PrefixMB    int
	CacheMB     int
	MaxConns    int
	RetainHot   bool
}

func loadConfig() Config {
	cfg := Config{
		Port:        envInt("PORT", 8080),
		DataDir:     envStr("DATA_DIR", "./data"),
		ReadaheadMB: envInt("READAHEAD_MB", 16),
		StorageMode: envStr("STORAGE_MODE", StorageModePrefixCache),
		PrefixMB:    envInt("PREFIX_MB", 32),
		// CacheMB is the bounded budget for the bulk; raising it directly raises how
		// many concurrent viewers can be served without re-hitting the swarm.
		CacheMB:   envInt("CACHE_MB", 2048),
		MaxConns:  envInt("MAX_CONNS", 200),
		RetainHot: envBool("RETAIN_HOT", false),
	}

	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Torrent data directory")
	flag.IntVar(&cfg.ReadaheadMB, "readahead", cfg.ReadaheadMB, "Max streaming readahead per reader in MB (scaled down under load)")
	flag.StringVar(&cfg.StorageMode, "storage", cfg.StorageMode,
		"Storage backend: "+StorageModePrefixCache+" or "+StorageModeCappedSQLite)
	flag.IntVar(&cfg.PrefixMB, "prefix", cfg.PrefixMB, "Bytes pinned at the start of each video file, in MB")
	flag.IntVar(&cfg.CacheMB, "cache", cfg.CacheMB, "Bounded cache budget for the bulk, in MB")
	flag.IntVar(&cfg.MaxConns, "max-conns", cfg.MaxConns, "Peer connections per torrent (higher fills the cache faster)")
	flag.BoolVar(&cfg.RetainHot, "retain-hot", cfg.RetainHot,
		"Keep every piece of a torrent that has an active viewer (trades disk for concurrent capacity)")
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

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

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

	// OpenSubtitles integration (env-only, since these include secrets).
	// When OpenSubtitlesAPIKey is empty the subtitle search/download endpoints
	// report "not configured". Username/Password are optional and only raise
	// the per-day download quota.
	OpenSubtitlesAPIKey    string
	OpenSubtitlesUserAgent string
	OpenSubtitlesUsername  string
	OpenSubtitlesPassword  string
}

func loadConfig() Config {
	cfg := Config{
		Port:        envInt("PORT", 8080),
		DataDir:     envStr("DATA_DIR", "./data"),
		ReadaheadMB: envInt("READAHEAD_MB", 16),
		StorageMode: envStr("STORAGE_MODE", StorageModePrefixCache),
		PrefixMB:    envInt("PREFIX_MB", 32),
		CacheMB:     envInt("CACHE_MB", 2048),

		OpenSubtitlesAPIKey:    envStr("OPENSUBTITLES_API_KEY", ""),
		OpenSubtitlesUserAgent: envStr("OPENSUBTITLES_USER_AGENT", "phimtor2 v1.0"),
		OpenSubtitlesUsername:  envStr("OPENSUBTITLES_USERNAME", ""),
		OpenSubtitlesPassword:  envStr("OPENSUBTITLES_PASSWORD", ""),
	}

	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Torrent data directory")
	flag.IntVar(&cfg.ReadaheadMB, "readahead", cfg.ReadaheadMB, "Streaming readahead in MB")
	flag.StringVar(&cfg.StorageMode, "storage", cfg.StorageMode,
		"Storage backend: "+StorageModePrefixCache+" or "+StorageModeCappedSQLite)
	flag.IntVar(&cfg.PrefixMB, "prefix", cfg.PrefixMB, "Bytes pinned at the start of each video file, in MB")
	flag.IntVar(&cfg.CacheMB, "cache", cfg.CacheMB, "Bounded cache budget for the bulk, in MB")
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

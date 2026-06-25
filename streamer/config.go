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
	IdleTTLMin  int

	// InternalToken gates the control-plane routes (add/list/get/delete). The
	// stats/stream routes stay public. Empty disables the gate (single-streamer
	// dev: the whole API is reachable, as before).
	InternalToken string

	// Self-registration with the manager. ManagerURL empty disables it entirely,
	// preserving standalone single-streamer mode.
	ManagerURL           string
	RegisterToken        string
	InstanceID           string
	AdvertiseInternalURL string
	AdvertisePublicURL   string
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
		// Drop torrents that go unstreamed this long, freeing disk and peer
		// connections. 0 disables reaping (torrents stay until explicitly removed).
		IdleTTLMin: envInt("IDLE_TTL_MIN", 30),

		// Control-plane / manager wiring (all env-only; secrets and topology).
		InternalToken:        envStr("STREAMER_INTERNAL_TOKEN", ""),
		ManagerURL:           envStr("MANAGER_URL", ""),
		RegisterToken:        envStr("MANAGER_REGISTER_TOKEN", ""),
		InstanceID:           envStr("STREAMER_INSTANCE_ID", defaultInstanceID()),
		AdvertiseInternalURL: envStr("STREAMER_ADVERTISE_INTERNAL_URL", ""),
		AdvertisePublicURL:   envStr("STREAMER_ADVERTISE_PUBLIC_URL", ""),
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
	flag.IntVar(&cfg.IdleTTLMin, "idle-ttl", cfg.IdleTTLMin,
		"Drop torrents unstreamed for this many minutes to free disk and peers (0 disables)")
	flag.Parse()

	return cfg
}

// defaultInstanceID gives each streamer a stable-ish identity when
// STREAMER_INSTANCE_ID is unset. The hostname is unique per container under
// Kamal/Docker, which is enough for the manager to key its registry.
func defaultInstanceID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "streamer"
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

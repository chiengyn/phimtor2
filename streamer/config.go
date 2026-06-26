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

	// MaxUnverifiedMB caps in-flight/unverified bytes across ALL torrents (0 =
	// unlimited). The library defaults this to 64 MiB, but that global budget lets
	// one stalled torrent starve every other torrent of piece requests, so we
	// default it off (see NewTorrentManager).
	MaxUnverifiedMB int

	// StallTimeoutSec drops a torrent a viewer is waiting on that downloads
	// nothing for this long with no connected seeders (dead/unreachable swarm), so
	// it stops pinning a peer slot and a reader. 0 disables (see runStallChecker).
	StallTimeoutSec int

	// Self-registration with the manager (all required — the streamer no longer
	// runs standalone). RegisterToken is the shared join token gating the manager's
	// register endpoint. The streamer's control-plane credential is not configured
	// here: it is the self-generated, persisted identity token (see
	// loadOrCreateControlToken), which the manager pins on approval.
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
		// 0 disables the global cross-torrent unverified-bytes cap so a stalled
		// torrent can't starve the others of piece requests.
		MaxUnverifiedMB: envInt("MAX_UNVERIFIED_MB", 0),
		// Drop a watched torrent that makes no download progress with no seeders
		// for this many seconds (dead swarm). 0 disables.
		StallTimeoutSec: envInt("STALL_TIMEOUT_SEC", 120),

		// Control-plane / manager wiring (all env-only; secrets and topology).
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
	flag.IntVar(&cfg.MaxUnverifiedMB, "max-unverified", cfg.MaxUnverifiedMB,
		"Global in-flight/unverified bytes cap across all torrents in MB (0 disables; default off so a stalled torrent can't starve others)")
	flag.IntVar(&cfg.StallTimeoutSec, "stall-timeout", cfg.StallTimeoutSec,
		"Drop a watched torrent that downloads nothing with no seeders for this many seconds (0 disables)")
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

package main

import (
	"flag"
	"os"
	"strconv"
)

// Strategy names for MANAGER_LB_STRATEGY.
const (
	LBLeastTorrents = "least-torrents"
	LBRoundRobin    = "round-robin"
)

type Config struct {
	Port int

	// RegisterToken gates the self-registration routes (register/heartbeat/
	// deregister) — streamers send it. Empty disables the gate (dev).
	RegisterToken string

	// InternalToken gates the control routes the manager exposes to admin/viewer,
	// AND is what the manager sends to streamers' internal routes. A single shared
	// secret across the internal control plane keeps the wiring simple.
	InternalToken string

	// StreamerInternalToken is the bearer the manager sends to streamers' internal
	// routes. Defaults to InternalToken when unset (one secret for the whole
	// internal plane); split them if you want distinct admin↔manager vs
	// manager↔streamer secrets.
	StreamerInternalToken string

	HeartbeatTTLSec      int
	ReconcileIntervalSec int
	LBStrategy           string
	ForwardTimeoutSec    int
}

func loadConfig() Config {
	cfg := Config{
		Port:                  envInt("MANAGER_PORT", 8083),
		RegisterToken:         envStr("MANAGER_REGISTER_TOKEN", ""),
		InternalToken:         envStr("MANAGER_INTERNAL_TOKEN", ""),
		StreamerInternalToken: envStr("STREAMER_INTERNAL_TOKEN", ""),
		HeartbeatTTLSec:       envInt("MANAGER_HEARTBEAT_TTL", 30),
		ReconcileIntervalSec:  envInt("MANAGER_RECONCILE_INTERVAL", 60),
		LBStrategy:            envStr("MANAGER_LB_STRATEGY", LBLeastTorrents),
		ForwardTimeoutSec:     envInt("MANAGER_FORWARD_TIMEOUT", 10),
	}
	if cfg.StreamerInternalToken == "" {
		cfg.StreamerInternalToken = cfg.InternalToken
	}

	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.IntVar(&cfg.HeartbeatTTLSec, "heartbeat-ttl", cfg.HeartbeatTTLSec,
		"Seconds without a heartbeat before an instance is dropped from the registry")
	flag.IntVar(&cfg.ReconcileIntervalSec, "reconcile-interval", cfg.ReconcileIntervalSec,
		"Seconds between owner-map reconciles (fan-out list of every instance)")
	flag.StringVar(&cfg.LBStrategy, "lb-strategy", cfg.LBStrategy,
		"Load-balancing strategy: "+LBLeastTorrents+" or "+LBRoundRobin)
	flag.IntVar(&cfg.ForwardTimeoutSec, "forward-timeout", cfg.ForwardTimeoutSec,
		"Per-request timeout for control-plane calls to streamers, in seconds")
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

package main

import (
	"flag"
	"os"
	"strconv"
)

// Strategy names for MANAGER_LB_STRATEGY.
const (
	LBLeastTorrents  = "least-torrents"
	LBLeastBandwidth = "least-bandwidth"
	LBRoundRobin     = "round-robin"
)

type Config struct {
	Port int

	// RegisterToken gates the self-registration routes (register/heartbeat/
	// deregister) — streamers send it. Empty disables the gate (dev).
	RegisterToken string

	// InternalToken gates the control routes the manager exposes to admin/viewer.
	// (The token the manager sends to streamers is no longer a shared secret — it
	// is each streamer's self-generated control token, pinned on approval.)
	InternalToken string

	// StateDir holds the persisted streamer enrollment allow-list
	// (<StateDir>/enrollments.json). The manager is otherwise stateless.
	StateDir string

	HeartbeatTTLSec      int
	ReconcileIntervalSec int
	LBStrategy           string
	ForwardTimeoutSec    int
}

func loadConfig() Config {
	cfg := Config{
		Port:                 envInt("MANAGER_PORT", 8083),
		RegisterToken:        envStr("MANAGER_REGISTER_TOKEN", ""),
		InternalToken:        envStr("MANAGER_INTERNAL_TOKEN", ""),
		StateDir:             envStr("MANAGER_STATE_DIR", "./data"),
		HeartbeatTTLSec:      envInt("MANAGER_HEARTBEAT_TTL", 30),
		ReconcileIntervalSec: envInt("MANAGER_RECONCILE_INTERVAL", 60),
		LBStrategy:           envStr("MANAGER_LB_STRATEGY", LBLeastTorrents),
		ForwardTimeoutSec:    envInt("MANAGER_FORWARD_TIMEOUT", 10),
	}

	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir,
		"Directory for the persisted streamer enrollment allow-list")
	flag.IntVar(&cfg.HeartbeatTTLSec, "heartbeat-ttl", cfg.HeartbeatTTLSec,
		"Seconds without a heartbeat before an instance is dropped from the registry")
	flag.IntVar(&cfg.ReconcileIntervalSec, "reconcile-interval", cfg.ReconcileIntervalSec,
		"Seconds between owner-map reconciles (fan-out list of every instance)")
	flag.StringVar(&cfg.LBStrategy, "lb-strategy", cfg.LBStrategy,
		"Load-balancing strategy: "+LBLeastTorrents+", "+LBLeastBandwidth+", or "+LBRoundRobin)
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

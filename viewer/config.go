package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Port int

	// PublicURL is the browser-facing origin of the viewer itself (e.g.
	// "https://phimnet.online"), used to build absolute canonical / Open Graph
	// / sitemap URLs for SEO. Empty in local dev → URLs fall back to relative.
	PublicURL string

	// DiscordURL is the public invite link to the support Discord channel,
	// surfaced in the site chrome. Empty → the link is hidden.
	DiscordURL string

	// MySQL connection. DSN, when set, overrides the individual DB_* fields.
	DSN        string
	DBHost     string
	DBPort     int
	DBUser     string
	DBPassword string
	DBName     string

	// Streamer service. The viewer adds torrents server-to-server (internal URL),
	// while the browser only reaches the streamer's public stats + stream
	// endpoints (public URL, must be browser-reachable). In local dev they're
	// usually identical.
	StreamerPublicURL   string
	StreamerInternalURL string

	// Subtitle storage. The viewer reads the SAME storage the admin writes to,
	// read-only — local must point at the same directory, s3 at the same bucket.
	SubtitleStorageBackend string
	SubtitleStorageDir     string
	S3Endpoint             string
	S3Region               string
	S3Bucket               string
	S3AccessKey            string
	S3SecretKey            string
	S3UseSSL               bool
}

func loadConfig() Config {
	cfg := Config{
		Port:       envInt("VIEWER_PORT", 8082),
		PublicURL:  envStr("VIEWER_PUBLIC_URL", ""),
		DiscordURL: envStr("VIEWER_DISCORD_URL", ""),

		DSN:        envStr("MYSQL_DSN", ""),
		DBHost:     envStr("DB_HOST", "127.0.0.1"),
		DBPort:     envInt("DB_PORT", 3306),
		DBUser:     envStr("DB_USER", "phimtor"),
		DBPassword: envStr("DB_PASSWORD", ""),
		DBName:     envStr("DB_NAME", "phimtor"),

		StreamerPublicURL:   envStr("STREAMER_PUBLIC_URL", "http://localhost:8080"),
		StreamerInternalURL: envStr("STREAMER_INTERNAL_URL", "http://localhost:8080"),

		SubtitleStorageBackend: envStr("SUBTITLE_STORAGE_BACKEND", "local"),
		SubtitleStorageDir:     envStr("SUBTITLE_STORAGE_DIR", "./data/subtitles"),
		S3Endpoint:             envStr("S3_ENDPOINT", ""),
		S3Region:               envStr("S3_REGION", "us-east-1"),
		S3Bucket:               envStr("S3_BUCKET", ""),
		S3AccessKey:            envStr("S3_ACCESS_KEY", ""),
		S3SecretKey:            envStr("S3_SECRET_KEY", ""),
		S3UseSSL:               envBool("S3_USE_SSL", true),
	}

	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.PublicURL, "public-url", cfg.PublicURL, "Browser-facing origin of the viewer (for canonical/OG/sitemap URLs)")
	flag.StringVar(&cfg.DiscordURL, "discord-url", cfg.DiscordURL, "Public invite link to the support Discord channel (hidden when empty)")
	flag.StringVar(&cfg.DSN, "dsn", cfg.DSN, "MySQL DSN (overrides DB_* flags when set)")
	flag.StringVar(&cfg.DBHost, "db-host", cfg.DBHost, "MySQL host")
	flag.IntVar(&cfg.DBPort, "db-port", cfg.DBPort, "MySQL port")
	flag.StringVar(&cfg.DBUser, "db-user", cfg.DBUser, "MySQL user")
	flag.StringVar(&cfg.DBName, "db-name", cfg.DBName, "MySQL database name")
	flag.StringVar(&cfg.StreamerPublicURL, "streamer-public-url", cfg.StreamerPublicURL, "Browser-reachable base URL of the streamer (stats + stream)")
	flag.StringVar(&cfg.StreamerInternalURL, "streamer-internal-url", cfg.StreamerInternalURL, "Server-to-server base URL of the streamer (add torrent)")
	flag.StringVar(&cfg.SubtitleStorageBackend, "subtitle-storage", cfg.SubtitleStorageBackend, "Subtitle storage backend: local | s3")
	flag.StringVar(&cfg.SubtitleStorageDir, "subtitle-dir", cfg.SubtitleStorageDir, "Local subtitle storage directory (read-only, shared with admin)")
	flag.Parse()

	return cfg
}

// mysqlDSN builds the connection string from the individual DB_* fields, unless
// an explicit DSN was provided. parseTime=true so DATE/DATETIME scan into
// time.Time; charset utf8mb4 so Vietnamese text round-trips.
func (c Config) mysqlDSN() string {
	if c.DSN != "" {
		return c.DSN
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&charset=utf8mb4&loc=Local",
		c.DBUser, c.DBPassword, c.DBHost, c.DBPort, c.DBName)
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

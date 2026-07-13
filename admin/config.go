package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Port int

	// MySQL connection. DSN, when set, overrides the individual DB_* fields.
	DSN        string
	DBHost     string
	DBPort     int
	DBUser     string
	DBPassword string
	DBName     string

	// TMDB integration. APIKey is env-only (a secret) and required.
	TMDBAPIKey       string
	TMDBLanguage     string
	TMDBFallbackLang string

	// YTSBaseURL is the base of YTS's movie API, used by the manually
	// triggered crawl jobs (crawl.go) to discover movies/torrents. yts.mx
	// itself is dead (NXDOMAIN); the default points at a working mirror, but
	// this rotates over time, so check it if crawling starts failing.
	YTSBaseURL string

	// HTTP Basic auth. Password is env-only (a secret) and required.
	AdminUser     string
	AdminPassword string

	// ManagerInternalURL is the base URL of the streamer manager (control plane).
	// The admin calls it server-side to add/list/get/delete torrents; the browser
	// never talks to it. ManagerInternalToken is the shared bearer it sends.
	ManagerInternalURL   string
	ManagerInternalToken string

	// HarvestIntervalMin is how often (minutes) the background harvester backfills
	// stored .torrent bytes for magnet-only sources from the metainfo streamers
	// have already resolved for live torrents. 0 disables the harvester.
	HarvestIntervalMin int

	// OpenSubtitles integration (env-only, since these include secrets). When
	// OpenSubtitlesAPIKey is empty the subtitle search/download endpoints report
	// "not configured". Username/Password are optional and only raise the
	// per-day download quota.
	OpenSubtitlesAPIKey    string
	OpenSubtitlesUserAgent string
	OpenSubtitlesUsername  string
	OpenSubtitlesPassword  string

	// SubSource integration (env-only, since it includes a secret). When
	// SubSourceAPIKey is empty the subsource provider is not registered. The key
	// is sent in subsource's X-API-Key header.
	SubSourceAPIKey    string
	SubSourceUserAgent string

	// Subtitle storage. Saved subtitle files are written to a BlobStore selected
	// by SubtitleStorageBackend ("local" | "s3"). The local backend uses
	// SubtitleStorageDir; the s3 backend uses the S3_* settings (and is only
	// built when S3Bucket is set). S3 secrets are env-only.
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
		Port: envInt("ADMIN_PORT", 8081),

		DSN:        envStr("MYSQL_DSN", ""),
		DBHost:     envStr("DB_HOST", "127.0.0.1"),
		DBPort:     envInt("DB_PORT", 3306),
		DBUser:     envStr("DB_USER", "phimtor"),
		DBPassword: envStr("DB_PASSWORD", ""),
		DBName:     envStr("DB_NAME", "phimtor"),

		TMDBAPIKey:       envStr("TMDB_API_KEY", ""),
		TMDBLanguage:     envStr("TMDB_LANGUAGE", "vi-VN"),
		TMDBFallbackLang: envStr("TMDB_FALLBACK_LANGUAGE", "en-US"),

		YTSBaseURL: envStr("YTS_BASE_URL", "https://movies-api.accel.li/api/v2"),

		AdminUser:     envStr("ADMIN_USER", "admin"),
		AdminPassword: envStr("ADMIN_PASSWORD", ""),

		ManagerInternalURL:   envStr("MANAGER_INTERNAL_URL", "http://localhost:8083"),
		ManagerInternalToken: envStr("MANAGER_INTERNAL_TOKEN", ""),

		HarvestIntervalMin: envInt("TORRENT_HARVEST_INTERVAL_MIN", 5),

		OpenSubtitlesAPIKey:    envStr("OPENSUBTITLES_API_KEY", ""),
		OpenSubtitlesUserAgent: envStr("OPENSUBTITLES_USER_AGENT", "phimtor2 v1.0"),
		OpenSubtitlesUsername:  envStr("OPENSUBTITLES_USERNAME", ""),
		OpenSubtitlesPassword:  envStr("OPENSUBTITLES_PASSWORD", ""),

		SubSourceAPIKey:    envStr("SUBSOURCE_API_KEY", ""),
		SubSourceUserAgent: envStr("SUBSOURCE_USER_AGENT", "phimtor2 v1.0"),

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
	flag.StringVar(&cfg.DSN, "dsn", cfg.DSN, "MySQL DSN (overrides DB_* flags when set)")
	flag.StringVar(&cfg.DBHost, "db-host", cfg.DBHost, "MySQL host")
	flag.IntVar(&cfg.DBPort, "db-port", cfg.DBPort, "MySQL port")
	flag.StringVar(&cfg.DBUser, "db-user", cfg.DBUser, "MySQL user")
	flag.StringVar(&cfg.DBName, "db-name", cfg.DBName, "MySQL database name")
	flag.StringVar(&cfg.TMDBLanguage, "tmdb-language", cfg.TMDBLanguage, "Primary TMDB language")
	flag.StringVar(&cfg.TMDBFallbackLang, "tmdb-fallback", cfg.TMDBFallbackLang, "Fallback TMDB language")
	flag.StringVar(&cfg.YTSBaseURL, "yts-base-url", cfg.YTSBaseURL, "Base URL of YTS's movie API (used by the crawl jobs)")
	flag.StringVar(&cfg.AdminUser, "admin-user", cfg.AdminUser, "HTTP Basic auth user")
	flag.StringVar(&cfg.ManagerInternalURL, "manager-url", cfg.ManagerInternalURL, "Base URL of the streamer manager (control plane)")
	flag.IntVar(&cfg.HarvestIntervalMin, "harvest-interval-min", cfg.HarvestIntervalMin, "How often (minutes) to backfill .torrent files for magnet-only sources; 0 disables")
	flag.StringVar(&cfg.SubtitleStorageBackend, "subtitle-storage", cfg.SubtitleStorageBackend, "Subtitle storage backend: local | s3")
	flag.StringVar(&cfg.SubtitleStorageDir, "subtitle-dir", cfg.SubtitleStorageDir, "Local subtitle storage directory")
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

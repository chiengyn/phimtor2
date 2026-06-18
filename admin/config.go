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

	// HTTP Basic auth. Password is env-only (a secret) and required.
	AdminUser     string
	AdminPassword string
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

		AdminUser:     envStr("ADMIN_USER", "admin"),
		AdminPassword: envStr("ADMIN_PASSWORD", ""),
	}

	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.DSN, "dsn", cfg.DSN, "MySQL DSN (overrides DB_* flags when set)")
	flag.StringVar(&cfg.DBHost, "db-host", cfg.DBHost, "MySQL host")
	flag.IntVar(&cfg.DBPort, "db-port", cfg.DBPort, "MySQL port")
	flag.StringVar(&cfg.DBUser, "db-user", cfg.DBUser, "MySQL user")
	flag.StringVar(&cfg.DBName, "db-name", cfg.DBName, "MySQL database name")
	flag.StringVar(&cfg.TMDBLanguage, "tmdb-language", cfg.TMDBLanguage, "Primary TMDB language")
	flag.StringVar(&cfg.TMDBFallbackLang, "tmdb-fallback", cfg.TMDBFallbackLang, "Fallback TMDB language")
	flag.StringVar(&cfg.AdminUser, "admin-user", cfg.AdminUser, "HTTP Basic auth user")
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

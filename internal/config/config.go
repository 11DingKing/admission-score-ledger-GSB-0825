// Package config loads runtime configuration from environment variables.
package config

import (
	"os"
)

// Config holds the process configuration.
type Config struct {
	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string
	// HTTPAddr is the listen address of the HTTP server.
	HTTPAddr string
}

// Load reads configuration from the environment, applying defaults suitable
// for a local Homebrew PostgreSQL installation.
func Load() Config {
	cfg := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		HTTPAddr:    os.Getenv("HTTP_ADDR"),
	}
	if cfg.DatabaseURL == "" {
		cfg.DatabaseURL = "postgres:///admission_score_ledger?sslmode=disable"
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
	}
	return cfg
}

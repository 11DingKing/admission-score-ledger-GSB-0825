// Package config handles application configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"time"
)

// Config holds all application configuration.
type Config struct {
	DatabaseURL    string
	ServerAddr     string
	OutboxInterval time.Duration
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL environment variable is required")
	}

	addr := os.Getenv("SERVER_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	outboxInterval := 5 * time.Second
	if v := os.Getenv("OUTBOX_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid OUTBOX_INTERVAL: %w", err)
		}
		outboxInterval = d
	}

	return &Config{
		DatabaseURL:    dbURL,
		ServerAddr:     addr,
		OutboxInterval: outboxInterval,
	}, nil
}

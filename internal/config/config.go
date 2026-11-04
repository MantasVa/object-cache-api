// Package config loads and validates the application's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// Config holds every setting the application reads.
type Config struct {
	LogLevel    slog.Level
	Address     string
	Cache       Cache
	Persistence Persistence
}

// Cache configures TTL limits and the cleanup sweep.
type Cache struct {
	DefaultTTL      time.Duration
	MaxTTL          time.Duration
	CleanupInterval time.Duration
}

// Persistence configures the disk snapshot.
type Persistence struct {
	Enabled      bool
	FilePath     string
	SaveInterval time.Duration
}

// Load builds a Config from built-in defaults and the given environment lookup. Pass os.Getenv in production;
// tests pass their own function, so they never touch the real environment.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		LogLevel: slog.LevelInfo,
		Address:  ":8080",
		Cache: Cache{
			DefaultTTL:      300 * time.Second,
			MaxTTL:          3600 * time.Second,
			CleanupInterval: 30 * time.Second,
		},
		Persistence: Persistence{
			Enabled:      true,
			FilePath:     "cache-snapshot.json",
			SaveInterval: 10 * time.Second,
		},
	}

	var errs []error
	setString := func(name string, dst *string) {
		if v := getenv(name); v != "" {
			*dst = v
		}
	}
	setSeconds := func(name string, dst *time.Duration) {
		if v := getenv(name); v != "" {
			n, err := strconv.ParseInt(v, 10, 32)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				return
			}
			*dst = time.Duration(n) * time.Second
		}
	}

	if v := getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
		}
	}
	setString("SERVER_ADDRESS", &cfg.Address)
	setSeconds("CACHE_DEFAULT_TTL_SECONDS", &cfg.Cache.DefaultTTL)
	setSeconds("CACHE_MAX_TTL_SECONDS", &cfg.Cache.MaxTTL)
	setSeconds("CACHE_CLEANUP_INTERVAL_SECONDS", &cfg.Cache.CleanupInterval)
	if v := getenv("CACHE_PERSISTENCE_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("CACHE_PERSISTENCE_ENABLED: %w", err))
		}
		cfg.Persistence.Enabled = enabled
	}
	setString("CACHE_PERSISTENCE_FILE_PATH", &cfg.Persistence.FilePath)
	setSeconds("CACHE_PERSISTENCE_SAVE_INTERVAL_SECONDS", &cfg.Persistence.SaveInterval)

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate fails fast at startup, so a bad configuration never reaches a running server.
func (c Config) validate() error {
	var errs []error
	if c.Cache.DefaultTTL <= 0 {
		errs = append(errs, errors.New("CACHE_DEFAULT_TTL_SECONDS must be positive"))
	}
	if c.Cache.MaxTTL <= c.Cache.DefaultTTL {
		errs = append(errs, errors.New("CACHE_MAX_TTL_SECONDS must be greater than CACHE_DEFAULT_TTL_SECONDS"))
	}
	if c.Cache.CleanupInterval <= 0 {
		errs = append(errs, errors.New("CACHE_CLEANUP_INTERVAL_SECONDS must be positive"))
	}
	if c.Persistence.SaveInterval <= 0 {
		errs = append(errs, errors.New("CACHE_PERSISTENCE_SAVE_INTERVAL_SECONDS must be positive"))
	}
	return errors.Join(errs...)
}

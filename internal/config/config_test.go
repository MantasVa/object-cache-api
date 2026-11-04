package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address != ":8080" || cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.Cache.DefaultTTL != 300*time.Second || cfg.Cache.MaxTTL != time.Hour || cfg.Cache.CleanupInterval != 30*time.Second {
		t.Fatalf("unexpected cache defaults: %+v", cfg.Cache)
	}
	if !cfg.Persistence.Enabled || cfg.Persistence.FilePath != "cache-snapshot.json" || cfg.Persistence.SaveInterval != 10*time.Second {
		t.Fatalf("unexpected persistence defaults: %+v", cfg.Persistence)
	}
}

func TestLoad_EnvironmentOverridesDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"LOG_LEVEL":                               "debug",
		"SERVER_ADDRESS":                          ":9090",
		"CACHE_DEFAULT_TTL_SECONDS":               "10",
		"CACHE_MAX_TTL_SECONDS":                   "20",
		"CACHE_CLEANUP_INTERVAL_SECONDS":          "5",
		"CACHE_PERSISTENCE_ENABLED":               "false",
		"CACHE_PERSISTENCE_FILE_PATH":             "other.json",
		"CACHE_PERSISTENCE_SAVE_INTERVAL_SECONDS": "7",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		LogLevel:    slog.LevelDebug,
		Address:     ":9090",
		Cache:       Cache{DefaultTTL: 10 * time.Second, MaxTTL: 20 * time.Second, CleanupInterval: 5 * time.Second},
		Persistence: Persistence{Enabled: false, FilePath: "other.json", SaveInterval: 7 * time.Second},
	}
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestLoad_FailsFast(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"max equal to default", map[string]string{"CACHE_DEFAULT_TTL_SECONDS": "10", "CACHE_MAX_TTL_SECONDS": "10"}, "CACHE_MAX_TTL_SECONDS"},
		{"max below default", map[string]string{"CACHE_DEFAULT_TTL_SECONDS": "10", "CACHE_MAX_TTL_SECONDS": "5"}, "CACHE_MAX_TTL_SECONDS"},
		{"zero default ttl", map[string]string{"CACHE_DEFAULT_TTL_SECONDS": "0"}, "CACHE_DEFAULT_TTL_SECONDS"},
		{"zero cleanup interval", map[string]string{"CACHE_CLEANUP_INTERVAL_SECONDS": "0"}, "CACHE_CLEANUP_INTERVAL_SECONDS"},
		{"negative save interval", map[string]string{"CACHE_PERSISTENCE_SAVE_INTERVAL_SECONDS": "-1"}, "CACHE_PERSISTENCE_SAVE_INTERVAL_SECONDS"},
		{"not a number", map[string]string{"CACHE_MAX_TTL_SECONDS": "abc"}, "CACHE_MAX_TTL_SECONDS"},
		{"out of range", map[string]string{"CACHE_MAX_TTL_SECONDS": "99999999999"}, "CACHE_MAX_TTL_SECONDS"},
		{"not a bool", map[string]string{"CACHE_PERSISTENCE_ENABLED": "maybe"}, "CACHE_PERSISTENCE_ENABLED"},
		{"unknown log level", map[string]string{"LOG_LEVEL": "loud"}, "LOG_LEVEL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

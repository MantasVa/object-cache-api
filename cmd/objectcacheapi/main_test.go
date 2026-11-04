package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"objectcacheapi/internal/cache"
)

type fakeLoader struct {
	entries []*cache.Entry
	err     error
}

func (f fakeLoader) Load(context.Context) ([]*cache.Entry, error) { return f.entries, f.err }

func envOf(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestRestoreSnapshot_FailedLoadWarnsAndStartsEmpty(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	store := cache.NewStore(time.Minute, time.Hour)

	restoreSnapshot(t.Context(), fakeLoader{err: errors.New("corrupt file")}, store, logger)

	if len(store.Snapshot()) != 0 {
		t.Fatal("store should be empty")
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "corrupt file") {
		t.Fatalf("expected a warning naming the cause, got %q", got)
	}
}

func TestRestoreSnapshot_LoadsEntriesIntoTheStore(t *testing.T) {
	store := cache.NewStore(time.Minute, time.Hour)
	entry := cache.NewEntry("k", nil, time.Now().Add(time.Hour))

	restoreSnapshot(t.Context(), fakeLoader{entries: []*cache.Entry{entry}}, store, slog.New(slog.DiscardHandler))

	if _, err := store.Get("k"); err != nil {
		t.Fatal(err)
	}
}

// TestRun_FullLifecycle boots the whole application as main does, then checks that an entry written over HTTP is
// flushed on graceful shutdown and restored by the next run.
func TestRun_FullLifecycle(t *testing.T) {
	snapshot := filepath.Join(t.TempDir(), "snap.json")

	boot := func() (address string, shutdown func() error) {
		address = freeAddress(t)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, envOf(map[string]string{
				"SERVER_ADDRESS":                          address,
				"CACHE_PERSISTENCE_FILE_PATH":             snapshot,
				"CACHE_PERSISTENCE_SAVE_INTERVAL_SECONDS": "3600", // only the shutdown save can write the file
			}))
		}()
		waitUntilListening(t, address)

		return address, func() error {
			cancel()
			select {
			case err := <-done:
				return err
			case <-time.After(10 * time.Second):
				t.Fatal("run did not return after cancellation")
				return nil
			}
		}
	}

	address, shutdown := boot()
	resp, err := http.Post("http://"+address+"/api/v1/cache", "application/json", strings.NewReader(`{"key":"survivor","values":["a"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d", resp.StatusCode)
	}
	if err := shutdown(); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatalf("no snapshot after graceful shutdown: %v", err)
	}

	address, shutdown = boot()
	defer func() {
		if err := shutdown(); err != nil {
			t.Errorf("second run: %v", err)
		}
	}()
	resp, err = http.Get("http://" + address + "/api/v1/cache/survivor")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("entry did not survive the restart: status %d", resp.StatusCode)
	}

	resp, err = http.Get("http://" + address + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: status %d", resp.StatusCode)
	}
}

func TestRun_PersistenceDisabledDoesNotLoadOrWriteSnapshot(t *testing.T) {
	snapshot := filepath.Join(t.TempDir(), "snap.json")
	stale := `[{"Key":"stale","Values":["a"],"ExpiresAtUtc":"2999-01-01T00:00:00Z"}]`
	if err := os.WriteFile(snapshot, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	address := freeAddress(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, envOf(map[string]string{
			"SERVER_ADDRESS":              address,
			"CACHE_PERSISTENCE_ENABLED":   "false",
			"CACHE_PERSISTENCE_FILE_PATH": snapshot,
		}))
	}()
	waitUntilListening(t, address)

	resp, err := http.Get("http://" + address + "/api/v1/cache/stale")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("snapshot was loaded while persistence is disabled: status %d", resp.StatusCode)
	}
	if data, _ := os.ReadFile(snapshot); string(data) != stale {
		t.Fatalf("snapshot was rewritten while persistence is disabled: %s", data)
	}
}

func TestRun_InvalidConfigFailsBeforeServing(t *testing.T) {
	err := run(t.Context(), envOf(map[string]string{
		"CACHE_DEFAULT_TTL_SECONDS": "100",
		"CACHE_MAX_TTL_SECONDS":     "50",
	}))

	if err == nil || !strings.Contains(err.Error(), "CACHE_MAX_TTL_SECONDS") {
		t.Fatalf("err = %v, want a CACHE_MAX_TTL_SECONDS validation error", err)
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

// waitUntilListening polls a real socket. This is the one place real time is unavoidable, because the test is
// waiting on the operating system rather than on anything simulated.
func waitUntilListening(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not start listening on %s", address)
}

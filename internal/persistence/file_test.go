package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"objectcacheapi/internal/cache"
)

func entry(key, rawValue string, expiresAt time.Time) *cache.Entry {
	return cache.NewEntry(key, []json.RawMessage{json.RawMessage(rawValue)}, expiresAt)
}

func TestSaveThenLoad_RoundTripsEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache-snapshot.json")
	expiry := time.Now().UTC().Add(5 * time.Minute)
	values := []json.RawMessage{
		json.RawMessage(`"x"`), json.RawMessage(`1`), json.RawMessage(`true`), json.RawMessage(`null`),
		json.RawMessage(`{"nested":[1,2,{"a":"<b>"}]}`),
		json.RawMessage(`12345678901234567890`), // too large for a float64, so it must survive as raw text
	}

	entries := []*cache.Entry{
		cache.NewEntry("a", values, expiry),
		entry("b", `"y"`, expiry.Add(5*time.Minute)),
	}
	if err := NewFile(path).Save(t.Context(), entries); err != nil {
		t.Fatal(err)
	}

	loaded, err := NewFile(path).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d entries, want 2", len(loaded))
	}
	if loaded[0].Key() != "a" || !loaded[0].ExpiresAt().Equal(expiry) {
		t.Fatalf("loaded[0] = %q expiring %v", loaded[0].Key(), loaded[0].ExpiresAt())
	}
	got := loaded[0].Values()
	for i := range values {
		if string(got[i]) != string(values[i]) {
			t.Errorf("values[%d] = %s, want %s", i, got[i], values[i])
		}
	}
}

func TestLoad_NoFileReturnsNothingAndNoError(t *testing.T) {
	loaded, err := NewFile(filepath.Join(t.TempDir(), "missing.json")).Load(t.Context())
	if err != nil || len(loaded) != 0 {
		t.Fatalf("got (%v, %v), want no entries and no error", loaded, err)
	}
}

func TestLoad_CorruptFileReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(path, []byte("{ not valid json ]"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := NewFile(path).Load(t.Context()); err == nil {
		t.Fatal("expected an error for a corrupt snapshot")
	}
}

func TestLoad_ReadsSevenDigitFractionsAndExplicitOffsets(t *testing.T) {
	// A snapshot with PascalCase names, 7-digit fractional
	// seconds and an explicit offset.
	const snapshot = `[{"Key":"2","Values":["string"],"ExpiresAtUtc":"2026-10-19T22:40:02.9474014+00:00"},` +
		`{"Key":"1","Values":["1"],"ExpiresAtUtc":"2026-10-19T22:19:18.3656934+00:00"}]`
	path := filepath.Join(t.TempDir(), "cache-snapshot.json")
	if err := os.WriteFile(path, []byte(snapshot), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := NewFile(path).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 || loaded[0].Key() != "2" || string(loaded[0].Values()[0]) != `"string"` {
		t.Fatalf("unexpected entries: %+v", loaded)
	}
	want := time.Date(2026, 10, 19, 22, 40, 2, 947401400, time.UTC)
	if !loaded[0].ExpiresAt().Equal(want) {
		t.Fatalf("expiry = %v, want %v", loaded[0].ExpiresAt(), want)
	}
}

func TestSave_WritesTheDocumentedFieldNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache-snapshot.json")
	if err := NewFile(path).Save(t.Context(), []*cache.Entry{entry("a", `"x"`, time.Now().Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`"Key":`, `"Values":`, `"ExpiresAtUtc":`} {
		if !strings.Contains(string(data), name) {
			t.Errorf("snapshot %s does not contain %s", data, name)
		}
	}
}

func TestSave_DoesNotLeaveTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache-snapshot.json")

	if err := NewFile(path).Save(t.Context(), []*cache.Entry{entry("a", `"x"`, time.Now().Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestSave_OverwritesRatherThanAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache-snapshot.json")
	file := NewFile(path)
	expiry := time.Now().Add(time.Hour)

	if err := file.Save(t.Context(), []*cache.Entry{entry("a", `"1"`, expiry)}); err != nil {
		t.Fatal(err)
	}
	if err := file.Save(t.Context(), []*cache.Entry{entry("b", `"2"`, expiry)}); err != nil {
		t.Fatal(err)
	}

	loaded, err := NewFile(path).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Key() != "b" {
		t.Fatalf("loaded %+v, want only \"b\"", loaded)
	}
}

func TestSave_FailureKeepsThePreviousSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache-snapshot.json")
	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory at the temp path makes creating the temp file fail.
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}

	if err := NewFile(path).Save(t.Context(), nil); err == nil {
		t.Fatal("expected a save error")
	}

	data, err := os.ReadFile(path)
	if err != nil || string(data) != "[]" {
		t.Fatalf("previous snapshot was damaged: %q, %v", data, err)
	}
}

func TestSave_CancelledContextDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache-snapshot.json")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := NewFile(path).Save(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file was written despite cancellation: %v", err)
	}
}

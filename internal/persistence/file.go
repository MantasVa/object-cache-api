// Package persistence snapshots cache entries to disk.
package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"objectcacheapi/internal/cache"
)

// snapshotEntry is the on-disk record. Its field names are part of the file format, so existing snapshots stop
// loading if they change.
type snapshotEntry struct {
	Key          string            `json:"Key"`
	Values       []json.RawMessage `json:"Values"`
	ExpiresAtUTC time.Time         `json:"ExpiresAtUtc"`
}

// File stores the snapshot as one JSON file.
type File struct {
	path string
	mu   sync.Mutex // serializes saves, because they all share one temp file
}

// NewFile returns a File that reads and writes path.
func NewFile(path string) *File {
	return &File{path: path}
}

// Load returns the saved entries. A missing file is not an error: it means nothing has been saved yet.
func (f *File) Load(ctx context.Context) ([]*cache.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}

	var records []snapshotEntry
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("parse snapshot %s: %w", f.path, err)
	}

	entries := make([]*cache.Entry, 0, len(records))
	for _, r := range records {
		entries = append(entries, cache.NewEntry(r.Key, r.Values, r.ExpiresAtUTC))
	}
	return entries, nil
}

// Save atomically replaces the snapshot: it writes a temp file, flushes it to disk, then renames it over the
// real one, so a crash at any point leaves either the old snapshot or the new one, never a partial file.
func (f *File) Save(ctx context.Context, entries []*cache.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	records := make([]snapshotEntry, 0, len(entries))
	for _, e := range entries {
		records = append(records, snapshotEntry{Key: e.Key(), Values: e.Values(), ExpiresAtUTC: e.ExpiresAt()})
	}

	tempPath := f.path + ".tmp"
	if err := writeFile(tempPath, records); err != nil {
		os.Remove(tempPath) // best effort: the write failure is the error worth reporting
		return fmt.Errorf("write snapshot: %w", err)
	}
	if err := os.Rename(tempPath, f.path); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("replace snapshot: %w", err)
	}
	return nil
}

func writeFile(path string, records []snapshotEntry) (err error) {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()

	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(records); err != nil {
		return err
	}
	return file.Sync()
}

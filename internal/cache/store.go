package cache

import (
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"
)

// Errors returned by Store. Callers match them with errors.Is.
var (
	ErrKeyExists     = errors.New("cache: key already exists")
	ErrKeyNotFound   = errors.New("cache: key not found")
	ErrTTLExceedsMax = errors.New("cache: ttl exceeds maximum")
)

// Store is a concurrency-safe in-memory cache. A single RWMutex guards the map, so every compound operation
// (check, then write) is atomic. Entries are immutable, so they can be used safely after the lock is released.
type Store struct {
	mu         sync.RWMutex
	entries    map[string]*Entry
	defaultTTL time.Duration
	maxTTL     time.Duration
}

// NewStore returns an empty store with the given TTL limits.
func NewStore(defaultTTL, maxTTL time.Duration) *Store {
	return &Store{entries: make(map[string]*Entry), defaultTTL: defaultTTL, maxTTL: maxTTL}
}

// Create adds a new entry, or replaces one that has already expired. A zero ttl means the configured default.
func (s *Store) Create(key string, values []json.RawMessage, ttl time.Duration) (*Entry, error) {
	ttl = s.orDefault(ttl)
	if ttl > s.maxTTL {
		return nil, ErrTTLExceedsMax
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if current, ok := s.entries[key]; ok && !current.HasExpired(now) {
		return nil, ErrKeyExists
	}

	created := NewEntry(key, values, now.Add(ttl))
	s.entries[key] = created
	return created, nil
}

// Get returns a live entry without changing it.
func (s *Store) Get(key string) (*Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	current, ok := s.entries[key]
	if !ok || current.HasExpired(time.Now().UTC()) {
		return nil, ErrKeyNotFound
	}
	return current, nil
}

// Extend pushes a live entry's expiry forward. A zero extendBy means the configured default.
func (s *Store) Extend(key string, extendBy time.Duration) (*Entry, error) {
	extendBy = s.orDefault(extendBy)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	current, ok := s.entries[key]
	if !ok || current.HasExpired(now) {
		return nil, ErrKeyNotFound
	}

	updated := &Entry{key: key, values: current.values, expiresAt: s.extendExpiry(current.expiresAt, extendBy, now)}
	s.entries[key] = updated
	return updated, nil
}

// Append adds values to a live entry and extends its expiry. It never creates an entry. A zero extendBy means
// the configured default.
func (s *Store) Append(key string, values []json.RawMessage, extendBy time.Duration) (*Entry, error) {
	extendBy = s.orDefault(extendBy)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	current, ok := s.entries[key]
	if !ok || current.HasExpired(now) {
		return nil, ErrKeyNotFound
	}

	// The new list never shares a backing array with the old one, which another goroutine may still be reading.
	updated := &Entry{key: key, values: slices.Concat(current.values, cloneValues(values)), expiresAt: s.extendExpiry(current.expiresAt, extendBy, now)}
	s.entries[key] = updated
	return updated, nil
}

// Delete removes an entry. An expired entry the cleanup has not swept yet still counts as present.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.entries[key]; !ok {
		return ErrKeyNotFound
	}
	delete(s.entries, key)
	return nil
}

// RemoveExpired deletes all expired entries and returns how many it removed.
func (s *Store) RemoveExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	removed := 0
	for key, entry := range s.entries {
		if entry.HasExpired(now) {
			delete(s.entries, key)
			removed++
		}
	}
	return removed
}

// Snapshot returns all live entries. Sharing the pointers is safe because entries are immutable.
func (s *Store) Snapshot() []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now().UTC()
	entries := make([]*Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		if !entry.HasExpired(now) {
			entries = append(entries, entry)
		}
	}
	return entries
}

// Load adds entries restored from a snapshot, skipping expired ones and keys that are already present.
func (s *Store) Load(entries []*Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	for _, entry := range entries {
		if _, exists := s.entries[entry.key]; !exists && !entry.HasExpired(now) {
			s.entries[entry.key] = entry
		}
	}
}

func (s *Store) orDefault(d time.Duration) time.Duration {
	if d == 0 {
		return s.defaultTTL
	}
	return d
}

// extendExpiry adds onto the current expiry, so repeated extensions accumulate rather than overwrite each
// other, but never lets the remaining TTL exceed the configured max.
func (s *Store) extendExpiry(currentExpiry time.Time, extendBy time.Duration, now time.Time) time.Time {
	extended := currentExpiry.Add(extendBy)
	if ceiling := now.Add(s.maxTTL); extended.After(ceiling) {
		return ceiling
	}
	return extended
}

// Package cache holds the domain model and the in-memory, concurrency-safe cache store.
package cache

import (
	"bytes"
	"encoding/json"
	"time"
)

// Entry is an immutable cache record. Its fields are unexported and it has no setters, so every state change
// has to build a new Entry. The store relies on that: it hands out *Entry pointers that callers read after the
// store's lock is released, which is only safe because nothing can change an Entry once it exists.
type Entry struct {
	key       string
	values    []json.RawMessage
	expiresAt time.Time
}

// NewEntry builds an Entry, copying values so that later changes to the caller's data cannot reach it.
func NewEntry(key string, values []json.RawMessage, expiresAt time.Time) *Entry {
	return &Entry{key: key, values: cloneValues(values), expiresAt: expiresAt}
}

// Key returns the entry's key.
func (e *Entry) Key() string { return e.key }

// Values returns a deep copy of the stored values, so callers cannot change what the store holds.
func (e *Entry) Values() []json.RawMessage { return cloneValues(e.values) }

// ExpiresAt returns the moment the entry expires, in UTC.
func (e *Entry) ExpiresAt() time.Time { return e.expiresAt }

// HasExpired reports whether the entry has expired at the given moment. Expiry is inclusive.
func (e *Entry) HasExpired(now time.Time) bool { return !now.Before(e.expiresAt) }

// cloneValues copies both the list and each value's bytes, because json.RawMessage is itself a byte slice.
func cloneValues(values []json.RawMessage) []json.RawMessage {
	if values == nil {
		return nil
	}
	cloned := make([]json.RawMessage, len(values))
	for i, value := range values {
		cloned[i] = bytes.Clone(value)
	}
	return cloned
}

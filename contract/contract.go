// Package contract is the wire format of the Object Cache API: everything a client needs and nothing else.
//
// It must only ever depend on the standard library - never on net/http handlers or the internal cache types.
package contract

import (
	"encoding/json"
	"time"
)

// Request-size bounds shared by the request types and the GET route parameter.
const (
	MaxKeyLength        = 256
	MaxValuesPerRequest = 1000
)

// CreateRequest is the body of POST /api/v1/cache.
type CreateRequest struct {
	// Key to store the values under. Must not already exist.
	Key string `json:"key"`
	// Values to store. Each item is arbitrary JSON, kept as raw bytes so it round-trips exactly.
	Values []json.RawMessage `json:"values"`
	// TTLSeconds is optional. It falls back to the configured default and may not exceed the configured max.
	TTLSeconds *int32 `json:"ttlSeconds,omitempty"`
}

// AppendRequest is the body of PATCH /api/v1/cache.
type AppendRequest struct {
	// Key of the existing entry to append to.
	Key string `json:"key"`
	// Values to append to the existing entry.
	Values []json.RawMessage `json:"values"`
	// TTLSeconds is the optional extension. It falls back to the configured default.
	TTLSeconds *int32 `json:"ttlSeconds,omitempty"`
}

// DeleteRequest is the body of DELETE /api/v1/cache.
type DeleteRequest struct {
	// Key of the entry to delete.
	Key string `json:"key"`
}

// EntryResponse is returned by the create, get and append endpoints.
type EntryResponse struct {
	Key          string            `json:"key"`
	Values       []json.RawMessage `json:"values"`
	ExpiresAtUTC time.Time         `json:"expiresAtUtc"`
}

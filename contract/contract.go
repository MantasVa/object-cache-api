// Package contract is the wire format of the Object Cache API: everything a client needs and nothing else.
//
// It must only ever depend on the standard library - never on net/http handlers or the internal cache types.
//
// The field comments and struct tags feed the generated Swagger document, and only swag reads them. That includes the
// validate tags: the rules are enforced by the Validate methods.
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
	Key string `json:"key" validate:"required" minLength:"1" maxLength:"256"`
	// Values to store. Each item can be any JSON value, and comes back exactly as it was sent.
	Values []json.RawMessage `json:"values" validate:"required,min=1,max=1000" swaggertype:"array,object"`
	// Optional TTL in seconds. Falls back to the configured default, and may not exceed the configured max.
	TTLSeconds *int32 `json:"ttlSeconds,omitempty" minimum:"1" maximum:"2147483647"`
}

// AppendRequest is the body of PATCH /api/v1/cache.
type AppendRequest struct {
	// Key of the existing entry to append to.
	Key string `json:"key" validate:"required" minLength:"1" maxLength:"256"`
	// Values to append to the existing entry. Each item can be any JSON value.
	Values []json.RawMessage `json:"values" validate:"required,min=1,max=1000" swaggertype:"array,object"`
	// Optional number of seconds to extend the TTL by. Falls back to the configured default.
	TTLSeconds *int32 `json:"ttlSeconds,omitempty" minimum:"1" maximum:"2147483647"`
}

// DeleteRequest is the body of DELETE /api/v1/cache.
type DeleteRequest struct {
	// Key of the entry to delete.
	Key string `json:"key" validate:"required" minLength:"1" maxLength:"256"`
}

// EntryResponse is returned by the create, get and append endpoints.
type EntryResponse struct {
	Key          string            `json:"key" validate:"required"`
	Values       []json.RawMessage `json:"values" validate:"required" swaggertype:"array,object"`
	ExpiresAtUTC time.Time         `json:"expiresAtUtc" validate:"required" format:"date-time"`
}

package contract

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf16"
)

// ValidationErrors maps a field name to what is wrong with it. An empty map means the input is valid.
type ValidationErrors map[string][]string

func (v ValidationErrors) add(field, message string) {
	v[field] = append(v[field], message)
}

// Validate checks the request's shape. Rules that need server state (such as the max TTL) belong to the store.
func (r CreateRequest) Validate() ValidationErrors {
	errs := ValidationErrors{}
	validateBodyKey(errs, r.Key)
	validateValues(errs, r.Values == nil, len(r.Values))
	ValidateTTL(errs, "TtlSeconds", r.TTLSeconds)
	return errs
}

// Validate checks the request's shape. Rules that need server state belong to the store.
func (r AppendRequest) Validate() ValidationErrors {
	errs := ValidationErrors{}
	validateBodyKey(errs, r.Key)
	validateValues(errs, r.Values == nil, len(r.Values))
	ValidateTTL(errs, "TtlSeconds", r.TTLSeconds)
	return errs
}

// Validate checks the request's shape.
func (r DeleteRequest) Validate() ValidationErrors {
	errs := ValidationErrors{}
	validateBodyKey(errs, r.Key)
	return errs
}

// ValidateRouteKey checks a key taken from the URL.
func ValidateRouteKey(errs ValidationErrors, field, key string) {
	if strings.TrimSpace(key) == "" {
		errs.add(field, fmt.Sprintf("The %s field is required.", field))
		return
	}
	validateKeyLength(errs, field, key)
}

// ValidateTTL checks an optional TTL. A nil TTL is valid and means "use the configured default".
func ValidateTTL(errs ValidationErrors, field string, ttl *int32) {
	if ttl != nil && *ttl < 1 {
		errs.add(field, fmt.Sprintf("The field %s must be between 1 and %d.", field, math.MaxInt32))
	}
}

func validateBodyKey(errs ValidationErrors, key string) {
	if strings.TrimSpace(key) == "" {
		errs.add("Key", "The Key field is required.")
	}
	validateKeyLength(errs, "Key", key)
}

func validateKeyLength(errs ValidationErrors, field, key string) {
	if n := utf16Len(key); n < 1 || n > MaxKeyLength {
		errs.add(field, fmt.Sprintf("The field %s must be a string with a minimum length of 1 and a maximum length of %d.", field, MaxKeyLength))
	}
}

func validateValues(errs ValidationErrors, missing bool, count int) {
	switch {
	case missing:
		errs.add("Values", "The Values field is required.")
	case count < 1:
		errs.add("Values", "The field Values must be a string or array type with a minimum length of '1'.")
	case count > MaxValuesPerRequest:
		errs.add("Values", fmt.Sprintf("The field Values must be a string or array type with a maximum length of '%d'.", MaxValuesPerRequest))
	}
}

// utf16Len measures a string in UTF-16 code units, which is the unit MaxKeyLength is defined in.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if width := utf16.RuneLen(r); width > 0 {
			n += width
		} else {
			n++
		}
	}
	return n
}

// Package api is the thin REST layer: it translates HTTP requests into store calls and store results into
// status codes and problem details. Business rules such as TTLs, expiry and concurrency belong to the store.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"objectcacheapi/contract"
	"objectcacheapi/internal/cache"
)

// Store is what the REST layer needs from the storage engine. It is declared here, by its consumer, so a
// different backend (such as Redis or SQL) can be swapped in without touching this package. A zero duration
// means "use the configured default".
type Store interface {
	Create(key string, values []json.RawMessage, ttl time.Duration) (*cache.Entry, error)
	Get(key string) (*cache.Entry, error)
	Extend(key string, extendBy time.Duration) (*cache.Entry, error)
	Append(key string, values []json.RawMessage, extendBy time.Duration) (*cache.Entry, error)
	Delete(key string) error
}

// cachePath is the versioned collection route. A breaking change gets a new version alongside this one.
const cachePath = "/api/v1/cache"

type handlers struct {
	store  Store
	logger *slog.Logger
}

// route is one documented endpoint. Every route listed here needs swag annotations on its handler, which a test
// enforces by comparing this list with the generated document.
type route struct {
	method  string
	path    string
	handler http.HandlerFunc
}

func (h *handlers) routes() []route {
	return []route{
		{http.MethodGet, cachePath + "/{key}", h.get},
		{http.MethodPost, cachePath, h.create},
		{http.MethodPatch, cachePath, h.append},
		{http.MethodDelete, cachePath, h.delete},
		{http.MethodGet, "/healthz", healthz},
	}
}

// NewHandler builds the application's complete HTTP handler, including the OpenAPI document and Swagger UI.
func NewHandler(store Store, logger *slog.Logger) http.Handler {
	h := &handlers{store: store, logger: logger}

	mux := http.NewServeMux()
	for _, rt := range h.routes() {
		mux.HandleFunc(rt.method+" "+rt.path, rt.handler)
	}
	registerSwagger(mux)

	return recoverPanics(logger, ignoreTrailingSlash(mux))
}

// healthz answers whether this process can serve requests. It depends on nothing external, because the cache
// lives in memory, and the snapshot is restored before the server starts listening.
//
// @Summary Check service health
// @Description Reports whether the service can handle requests.
// @Tags health
// @Produce json
// @Success 200 {object} map[string]string "The service is healthy"
// @Router /healthz [get]
func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// get returns a live entry, and extends its TTL when the request asks for that.
//
// @Summary Get an entry
// @Description Returns an entry if it exists and hasn't expired. With extendTtl=true, it also adds to the entry's expiry, up to the configured maximum.
// @Tags cache
// @Produce json
// @Produce application/problem+json
// @Param key path string true "Cache key" minlength(1) maxlength(256)
// @Param extendTtl query bool false "When true, extends the entry's TTL as a side effect of the read"
// @Param ttlSeconds query int false "Seconds to extend by when extendTtl is true. Defaults to the configured default TTL" minimum(1) maximum(2147483647)
// @Success 200 {object} contract.EntryResponse "Entry found"
// @Failure 400 {object} problem "Validation failed"
// @Failure 404 {object} problem "No entry for this key, or it has expired"
// @Failure 500 {object} problem "Unexpected server error"
// @Router /api/v1/cache/{key} [get]
func (h *handlers) get(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	errs := contract.ValidationErrors{}
	contract.ValidateRouteKey(errs, "key", key)
	extend := parseBoolQuery(r, "extendTtl", errs)
	ttl := parseTTLQuery(r, "ttlSeconds", errs)
	if len(errs) > 0 {
		writeValidationProblem(w, errs)
		return
	}

	var (
		entry *cache.Entry
		err   error
	)
	if extend {
		entry, err = h.store.Extend(key, toDuration(ttl))
	} else {
		entry, err = h.store.Get(key)
	}
	if err != nil {
		h.writeStoreError(w, r, err, key, ttl)
		return
	}

	writeJSON(w, http.StatusOK, toResponse(entry))
}

// create adds a new entry.
//
// @Summary Create an entry
// @Description Creates a new entry. Fails if the key already exists and hasn't expired.
// @Tags cache
// @Accept json
// @Produce json
// @Produce application/problem+json
// @Param request body contract.CreateRequest true "Key, values and optional TTL"
// @Success 201 {object} contract.EntryResponse "Entry created"
// @Header 201 {string} Location "URL of the created entry"
// @Failure 400 {object} problem "Validation failed, or the TTL exceeds the configured maximum"
// @Failure 409 {object} problem "An entry already exists for this key"
// @Failure 413 {object} problem "Request body too large"
// @Failure 415 {object} problem "Request body is not JSON"
// @Failure 500 {object} problem "Unexpected server error"
// @Router /api/v1/cache [post]
func (h *handlers) create(w http.ResponseWriter, r *http.Request) {
	var req contract.CreateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if errs := req.Validate(); len(errs) > 0 {
		writeValidationProblem(w, errs)
		return
	}

	entry, err := h.store.Create(req.Key, req.Values, toDuration(req.TTLSeconds))
	if err != nil {
		h.writeStoreError(w, r, err, req.Key, req.TTLSeconds)
		return
	}

	w.Header().Set("Location", locationOf(r, req.Key))
	writeJSON(w, http.StatusCreated, toResponse(entry))
}

// append adds values to an existing entry and extends its TTL.
//
// @Summary Append to an entry
// @Description Appends values to an existing entry and extends its TTL. Never creates an entry.
// @Tags cache
// @Accept json
// @Produce json
// @Produce application/problem+json
// @Param request body contract.AppendRequest true "Key, values to append and optional TTL extension"
// @Success 200 {object} contract.EntryResponse "Entry updated"
// @Failure 400 {object} problem "Validation failed"
// @Failure 404 {object} problem "No entry for this key, or it has expired"
// @Failure 413 {object} problem "Request body too large"
// @Failure 415 {object} problem "Request body is not JSON"
// @Failure 500 {object} problem "Unexpected server error"
// @Router /api/v1/cache [patch]
func (h *handlers) append(w http.ResponseWriter, r *http.Request) {
	var req contract.AppendRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if errs := req.Validate(); len(errs) > 0 {
		writeValidationProblem(w, errs)
		return
	}

	entry, err := h.store.Append(req.Key, req.Values, toDuration(req.TTLSeconds))
	if err != nil {
		h.writeStoreError(w, r, err, req.Key, req.TTLSeconds)
		return
	}

	writeJSON(w, http.StatusOK, toResponse(entry))
}

// delete removes an entry.
//
// @Summary Delete an entry
// @Description Deletes an entry by key.
// @Tags cache
// @Accept json
// @Produce application/problem+json
// @Param request body contract.DeleteRequest true "Key of the entry to delete"
// @Success 204 "Entry deleted"
// @Failure 400 {object} problem "Validation failed"
// @Failure 404 {object} problem "No entry for this key"
// @Failure 413 {object} problem "Request body too large"
// @Failure 415 {object} problem "Request body is not JSON"
// @Failure 500 {object} problem "Unexpected server error"
// @Router /api/v1/cache [delete]
func (h *handlers) delete(w http.ResponseWriter, r *http.Request) {
	var req contract.DeleteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if errs := req.Validate(); len(errs) > 0 {
		writeValidationProblem(w, errs)
		return
	}

	if err := h.store.Delete(req.Key); err != nil {
		h.writeStoreError(w, r, err, req.Key, nil)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// writeStoreError maps store errors to responses. Anything it does not recognize is a bug or an outage: the
// real error is logged, and the client gets a generic 500 with no internal details.
func (h *handlers) writeStoreError(w http.ResponseWriter, r *http.Request, err error, key string, ttl *int32) {
	switch {
	case errors.Is(err, cache.ErrKeyNotFound):
		writeProblem(w, problem{
			Status: http.StatusNotFound,
			Title:  "Key not found",
			Detail: fmt.Sprintf("No entry found for key '%s' or the key has expired.", key),
		})
	case errors.Is(err, cache.ErrKeyExists):
		writeProblem(w, problem{
			Status: http.StatusConflict,
			Title:  "Key already exists",
			Detail: fmt.Sprintf("An entry for key '%s' already exists.", key),
		})
	case errors.Is(err, cache.ErrTTLExceedsMax):
		writeProblem(w, problem{
			Status: http.StatusBadRequest,
			Title:  "TTL exceeds maximum",
			Detail: fmt.Sprintf("Requested TTL of %ss exceeds the configured maximum.", ttlText(ttl)),
		})
	default:
		h.logger.Error("unhandled store error", "method", r.Method, "path", r.URL.Path, "error", err)
		writeInternalError(w)
	}
}

func toResponse(e *cache.Entry) contract.EntryResponse {
	return contract.EntryResponse{Key: e.Key(), Values: e.Values(), ExpiresAtUTC: e.ExpiresAt()}
}

// toDuration converts an optional TTL. Nil becomes zero, which the store reads as "use the configured default".
func toDuration(seconds *int32) time.Duration {
	if seconds == nil {
		return 0
	}
	return time.Duration(*seconds) * time.Second
}

func ttlText(seconds *int32) string {
	if seconds == nil {
		return ""
	}
	return strconv.Itoa(int(*seconds))
}

// locationOf builds the absolute URL of the created resource.
func locationOf(r *http.Request, key string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + cachePath + "/" + url.PathEscape(key)
}

// parseBoolQuery accepts only "true" and "false" in any case. strconv.ParseBool is too lenient here, because
// it would also accept "1" and "t", which this API rejects with a 400.
func parseBoolQuery(r *http.Request, name string, errs contract.ValidationErrors) bool {
	raw, present := queryValue(r, name)
	if !present {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		return true
	case "false":
		return false
	case "":
		errs[name] = append(errs[name], "The value '' is invalid.")
		return false
	default:
		errs[name] = append(errs[name], fmt.Sprintf("The value '%s' is not valid.", raw))
		return false
	}
}

// queryValue looks a parameter up by name, ignoring case, and returns its first value. url.Values is
// case-sensitive, so an exact lookup would silently ignore ?EXTENDTTL=true.
func queryValue(r *http.Request, name string) (string, bool) {
	for key, values := range r.URL.Query() {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0], true
		}
	}
	return "", false
}

func parseTTLQuery(r *http.Request, name string, errs contract.ValidationErrors) *int32 {
	raw, _ := queryValue(r, name)
	if raw == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		errs[name] = append(errs[name], fmt.Sprintf("The value '%s' is not valid.", raw))
		return nil
	}
	ttl := int32(parsed)
	contract.ValidateTTL(errs, name, &ttl)
	return &ttl
}

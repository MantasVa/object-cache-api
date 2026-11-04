package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"runtime/debug"
	"strings"

	"objectcacheapi/contract"
)

// maxBodyBytes bounds a request's total size.
const maxBodyBytes = 30_000_000

// maxBodyDepth bounds how deeply a request body may nest, counting the request object itself. It is much stricter
// than encoding/json's own limit.
const maxBodyDepth = 32

// problem is an RFC 9457 problem details body, the only error shape this API returns.
type problem struct {
	Type   string                    `json:"type"`
	Title  string                    `json:"title"`
	Status int                       `json:"status"`
	Detail string                    `json:"detail,omitempty"`
	Errors contract.ValidationErrors `json:"errors,omitempty"`
}

var problemTypes = map[int]string{
	http.StatusBadRequest:            "https://tools.ietf.org/html/rfc9110#section-15.5.1",
	http.StatusNotFound:              "https://tools.ietf.org/html/rfc9110#section-15.5.5",
	http.StatusConflict:              "https://tools.ietf.org/html/rfc9110#section-15.5.10",
	http.StatusRequestEntityTooLarge: "https://tools.ietf.org/html/rfc9110#section-15.5.14",
	http.StatusUnsupportedMediaType:  "https://tools.ietf.org/html/rfc9110#section-15.5.16",
	http.StatusInternalServerError:   "https://tools.ietf.org/html/rfc9110#section-15.6.1",
}

func writeProblem(w http.ResponseWriter, p problem) {
	p.Type = problemTypes[p.Status]
	writeBody(w, "application/problem+json", p.Status, p)
}

func writeValidationProblem(w http.ResponseWriter, errs contract.ValidationErrors) {
	writeProblem(w, problem{
		Status: http.StatusBadRequest,
		Title:  "One or more validation errors occurred.",
		Errors: errs,
	})
}

// writeInternalError deliberately takes no error, so internal details cannot reach the client through it.
func writeInternalError(w http.ResponseWriter) {
	writeProblem(w, problem{
		Status: http.StatusInternalServerError,
		Title:  "An error occurred while processing your request.",
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	writeBody(w, "application/json; charset=utf-8", status, body)
}

func writeBody(w http.ResponseWriter, contentType string, status int, body any) {
	// Encode before touching the response, so an encoding failure can still become a proper 500.
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		panic(err) // the response types always encode, so this is a bug; recoverPanics turns it into a 500
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	w.Write(buf.Bytes()) // a write error means the client has gone, and nothing useful can be done about that
}

// decodeBody reads a JSON request body into dst. If it fails, it has already written the error response and
// returns false, so the caller only has to return.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeProblem(w, problem{
			Status: http.StatusUnsupportedMediaType,
			Title:  "Unsupported Media Type",
		})
		return false
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, problem{
				Status: http.StatusRequestEntityTooLarge,
				Title:  "Request body too large",
				Detail: "The request body exceeds the maximum allowed size.",
			})
			return false
		}
		writeBadBody(w, "The request body could not be read.")
		return false
	}

	if len(bytes.TrimSpace(data)) == 0 {
		writeBadBody(w, "A non-empty request body is required.")
		return false
	}
	// Unmarshal rather than a streaming Decoder, because Unmarshal also rejects data after the JSON document.
	if err := json.Unmarshal(data, dst); err != nil {
		writeBadBody(w, "The request body is not valid JSON or does not match the expected shape.")
		return false
	}
	if jsonDepth(data) > maxBodyDepth {
		writeBadBody(w, fmt.Sprintf("The maximum configured depth of %d has been exceeded.", maxBodyDepth))
		return false
	}
	return true
}

// jsonDepth returns how deeply a valid JSON document nests. Brackets inside strings do not count.
func jsonDepth(data []byte) int {
	depth, deepest := 0, 0
	inString, escaped := false, false
	for _, b := range data {
		switch {
		case escaped:
			escaped = false
		case inString:
			escaped = b == '\\'
			inString = b != '"'
		case b == '"':
			inString = true
		case b == '{' || b == '[':
			depth++
			deepest = max(deepest, depth)
		case b == '}' || b == ']':
			depth--
		}
	}
	return deepest
}

func writeBadBody(w http.ResponseWriter, message string) {
	writeValidationProblem(w, contract.ValidationErrors{"$": {message}})
}

func isJSONContentType(header string) bool {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	return mediaType == "application/json" || mediaType == "text/json" ||
		(strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json"))
}

// ignoreTrailingSlash makes /api/v1/cache/ equivalent to /api/v1/cache, which net/http's ServeMux does not do by
// itself. It checks the escaped path because an encoded slash (%2F) is part of a key, not a trailing slash.
func ignoreTrailingSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path := r.URL.Path; len(path) > 1 && strings.HasSuffix(r.URL.EscapedPath(), "/") {
			// A clone, because a handler must not modify the request it was given.
			r = r.Clone(r.Context())
			r.URL.Path = strings.TrimSuffix(path, "/")
			r.URL.RawPath = strings.TrimSuffix(r.URL.RawPath, "/")
		}
		next.ServeHTTP(w, r)
	})
}

// recoverPanics is the global exception handler: a panic anywhere below it is logged with its stack and becomes
// a generic 500. Without it, net/http would just drop the connection and the client would get no response.
func recoverPanics(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			// ErrAbortHandler is net/http's own signal to abort a response, so it has to keep propagating.
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			logger.Error("panic while handling request",
				"method", r.Method, "path", r.URL.Path, "panic", recovered, "stack", string(debug.Stack()))
			writeInternalError(w)
		}()
		next.ServeHTTP(w, r)
	})
}

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"objectcacheapi/contract"
	"objectcacheapi/internal/cache"
)

// client is a thin wrapper over the HTTP API, so tests read as API calls rather than raw net/http plumbing.
type client struct {
	t    *testing.T
	base string
}

// newClient runs the real handler with a real store on a real local listener.
func newClient(t *testing.T) *client {
	t.Helper()
	store := cache.NewStore(300*time.Second, 3600*time.Second)
	return newClientFor(t, store, slog.New(slog.DiscardHandler))
}

func newClientFor(t *testing.T, store Store, logger *slog.Logger) *client {
	t.Helper()
	server := httptest.NewServer(NewHandler(store, logger))
	t.Cleanup(server.Close)
	return &client{t: t, base: server.URL}
}

type result struct {
	status int
	header http.Header
	body   string
}

func (c *client) do(method, path, contentType, body string) result {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	httpClient := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := httpClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return result{status: resp.StatusCode, header: resp.Header, body: string(data)}
}

func (c *client) sendJSON(method string, payload any) result {
	c.t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.do(method, "/api/v1/cache", "application/json", string(data))
}

func (c *client) create(key string, ttl *int32, items ...string) result {
	c.t.Helper()
	return c.sendJSON(http.MethodPost, contract.CreateRequest{Key: key, Values: raw(items...), TTLSeconds: ttl})
}

func (c *client) appendTo(key string, ttl *int32, items ...string) result {
	c.t.Helper()
	return c.sendJSON(http.MethodPatch, contract.AppendRequest{Key: key, Values: raw(items...), TTLSeconds: ttl})
}

func (c *client) deleteKey(key string) result {
	c.t.Helper()
	return c.sendJSON(http.MethodDelete, contract.DeleteRequest{Key: key})
}

func (c *client) get(key, query string) result {
	c.t.Helper()
	return c.do(http.MethodGet, "/api/v1/cache/"+url.PathEscape(key)+query, "", "")
}

func raw(items ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(items))
	for i, item := range items {
		out[i] = json.RawMessage(strconv.Quote(item))
	}
	return out
}

func ttl(seconds int32) *int32 { return &seconds }

func repeat(item string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = item
	}
	return out
}

func wantStatus(t *testing.T, r result, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status = %d, want %d; body = %s", r.status, want, r.body)
	}
}

func readEntry(t *testing.T, r result) contract.EntryResponse {
	t.Helper()
	var entry contract.EntryResponse
	if err := json.Unmarshal([]byte(r.body), &entry); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return entry
}

func readProblem(t *testing.T, r result) problem {
	t.Helper()
	if got := r.header.Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
	var p problem
	if err := json.Unmarshal([]byte(r.body), &p); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return p
}

func TestCreate_Succeeds(t *testing.T) {
	c := newClient(t)

	r := c.create("k1", nil, "a", "b", "c")

	wantStatus(t, r, http.StatusCreated)
	if got := r.header.Get("Location"); got != c.base+"/api/v1/cache/k1" {
		t.Fatalf("Location = %q", got)
	}
	entry := readEntry(t, r)
	if entry.Key != "k1" || len(entry.Values) != 3 {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if entry.ExpiresAtUTC.Location() != time.UTC {
		t.Fatalf("expiry is not UTC: %v", entry.ExpiresAtUTC)
	}
}

func TestCreate_LocationCanBeFollowedForAKeyWithReservedCharacters(t *testing.T) {
	c := newClient(t)
	const key = "a/b c?d"

	created := c.create(key, nil, "x")
	wantStatus(t, created, http.StatusCreated)

	location := created.header.Get("Location")
	resp, err := http.Get(location)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", location, resp.StatusCode)
	}
}

func TestCreate_DuplicateKeyReturns409(t *testing.T) {
	c := newClient(t)
	c.create("k1", nil, "a")

	r := c.create("k1", nil, "b")

	wantStatus(t, r, http.StatusConflict)
	if p := readProblem(t, r); p.Title != "Key already exists" || p.Status != http.StatusConflict {
		t.Fatalf("unexpected problem: %+v", p)
	}
}

func TestRequestValidation(t *testing.T) {
	over := repeat("a", contract.MaxValuesPerRequest+1)
	longKey := strings.Repeat("k", contract.MaxKeyLength+1)

	tests := []struct {
		name   string
		method string
		body   string
	}{
		{"create ttl above max", http.MethodPost, `{"key":"k","values":["a"],"ttlSeconds":999999}`},
		{"create empty values", http.MethodPost, `{"key":"k","values":[]}`},
		{"create missing values", http.MethodPost, `{"key":"k"}`},
		{"create null values", http.MethodPost, `{"key":"k","values":null}`},
		{"create empty key", http.MethodPost, `{"key":"","values":["a"]}`},
		{"create missing key", http.MethodPost, `{"values":["a"]}`},
		{"create whitespace key", http.MethodPost, `{"key":"   ","values":["a"]}`},
		{"create key too long", http.MethodPost, fmt.Sprintf(`{"key":%q,"values":["a"]}`, longKey)},
		{"create too many values", http.MethodPost, mustJSON(t, contract.CreateRequest{Key: "k", Values: raw(over...)})},
		{"create negative ttl", http.MethodPost, `{"key":"k","values":["a"],"ttlSeconds":-1}`},
		{"create zero ttl", http.MethodPost, `{"key":"k","values":["a"],"ttlSeconds":0}`},
		{"create ttl overflows int32", http.MethodPost, `{"key":"k","values":["a"],"ttlSeconds":2147483648}`},
		{"create fractional ttl", http.MethodPost, `{"key":"k","values":["a"],"ttlSeconds":1.5}`},
		{"create ttl as string", http.MethodPost, `{"key":"k","values":["a"],"ttlSeconds":"5"}`},
		{"create key of wrong type", http.MethodPost, `{"key":5,"values":["a"]}`},
		{"create values of wrong type", http.MethodPost, `{"key":"k","values":"a"}`},
		{"create malformed json", http.MethodPost, `{ nope`},
		{"create trailing data", http.MethodPost, `{"key":"k","values":["a"]} {"more":1}`},
		{"create empty body", http.MethodPost, ``},
		{"append too many values", http.MethodPatch, mustJSON(t, contract.AppendRequest{Key: "k", Values: raw(over...)})},
		{"append negative ttl", http.MethodPatch, `{"key":"k","values":["b"],"ttlSeconds":-1}`},
		{"append empty values", http.MethodPatch, `{"key":"k","values":[]}`},
		{"delete key too long", http.MethodDelete, fmt.Sprintf(`{"key":%q}`, longKey)},
		{"delete empty key", http.MethodDelete, `{"key":""}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t)
			r := c.do(tt.method, "/api/v1/cache", "application/json", tt.body)
			wantStatus(t, r, http.StatusBadRequest)
			if p := readProblem(t, r); p.Status != http.StatusBadRequest {
				t.Fatalf("unexpected problem: %+v", p)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestValidation_ReportsEveryInvalidField(t *testing.T) {
	c := newClient(t)

	r := c.do(http.MethodPost, "/api/v1/cache", "application/json", `{"key":"","values":[],"ttlSeconds":-1}`)

	wantStatus(t, r, http.StatusBadRequest)
	p := readProblem(t, r)
	for _, field := range []string{"Key", "Values", "TtlSeconds"} {
		if len(p.Errors[field]) == 0 {
			t.Errorf("no error reported for %s: %+v", field, p.Errors)
		}
	}
}

func TestKeyLength_IsMeasuredInUTF16CodeUnits(t *testing.T) {
	c := newClient(t)
	// Each emoji is 1 rune and 4 bytes, but 2 UTF-16 code units, so 128 fit exactly and 129 do not.
	wantStatus(t, c.create(strings.Repeat("😀", 128), nil, "a"), http.StatusCreated)
	wantStatus(t, c.create(strings.Repeat("😀", 129), nil, "a"), http.StatusBadRequest)
}

func TestBody_WrongContentTypeReturns415(t *testing.T) {
	c := newClient(t)

	r := c.do(http.MethodPost, "/api/v1/cache", "text/plain", `{"key":"k","values":["a"]}`)

	wantStatus(t, r, http.StatusUnsupportedMediaType)
	readProblem(t, r)
}

func TestBody_JSONContentTypesAreAccepted(t *testing.T) {
	for i, contentType := range []string{"application/json", "application/json; charset=utf-8", "text/json", "application/problem+json"} {
		c := newClient(t)
		r := c.do(http.MethodPost, "/api/v1/cache", contentType, fmt.Sprintf(`{"key":"k%d","values":["a"]}`, i))
		wantStatus(t, r, http.StatusCreated)
	}
}

func TestBody_OverTheLimitReturns413(t *testing.T) {
	c := newClient(t)
	body := `{"key":"k","values":["` + strings.Repeat("x", maxBodyBytes) + `"]}`

	r := c.do(http.MethodPost, "/api/v1/cache", "application/json", body)

	wantStatus(t, r, http.StatusRequestEntityTooLarge)
	readProblem(t, r)
}

func TestValues_RoundTripExactly(t *testing.T) {
	c := newClient(t)
	// Null, nesting, HTML characters, unicode, and a number too large for float64 must all come back untouched.
	const values = `["s",1,1.5,true,null,{"a":[1,{"b":"<&>"}]},"ąčę😀",12345678901234567890]`

	wantStatus(t, c.do(http.MethodPost, "/api/v1/cache", "application/json", `{"key":"k","values":`+values+`}`), http.StatusCreated)

	r := c.get("k", "")
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.body, `"values":`+values) {
		t.Fatalf("values changed: %s", r.body)
	}
}

func TestBody_FieldNamesAreCaseInsensitiveAndUnknownFieldsIgnored(t *testing.T) {
	c := newClient(t)

	r := c.do(http.MethodPost, "/api/v1/cache", "application/json", `{"KEY":"k","Values":["a"],"unknown":1}`)

	wantStatus(t, r, http.StatusCreated)
}

func TestGet(t *testing.T) {
	t.Run("existing key returns the entry", func(t *testing.T) {
		c := newClient(t)
		c.create("k", nil, "a", "b")

		r := c.get("k", "")

		wantStatus(t, r, http.StatusOK)
		if entry := readEntry(t, r); len(entry.Values) != 2 {
			t.Fatalf("unexpected entry: %+v", entry)
		}
	})

	t.Run("missing key returns 404", func(t *testing.T) {
		r := newClient(t).get("missing", "")

		wantStatus(t, r, http.StatusNotFound)
		if p := readProblem(t, r); p.Title != "Key not found" || !strings.Contains(p.Detail, "missing") {
			t.Fatalf("unexpected problem: %+v", p)
		}
	})

	t.Run("extendTtl pushes the expiry forward", func(t *testing.T) {
		c := newClient(t)
		created := readEntry(t, c.create("k", ttl(5), "a"))

		r := c.get("k", "?extendTtl=true&ttlSeconds=120")

		wantStatus(t, r, http.StatusOK)
		if extended := readEntry(t, r); !extended.ExpiresAtUTC.After(created.ExpiresAtUTC) {
			t.Fatalf("expiry %v is not after %v", extended.ExpiresAtUTC, created.ExpiresAtUTC)
		}
	})

	t.Run("ttlSeconds without extendTtl does not extend", func(t *testing.T) {
		c := newClient(t)
		created := readEntry(t, c.create("k", ttl(5), "a"))

		got := readEntry(t, c.get("k", "?ttlSeconds=120"))

		if !got.ExpiresAtUTC.Equal(created.ExpiresAtUTC) {
			t.Fatalf("expiry changed from %v to %v", created.ExpiresAtUTC, got.ExpiresAtUTC)
		}
	})

	t.Run("whitespace-only route key returns 400", func(t *testing.T) {
		r := newClient(t).get("   ", "")
		wantStatus(t, r, http.StatusBadRequest)
		if p := readProblem(t, r); len(p.Errors["key"]) == 0 {
			t.Fatalf("unexpected problem: %+v", p)
		}
	})

	t.Run("query parameter names are case-insensitive", func(t *testing.T) {
		c := newClient(t)
		created := readEntry(t, c.create("k", ttl(5), "a"))

		got := readEntry(t, c.get("k", "?EXTENDTTL=true&TTLSECONDS=120"))

		if !got.ExpiresAtUTC.Equal(created.ExpiresAtUTC.Add(120 * time.Second)) {
			t.Fatalf("expiry = %v, want it extended from %v", got.ExpiresAtUTC, created.ExpiresAtUTC)
		}
	})

	t.Run("empty extendTtl returns 400", func(t *testing.T) {
		c := newClient(t)
		c.create("k", nil, "a")
		wantStatus(t, c.get("k", "?extendTtl="), http.StatusBadRequest)
	})

	t.Run("empty ttlSeconds counts as absent", func(t *testing.T) {
		c := newClient(t)
		c.create("k", nil, "a")
		wantStatus(t, c.get("k", "?ttlSeconds="), http.StatusOK)
	})

	for name, query := range map[string]string{
		"negative ttl":                  "?extendTtl=true&ttlSeconds=-1",
		"negative ttl, no extend":       "?ttlSeconds=-1",
		"zero ttl":                      "?extendTtl=true&ttlSeconds=0",
		"non-numeric ttl":               "?extendTtl=true&ttlSeconds=abc",
		"ttl overflows int32":           "?extendTtl=true&ttlSeconds=2147483648",
		"non-boolean extendTtl":         "?extendTtl=maybe",
		"numeric boolean is not a bool": "?extendTtl=1",
	} {
		t.Run(name+" returns 400", func(t *testing.T) {
			c := newClient(t)
			c.create("k", nil, "a")
			r := c.get("k", query)
			wantStatus(t, r, http.StatusBadRequest)
			readProblem(t, r)
		})
	}

	t.Run("key too long returns 400", func(t *testing.T) {
		wantStatus(t, newClient(t).get(strings.Repeat("k", contract.MaxKeyLength+1), ""), http.StatusBadRequest)
	})

	t.Run("extendTtl is case-insensitive", func(t *testing.T) {
		c := newClient(t)
		c.create("k", nil, "a")
		wantStatus(t, c.get("k", "?extendTtl=TRUE"), http.StatusOK)
	})
}

func TestAppend(t *testing.T) {
	t.Run("missing key returns 404 and does not create", func(t *testing.T) {
		c := newClient(t)

		wantStatus(t, c.appendTo("k", nil, "a"), http.StatusNotFound)
		wantStatus(t, c.get("k", ""), http.StatusNotFound)
	})

	t.Run("existing key merges values and extends ttl", func(t *testing.T) {
		c := newClient(t)
		created := readEntry(t, c.create("k", ttl(5), "a"))

		r := c.appendTo("k", ttl(120), "b")

		wantStatus(t, r, http.StatusOK)
		updated := readEntry(t, r)
		if len(updated.Values) != 2 || !updated.ExpiresAtUTC.After(created.ExpiresAtUTC) {
			t.Fatalf("unexpected entry: %+v", updated)
		}
	})
}

func TestDelete(t *testing.T) {
	t.Run("existing key returns 204 and is gone", func(t *testing.T) {
		c := newClient(t)
		c.create("k", nil, "a")

		r := c.deleteKey("k")

		wantStatus(t, r, http.StatusNoContent)
		if r.body != "" {
			t.Fatalf("204 with a body: %q", r.body)
		}
		wantStatus(t, c.get("k", ""), http.StatusNotFound)
	})

	t.Run("missing key returns 404", func(t *testing.T) {
		wantStatus(t, newClient(t).deleteKey("missing"), http.StatusNotFound)
	})
}

func TestRouting_UnsupportedMethodReturns405(t *testing.T) {
	r := newClient(t).do(http.MethodPut, "/api/v1/cache", "application/json", `{}`)
	wantStatus(t, r, http.StatusMethodNotAllowed)
}

func TestConcurrentRequests_LoseNoValues(t *testing.T) {
	c := newClient(t)
	c.create("k", nil, "seed")

	const writers = 50
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			data, _ := json.Marshal(contract.AppendRequest{Key: "k", Values: raw(strconv.Itoa(i))})
			req, _ := http.NewRequest(http.MethodPatch, c.base+"/api/v1/cache", bytes.NewReader(data))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d", resp.StatusCode)
			}
		})
	}
	wg.Wait()

	if got := len(readEntry(t, c.get("k", "")).Values); got != writers+1 {
		t.Fatalf("got %d values, want %d", got, writers+1)
	}
}

func TestSwagger(t *testing.T) {
	t.Run("document is valid and lists every endpoint", func(t *testing.T) {
		r := newClient(t).do(http.MethodGet, "/swagger/v1/swagger.json", "", "")
		wantStatus(t, r, http.StatusOK)

		var doc struct {
			Paths map[string]map[string]any `json:"paths"`
		}
		if err := json.Unmarshal([]byte(r.body), &doc); err != nil {
			t.Fatal(err)
		}
		for path, methods := range map[string][]string{
			"/api/v1/cache":       {"post", "patch", "delete"},
			"/api/v1/cache/{key}": {"get"},
		} {
			for _, method := range methods {
				if _, ok := doc.Paths[path][method]; !ok {
					t.Errorf("document is missing %s %s", method, path)
				}
			}
		}
	})

	t.Run("UI is served", func(t *testing.T) {
		r := newClient(t).do(http.MethodGet, "/swagger", "", "")
		wantStatus(t, r, http.StatusOK)
		if !strings.Contains(r.body, "/swagger/v1/swagger.json") {
			t.Fatal("UI does not point at the document")
		}
	})

	t.Run("root redirects to the UI", func(t *testing.T) {
		r := newClient(t).do(http.MethodGet, "/", "", "")
		wantStatus(t, r, http.StatusFound)
		if got := r.header.Get("Location"); got != "/swagger" {
			t.Fatalf("Location = %q", got)
		}
	})

	t.Run("root is not a catch-all", func(t *testing.T) {
		wantStatus(t, newClient(t).do(http.MethodGet, "/nope", "", ""), http.StatusNotFound)
	})
}

// failingStore is a hand-written fake: it returns the given error, or panics with it when panics is true.
type failingStore struct {
	err    error
	panics bool
}

func (f failingStore) fail() (*cache.Entry, error) {
	if f.panics {
		panic(f.err)
	}
	return nil, f.err
}

func (f failingStore) Create(string, []json.RawMessage, time.Duration) (*cache.Entry, error) {
	return f.fail()
}
func (f failingStore) Get(string) (*cache.Entry, error) { return f.fail() }
func (f failingStore) Extend(string, time.Duration) (*cache.Entry, error) {
	return f.fail()
}
func (f failingStore) Append(string, []json.RawMessage, time.Duration) (*cache.Entry, error) {
	return f.fail()
}
func (f failingStore) Delete(string) error {
	_, err := f.fail()
	return err
}

// syncBuffer is a log destination that is safe to read from the test while the server goroutine writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestUnexpectedFailures_LogTheCauseAndReturnGeneric500(t *testing.T) {
	const secret = "simulated store failure"

	calls := map[string]func(c *client) result{
		"get":    func(c *client) result { return c.get("k", "") },
		"extend": func(c *client) result { return c.get("k", "?extendTtl=true") },
		"create": func(c *client) result { return c.create("k", nil, "a") },
		"append": func(c *client) result { return c.appendTo("k", nil, "a") },
		"delete": func(c *client) result { return c.deleteKey("k") },
	}

	for name, call := range calls {
		for _, panics := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s panics=%v", name, panics), func(t *testing.T) {
				var logs syncBuffer
				logger := slog.New(slog.NewTextHandler(&logs, nil))
				c := newClientFor(t, failingStore{err: errors.New(secret), panics: panics}, logger)

				r := call(c)

				wantStatus(t, r, http.StatusInternalServerError)
				if p := readProblem(t, r); p.Status != http.StatusInternalServerError {
					t.Fatalf("unexpected problem: %+v", p)
				}
				if strings.Contains(r.body, secret) {
					t.Fatalf("internal details leaked to the client: %s", r.body)
				}
				if got := logs.String(); !strings.Contains(got, secret) || !strings.Contains(got, "level=ERROR") {
					t.Fatalf("the real cause was not logged as an error: %q", got)
				}
			})
		}
	}
}

func TestRecoverPanics_LetsAbortHandlerPropagate(t *testing.T) {
	handler := recoverPanics(slog.New(slog.DiscardHandler), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", got)
		}
	}()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestRouting_TrailingSlashIsIgnored(t *testing.T) {
	c := newClient(t)

	wantStatus(t, c.do(http.MethodPost, "/api/v1/cache/", "application/json", `{"key":"k","values":["a"]}`), http.StatusCreated)
	wantStatus(t, c.do(http.MethodGet, "/api/v1/cache/k/", "", ""), http.StatusOK)
	wantStatus(t, c.do(http.MethodGet, "/api/v1/cache/", "", ""), http.StatusMethodNotAllowed)
}

func TestBody_NestingDepthIsLimited(t *testing.T) {
	// The request object and its values array use two levels, so 30 more reach the limit of 32 exactly.
	nested := func(levels int) string {
		return `{"key":"k","values":[` + strings.Repeat("[", levels) + strings.Repeat("]", levels) + `]}`
	}

	wantStatus(t, newClient(t).do(http.MethodPost, "/api/v1/cache", "application/json", nested(30)), http.StatusCreated)
	wantStatus(t, newClient(t).do(http.MethodPost, "/api/v1/cache", "application/json", nested(31)), http.StatusBadRequest)
}

func TestJSONDepth(t *testing.T) {
	tests := []struct {
		doc  string
		want int
	}{
		{`1`, 0},
		{`[]`, 1},
		{`{"a":[1,{"b":[]}]}`, 4},
		{`[[],[],[]]`, 2},
		{`"[[[["`, 0},
		{`["a]\\"]`, 1},
		{`["\"[["]`, 1},
	}
	for _, tt := range tests {
		if got := jsonDepth([]byte(tt.doc)); got != tt.want {
			t.Errorf("jsonDepth(%s) = %d, want %d", tt.doc, got, tt.want)
		}
	}
}

func TestSwagger_IndexPageIsServed(t *testing.T) {
	wantStatus(t, newClient(t).do(http.MethodGet, "/swagger/index.html", "", ""), http.StatusOK)
}

func TestRouting_UnversionedPathIsGone(t *testing.T) {
	c := newClient(t)

	wantStatus(t, c.do(http.MethodPost, "/api/cache", "application/json", `{"key":"k","values":["a"]}`), http.StatusNotFound)
	wantStatus(t, c.do(http.MethodGet, "/api/cache/k", "", ""), http.StatusNotFound)
}

func TestHealthz(t *testing.T) {
	r := newClient(t).do(http.MethodGet, "/healthz", "", "")

	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.body, `"status":"ok"`) {
		t.Fatalf("unexpected body: %s", r.body)
	}
}

// The document is generated from annotations, which the compiler can't check, and it's committed, so it can also be
// out of date. These tests fail if it stops matching the code. To fix one, update the annotations and run go generate.

type openAPI struct {
	Paths       map[string]map[string]json.RawMessage `json:"paths"`
	Definitions map[string]struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	} `json:"definitions"`
}

func readOpenAPI(t *testing.T) openAPI {
	t.Helper()
	var doc openAPI
	if err := json.Unmarshal(openAPIDocument, &doc); err != nil {
		t.Fatalf("docs/swagger.json is invalid: %v", err)
	}
	return doc
}

func TestOpenAPI_DocumentsExactlyTheRegisteredRoutes(t *testing.T) {
	want := map[string]bool{}
	for _, rt := range (&handlers{}).routes() {
		want[strings.ToLower(rt.method)+" "+rt.path] = true
	}

	got := map[string]bool{}
	for path, methods := range readOpenAPI(t).Paths {
		for method := range methods {
			got[method+" "+path] = true
		}
	}

	for route := range want {
		if !got[route] {
			t.Errorf("route %q is registered but not documented", route)
		}
	}
	for route := range got {
		if !want[route] {
			t.Errorf("route %q is documented but not registered", route)
		}
	}
}

func TestOpenAPI_SchemasMatchTheContractTypes(t *testing.T) {
	schemas := readOpenAPI(t).Definitions

	for name, value := range map[string]any{
		"contract.CreateRequest": contract.CreateRequest{},
		"contract.AppendRequest": contract.AppendRequest{},
		"contract.DeleteRequest": contract.DeleteRequest{},
		"contract.EntryResponse": contract.EntryResponse{},
	} {
		typ := reflect.TypeOf(value)
		fields := map[string]bool{}
		for i := range typ.NumField() {
			fieldName, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			fields[fieldName] = true
		}

		schema, ok := schemas[name]
		if !ok {
			t.Errorf("schema %s is missing", name)
			continue
		}
		for field := range fields {
			if _, ok := schema.Properties[field]; !ok {
				t.Errorf("%s: field %q is not documented", name, field)
			}
		}
		for property := range schema.Properties {
			if !fields[property] {
				t.Errorf("%s: property %q does not exist in the contract", name, property)
			}
		}
		for _, required := range schema.Required {
			if !fields[required] {
				t.Errorf("%s: required property %q does not exist in the contract", name, required)
			}
		}
	}
}

func TestOpenAPI_LimitsMatchTheContractConstants(t *testing.T) {
	var doc struct {
		Definitions map[string]struct {
			Properties map[string]struct {
				MinLength *int `json:"minLength"`
				MaxLength *int `json:"maxLength"`
				MinItems  *int `json:"minItems"`
				MaxItems  *int `json:"maxItems"`
			} `json:"properties"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(openAPIDocument, &doc); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"contract.CreateRequest", "contract.AppendRequest"} {
		props := doc.Definitions[name].Properties

		key, values := props["key"], props["values"]
		if key.MinLength == nil || *key.MinLength != 1 || key.MaxLength == nil || *key.MaxLength != contract.MaxKeyLength {
			t.Errorf("%s: key length limits are wrong: %+v", name, key)
		}
		if values.MinItems == nil || *values.MinItems != 1 || values.MaxItems == nil || *values.MaxItems != contract.MaxValuesPerRequest {
			t.Errorf("%s: values count limits are wrong: %+v", name, values)
		}
	}
}

# Agent instructions

How to change this repository correctly. See `README.md` first for what the project is and how it fits together.

## Working conventions

- Keep changes narrow and task-focused, and prefer the simplest fix that works.
- Ask before anything destructive or hard to undo.
- Don't invent requirements, APIs or test results. If a test wasn't run, don't say it passed.
- Flag low-confidence conclusions, and present the trade-off when several paths are reasonable.

## Go conventions

- **Errors.** Wrap with `fmt.Errorf("...: %w", err)` and match with `errors.Is` or `errors.As`. Handle each error once:
  either return it or log it, not both.
- **Panics** are for programmer bugs only. Expected failures are returned as errors.
- **Context.** Pass `ctx` as the first parameter, and don't store it in a struct.
- **Goroutines.** Every goroutine needs a clear owner and a way to stop. The owner waits for it before returning.
- **Naming and comments.** Keep initialisms in one case, such as `TTL`, `UTC` and `ID`. Exported names have doc comments.
- **Tests.** Prefer table-driven tests when cases differ only in their data.

## Project rules

- **Entries are immutable.** `cache.Entry` has unexported fields and no setters, and every change builds a new entry.
  `NewEntry` and `Values()` deep-copy the values, including each value's bytes, because `json.RawMessage` is itself a
  byte slice. Keep it that way: the store hands out `*Entry` pointers that are read after its lock is released, which
  is only safe because entries never change.
- **One mutex guards the store's map.** Hold it for the whole check-then-write sequence, and never release it
  partway through an operation, because that is how updates get lost.
- **Interfaces belong to their consumer and stay small.** `api.Store`, `background.Saver` and friends are declared
  where they are used, with only the methods needed there. Don't add a `cache.StoreInterface`. In production code,
  `cmd/objectcacheapi/main.go` is the only file that wires concrete types together.
- **`contract` is the wire format only.** It may import only the standard library.
- **Handlers stay thin.** They decode and validate the request, call the store, and write the response. TTL rules and
  expiry belong in the store.
- **REST conventions.** API endpoints live under `/api/v1`, and a breaking change gets a new version alongside the old
  one. `/healthz` and the Swagger routes describe the service rather than the API, so they aren't versioned. `key` is
  in the body for `POST`/`PATCH`/`DELETE` and in the route for `GET`.
- **Status codes.** `201` create, `200` read/append, `204` delete, `400` validation or a TTL above the maximum, `404`
  not found or expired, `405` wrong method, `409` create on an existing key, `413` body too large, `415` not JSON,
  `500` unexpected failure.
- **Error bodies are RFC 9457 problem details** for everything the `/api/v1` handlers return, and for panics. The
  exception is `http.ServeMux`'s own `404` and `405` for unmatched requests, which are plain text.
- **Unexpected failures are generic 500s.** An unrecognized store error or a panic is logged in full and returned
  as a generic 500. Never put an error's text in a response, and don't swallow an unknown error.
- **Background jobs are small structs with a `Run(ctx)` method** that blocks until the context is cancelled, then
  returns. The caller chooses the goroutine, and tests call `Run` directly inside a `testing/synctest` bubble.
- **Configuration comes from environment variables** through `internal/config`, validated at startup. Don't read
  the environment anywhere else.
- **`openapi.json` is written by hand.** Add documented endpoints to `handlers.routes()`, not directly to the mux.
  Tests compare the document with `routes()` for methods and paths, and with the `contract` types for field names.
  They don't check status codes, parameters or limits, so review those by hand.

## Testing

- Use the standard `testing` package with hand-written fakes. Don't add assertion or mocking libraries.
- Never wait on real time for simulated behavior. Use `testing/synctest`: inside a bubble, `time.Sleep` advances a
  fake clock and `synctest.Wait` waits until other goroutines are idle. Only tests that wait on real sockets may
  use real time.
- Every new behavior needs a test.
- Run `gofmt -l .`, `go vet ./...` and `go test ./...` before finishing.
- Run `go test -race ./...` for any change to concurrent code. It needs a C toolchain and a 64-bit target, so if it
  can't run in your environment, say so instead of treating the change as verified.
- Run `staticcheck ./...` or `golangci-lint run` if one is available, and say so if neither is.

## Things not to do without being asked

- Don't change the snapshot format, because existing snapshot files must keep loading.
- Don't add a dependency for something the standard library already does, such as routing, JSON or logging.

## Before finishing a change

If you changed an endpoint, update `internal/api/openapi.json` and the API table in `README.md`. If you changed a
convention, update this file in the same change.

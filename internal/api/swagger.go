package api

import (
	_ "embed"
	"net/http"
)

// @title Object Cache API
// @version v1
// @description Key-based cache store for lists of arbitrary JSON values, with configurable TTL.

//go:generate go run github.com/swaggo/swag/cmd/swag@v1.16.6 init --generalInfo swagger.go --dir .,../../contract --output docs --outputTypes json --parseInternal

//go:embed docs/swagger.json
var openAPIDocument []byte

//go:embed swagger.html
var swaggerPage []byte

func registerSwagger(mux *http.ServeMux) {
	serve := func(contentType string, body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.Write(body)
		}
	}

	// Swagger UI is the app's starting point. {$} matches "/" exactly rather than acting as a catch-all.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger", http.StatusFound)
	})
	mux.HandleFunc("GET /swagger", serve("text/html; charset=utf-8", swaggerPage))
	mux.HandleFunc("GET /swagger/index.html", serve("text/html; charset=utf-8", swaggerPage))
	mux.HandleFunc("GET /swagger/v1/swagger.json", serve("application/json; charset=utf-8", openAPIDocument))
}

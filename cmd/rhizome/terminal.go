package main

import (
	"net/http"

	"rhizome/internal/terminal"
)

// withTerminals routes /v1/terminals* to the terminal bridge (RHZ-140,
// FR-RHZ-184) and everything else to the workspace handler.
func withTerminals(next, terms http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if terminal.Routes(r.URL.Path) {
			terms.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Package httpapi implements the scaffold's operational HTTP endpoints.
package httpapi

import (
	"encoding/json"
	"net/http"
)

func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"service": "relay", "stage": "scaffold", "message": "Webhook delivery is not implemented yet.",
		})
	})
	for _, path := range []string{"GET /livez", "GET /readyz"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("ok\n"))
		})
	}
	return mux
}

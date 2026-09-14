package httpapi

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BoniLuan/relay/internal/storage"
)

func TestHistoryHTTPBoundary(t *testing.T) {
	const path = "/api/v1/events/11111111-1111-4111-8111-111111111111/attempts"
	for _, tc := range []struct {
		name, path    string
		auth          bool
		err           error
		status, calls int
	}{
		{"no token", path, false, nil, 401, 0},
		{"malformed ID", "/api/v1/events/nope/attempts", true, nil, 404, 0},
		{"missing or foreign event", path, true, storage.ErrNotFound, 404, 1},
		{"storage failure", path, true, errors.New("private_payload_secret"), 503, 1},
		{"empty history", path, true, nil, 200, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeBackend{err: tc.err}
			var logs bytes.Buffer
			handler := NewHandler(db, slog.New(slog.NewJSONHandler(&logs, nil)))
			r := httptest.NewRequest("GET", tc.path+"?private_query_secret=value", nil)
			if tc.auth {
				r.Header.Set("Authorization", "Bearer relay_"+strings.Repeat("a", 64))
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status || db.calls != tc.calls {
				t.Fatalf("status=%d calls=%d", w.Code, db.calls)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("history response can be cached")
			}
			for _, s := range []string{w.Body.String(), logs.String()} {
				if strings.Contains(s, "private_") {
					t.Fatal("private data leaked")
				}
			}
			if tc.status == 200 && !strings.Contains(w.Body.String(), `"attempts":[]`) {
				t.Fatal("empty history must be an array")
			}
		})
	}
}

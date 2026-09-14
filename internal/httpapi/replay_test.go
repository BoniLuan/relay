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

func TestReplayHTTPContract(t *testing.T) {
	const body = `{"acknowledge_duplicate_risk":true}`
	for _, tc := range []struct {
		name, body, key string
		auth, duplicate bool
		err             error
		status, calls   int
	}{
		{"unauthenticated", body, "key", false, false, nil, 401, 0},
		{"missing key", body, "", true, false, nil, 400, 0},
		{"missing acknowledgment", `{}`, "key", true, false, nil, 400, 0},
		{"null body", `null`, "key", true, false, nil, 400, 0},
		{"unknown input", `{"acknowledge_duplicate_risk":true,"url":"https://evil.example"}`, "key", true, false, nil, 400, 0},
		{"owner only", body, "key", true, false, storage.ErrNotFound, 404, 1},
		{"not eligible", body, "key", true, false, storage.ErrReplayState, 409, 1},
		{"different request", body, "key", true, false, storage.ErrReplayConflict, 409, 1},
		{"accepted", body, "key", true, false, nil, 201, 1},
		{"idempotent", body, "key", true, true, nil, 200, 1},
		{"commit failure", body, "key", true, false, errors.New("private-replay-detail"), 503, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeBackend{err: tc.err, duplicate: tc.duplicate}
			var logs bytes.Buffer
			h := NewHandler(db, slog.New(slog.NewJSONHandler(&logs, nil)))
			r := httptest.NewRequest("POST", "/api/v1/events/11111111-1111-4111-8111-111111111111/replay", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			if tc.auth {
				r.Header.Set("Authorization", "Bearer relay_"+strings.Repeat("a", 64))
			}
			if tc.key != "" {
				r.Header.Set("Idempotency-Key", tc.key)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || db.calls != tc.calls {
				t.Fatalf("status %d calls %d", w.Code, db.calls)
			}
			if tc.duplicate && w.Header().Get("Idempotency-Replayed") != "true" {
				t.Fatal("missing duplicate marker")
			}
			if strings.Contains(w.Body.String()+logs.String(), "private-replay-detail") {
				t.Fatal("leaked details")
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("cacheable mutation response")
			}
		})
	}
}

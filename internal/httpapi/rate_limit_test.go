package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/BoniLuan/relay/internal/storage"
)

func TestRequestAdmissionHTTP(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		retry                 int
		limitErr, authErr     error
		token                 bool
		path                  string
		status, limits, calls int
	}{
		{name: "allowed", token: true, path: "/api/v1/events", status: 201, limits: 1, calls: 1},
		{name: "throttled before ingestion", token: true, path: "/api/v1/events", retry: 17, status: 429, limits: 1},
		{name: "throttled before replay", token: true, path: "/api/v1/events/11111111-1111-4111-8111-111111111111/replay", retry: 17, status: 429, limits: 1},
		{name: "database fails closed", token: true, path: "/api/v1/events", limitErr: errors.New("private database detail"), status: 503, limits: 1},
		{name: "missing bearer", path: "/api/v1/events", status: 401},
		{name: "revoked bearer", token: true, path: "/api/v1/events", authErr: storage.ErrUnauthorized, status: 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeBackend{retryAfter: tc.retry, limitErr: tc.limitErr, authErr: tc.authErr}
			h := NewHandler(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
			r := httptest.NewRequest("POST", tc.path, strings.NewReader(validBody))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "same-key")
			if tc.token {
				r.Header.Set("Authorization", "Bearer relay_"+strings.Repeat("a", 64))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || db.limitCalls != tc.limits || db.calls != tc.calls {
				t.Fatalf("status=%d limits=%d operations=%d", w.Code, db.limitCalls, db.calls)
			}
			if tc.status == 429 && (w.Header().Get("Retry-After") != "17" || w.Header().Get("Cache-Control") != "no-store") {
				t.Fatal("missing throttle headers")
			}
			if tc.status != 429 && w.Header().Get("Retry-After") != "" {
				t.Fatal("unexpected retry header")
			}
			if strings.Contains(w.Body.String(), "private database detail") {
				t.Fatal("leaked storage error")
			}
		})
	}
	for _, path := range []string{"/", "/livez", "/readyz"} {
		db := &fakeBackend{retryAfter: 60}
		w := httptest.NewRecorder()
		NewHandler(db, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || db.limitCalls != 0 {
			t.Fatal("health/root consumed admission")
		}
	}
}

// Exercise real admission and idempotent ingestion through two independent API
// handlers and two credentials for the same client. No production database is used.
func testHTTPRateLimit(t *testing.T, ctx context.Context, db *storage.Store) {
	t.Helper()
	client, first, err := db.ProvisionClient(ctx, "rate-limit-http")
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := db.IssueClientToken(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := db.CreateDestination(ctx, client, "https://example.com/hook")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"destination_id":"` + destination.ID + `","payload":{"test":true}}`
	handlers := []http.Handler{NewHandler(db, slog.New(slog.NewTextHandler(io.Discard, nil))), NewHandler(db, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	tokens := []string{first, second}
	var eventID string
	denied := false
	// A minute boundary can legitimately permit a second budget during the burst.
	for i := 0; i <= 2*storage.ClientRequestsPerMinute; i++ {
		r := httptest.NewRequest("POST", "/api/v1/events", strings.NewReader(body)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+tokens[i%2])
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "rate-limit-duplicate")
		w := httptest.NewRecorder()
		handlers[i%2].ServeHTTP(w, r)
		if w.Code == 429 {
			retry, e := strconv.Atoi(w.Header().Get("Retry-After"))
			if e != nil || retry < 1 || retry > 60 {
				t.Fatal("invalid Retry-After")
			}
			if i < storage.ClientRequestsPerMinute {
				t.Fatal("premature limit")
			}
			denied = true
			break
		}
		want := 200
		if i == 0 {
			want = 201
		}
		if w.Code != want {
			t.Fatalf("request %d: status %d", i, w.Code)
		}
		var event storage.Event
		if err = json.Unmarshal(w.Body.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			eventID = event.ID
		} else if event.ID != eventID || w.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatal("duplicate created new event")
		}
	}
	if !denied {
		t.Fatal("no shared request limit")
	}
	rows, _, err := db.ListDeliveries(ctx, client, storage.DeliveryFilter{Limit: 20})
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected one persisted delivery, got %d: %v", len(rows), err)
	}
	// Another client's endpoint remains available despite this client's exhaustion.
	_, other, err := db.ProvisionClient(ctx, "rate-limit-independent")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v1/deliveries", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+other)
	w := httptest.NewRecorder()
	handlers[0].ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("foreign client throttled: %d", w.Code)
	}
}

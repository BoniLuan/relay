package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BoniLuan/relay/internal/storage"
)

func TestDeliveryCursorValidation(t *testing.T) {
	c := deliveryCursor{Version: 1, Client: "client", Status: "pending", Position: storage.DeliveryPosition{CreatedAt: time.Now().UTC(), EventID: "11111111-1111-4111-8111-111111111111"}}
	encode := func(c deliveryCursor) string {
		raw, _ := json.Marshal(c)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	valid := encode(c)
	f, err := parseDeliveryFilter("status=pending&limit=2&cursor="+valid, "client")
	if err != nil || f.Limit != 2 || f.Before == nil || !f.Before.CreatedAt.Equal(c.Position.CreatedAt) {
		t.Fatal("valid cursor rejected")
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=-1", "limit=1.5", "limit=", "limit=1&limit=2", "status=nope", "destination_id=bad", "extra=1", "limit=%zz", "cursor=***", "cursor=" + strings.Repeat("a", 1025), "cursor=" + valid, "status=failed&cursor=" + valid} {
		if _, err := parseDeliveryFilter(query, "client"); err == nil {
			t.Errorf("accepted %q", query)
		}
	}
	if _, err := parseDeliveryFilter("status=pending&cursor="+valid, "other"); err == nil {
		t.Fatal("cross-client cursor accepted")
	}
	for _, mutate := range []func(*deliveryCursor){func(c *deliveryCursor) { c.Version = 2 }, func(c *deliveryCursor) { c.Position.EventID = "bad" }, func(c *deliveryCursor) { c.Position.CreatedAt = time.Time{} }, func(c *deliveryCursor) { c.DestinationID = "11111111-1111-4111-8111-111111111111" }} {
		altered := c
		mutate(&altered)
		if _, err := parseDeliveryFilter("status=pending&cursor="+encode(altered), "client"); err == nil {
			t.Fatal("invalid cursor accepted")
		}
	}
}

func TestDeliveryListHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name, query   string
		auth          bool
		err           error
		status, calls int
	}{
		{"auth first", "?limit=bad", false, nil, 401, 0},
		{"bad query", "?status=nope", true, nil, 400, 0},
		{"missing destination", "?destination_id=11111111-1111-4111-8111-111111111111", true, storage.ErrNotFound, 404, 1},
		{"database error", "", true, errors.New("private-data"), 503, 1},
		{"empty list", "", true, nil, 200, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeBackend{err: tc.err}
			h := NewHandler(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
			r := httptest.NewRequest("GET", "/api/v1/deliveries"+tc.query, nil)
			if tc.auth {
				r.Header.Set("Authorization", "Bearer relay_"+strings.Repeat("a", 64))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || db.calls != tc.calls || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status %d calls %d", w.Code, db.calls)
			}
			if strings.Contains(w.Body.String(), "private-data") {
				t.Fatal("error leaked")
			}
			if tc.status == 200 && (!strings.Contains(w.Body.String(), `"items":[]`) || !strings.Contains(w.Body.String(), `"next_cursor":null`)) {
				t.Fatal("invalid empty list")
			}
		})
	}
}

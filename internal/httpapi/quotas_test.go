package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BoniLuan/relay/internal/storage"
)

func TestQuotaErrorHTTP(t *testing.T) {
	for _, resource := range []string{"destinations", "events", "open_deliveries"} {
		db := &fakeBackend{err: &storage.QuotaError{Resource: resource, Limit: 100}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/v1/events", strings.NewReader(validBody))
		r.Header.Set("Authorization", "Bearer relay_"+strings.Repeat("a", 64))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "key")
		NewHandler(db, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, r)
		var body struct {
			Code, Resource string
			Limit          int
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != 409 || body.Code != "quota_exceeded" || body.Resource != resource || body.Limit != 100 || w.Header().Get("Retry-After") != "" {
			t.Fatalf("quota response %d %s", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	NewHandler(&fakeBackend{}, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/usage", nil))
	if w.Code != 401 {
		t.Fatal("unauthenticated usage exposed")
	}
}

func testHTTPQuotas(t *testing.T, ctx context.Context, db *storage.Store) {
	t.Helper()
	client, token, err := db.ProvisionClient(ctx, "http-quota")
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.CreateDestination(ctx, client, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := func(method, path, key, body, credential string, want int) []byte {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	body := `{"destination_id":"` + d.ID + `","payload":{"quota":true}}`
	for i := 0; i < storage.MaxClientOpenDeliveries; i++ {
		request("POST", "/api/v1/events", fmt.Sprint(i), body, token, 201)
	}
	rejected := request("POST", "/api/v1/events", "over-capacity", body, token, 409)
	if !strings.Contains(string(rejected), `"resource":"open_deliveries"`) {
		t.Fatal("missing capacity reason")
	}
	request("POST", "/api/v1/events", "0", body, token, 200)
	var usage storage.ClientUsage
	if err = json.Unmarshal(request("GET", "/api/v1/usage", "", "", token, 200), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.OpenDeliveries.Used != storage.MaxClientOpenDeliveries || usage.Events.Used != storage.MaxClientOpenDeliveries || usage.Destinations.Used != 1 {
		t.Fatalf("usage %+v", usage)
	}
	_, other, err := db.ProvisionClient(ctx, "http-quota-other")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(request("GET", "/api/v1/usage?client_id="+client, "", "", other, 200), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.Events.Used != 0 || usage.Destinations.Used != 0 || usage.OpenDeliveries.Used != 0 {
		t.Fatal("foreign usage disclosed")
	}
}

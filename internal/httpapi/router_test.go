package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BoniLuan/relay/internal/delivery"
	"github.com/BoniLuan/relay/internal/storage"
)

type fakeBackend struct {
	err       error
	authErr   error
	duplicate bool
	calls     int
}

func (f *fakeBackend) Ready(context.Context) error { return f.err }
func (f *fakeBackend) Authenticate(context.Context, string) (string, error) {
	return "client", f.authErr
}
func (f *fakeBackend) CreateDestination(context.Context, string, string) (storage.Destination, error) {
	f.calls++
	return storage.Destination{}, f.err
}
func (f *fakeBackend) Ingest(context.Context, string, string, string, [32]byte, []byte) (storage.Event, bool, error) {
	f.calls++
	return storage.Event{}, f.duplicate, f.err
}
func (f *fakeBackend) GetEvent(context.Context, string, string) (storage.Event, error) {
	return storage.Event{}, f.err
}

const validBody = `{"destination_id":"11111111-1111-4111-8111-111111111111","payload":{"hello":"world"}}`

func TestHTTPContract(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body, token, key string
		err, authErr                         error
		duplicate                            bool
		status, calls                        int
	}{
		{name: "liveness independent of DB", method: "GET", path: "/livez", err: errors.New("down"), status: 200},
		{name: "readiness detects DB outage", method: "GET", path: "/readyz", err: errors.New("down"), status: 503},
		{name: "missing auth", method: "POST", path: "/api/v1/events", body: validBody, status: 401},
		{name: "invalid token", method: "POST", path: "/api/v1/events", body: validBody, token: "valid", authErr: storage.ErrUnauthorized, status: 401},
		{name: "auth DB outage", method: "POST", path: "/api/v1/events", token: "valid", authErr: errors.New("down"), status: 503},
		{name: "missing key", method: "POST", path: "/api/v1/events", body: validBody, token: "valid", status: 400},
		{name: "unknown field", method: "POST", path: "/api/v1/events", body: `{"extra":1}`, token: "valid", key: "k", status: 400},
		{name: "missing payload", method: "POST", path: "/api/v1/events", body: `{"destination_id":"11111111-1111-4111-8111-111111111111"}`, token: "valid", key: "k", status: 400},
		{name: "multiple documents", method: "POST", path: "/api/v1/events", body: validBody + `{}`, token: "valid", key: "k", status: 400},
		{name: "oversized body", method: "POST", path: "/api/v1/events", body: strings.Repeat(" ", maxBody+1), token: "valid", key: "k", status: 413},
		{name: "invalid UTF-8", method: "POST", path: "/api/v1/events", body: "\xff", token: "valid", key: "k", status: 400},
		{name: "excessive nesting", method: "POST", path: "/api/v1/events", body: strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65), token: "valid", key: "k", status: 400},
		{name: "JSONB validation", method: "POST", path: "/api/v1/events", body: validBody, token: "valid", key: "k", err: storage.ErrInvalidPayload, status: 400, calls: 1},
		{name: "created", method: "POST", path: "/api/v1/events", body: validBody, token: "valid", key: "k", status: 201, calls: 1},
		{name: "duplicate", method: "POST", path: "/api/v1/events", body: validBody, token: "valid", key: "k", duplicate: true, status: 200, calls: 1},
		{name: "conflict", method: "POST", path: "/api/v1/events", body: validBody, token: "valid", key: "k", err: storage.ErrConflict, status: 409, calls: 1},
		{name: "foreign destination", method: "POST", path: "/api/v1/events", body: validBody, token: "valid", key: "k", err: storage.ErrNotFound, status: 404, calls: 1},
		{name: "commit failure never acknowledged", method: "POST", path: "/api/v1/events", body: validBody, token: "valid", key: "k", err: errors.New("secret database detail"), status: 503, calls: 1},
		{name: "HTTPS destination", method: "POST", path: "/api/v1/destinations", body: `{"url":"https://example.com/hook"}`, token: "valid", status: 201, calls: 1},
		{name: "reject credentials", method: "POST", path: "/api/v1/destinations", body: `{"url":"https://user:pass@example.com"}`, token: "valid", status: 400},
		{name: "reject HTTP", method: "POST", path: "/api/v1/destinations", body: `{"url":"http://example.com"}`, token: "valid", status: 400},
		{name: "reject fragment", method: "POST", path: "/api/v1/destinations", body: `{"url":"https://example.com/#secret"}`, token: "valid", status: 400},
		{name: "malformed event ID", method: "GET", path: "/api/v1/events/nope", token: "valid", status: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeBackend{err: tc.err, authErr: tc.authErr, duplicate: tc.duplicate}
			handler := NewHandler(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer relay_"+strings.Repeat("a", 64))
			}
			if tc.key != "" {
				r.Header.Set("Idempotency-Key", tc.key)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status || db.calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s; want %d/%d", w.Code, db.calls, w.Body, tc.status, tc.calls)
			}
			if strings.Contains(w.Body.String(), "secret database detail") {
				t.Fatal("leaked database error")
			}
			if tc.duplicate && w.Header().Get("Idempotency-Replayed") != "true" {
				t.Fatal("missing replay header")
			}
		})
	}
}

func TestStorageErrorDoesNotLeakIntoLogs(t *testing.T) {
	var logs bytes.Buffer
	db := &fakeBackend{err: errors.New("password=secret payload=private")}
	handler := NewHandler(db, slog.New(slog.NewJSONHandler(&logs, nil)))
	r := httptest.NewRequest("POST", "/api/v1/events", strings.NewReader(validBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer relay_"+strings.Repeat("a", 64))
	r.Header.Set("Idempotency-Key", "test")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("status=%d", w.Code)
	}
	if logs.Len() == 0 {
		t.Fatal("missing operational error log")
	}
	for _, secret := range []string{"password=secret", "payload=private", r.Header.Get("Authorization"), validBody} {
		if strings.Contains(logs.String()+w.Body.String(), secret) {
			t.Fatal("sensitive data leaked")
		}
	}
}

func (f *fakeBackend) StageSigningSecret(context.Context, string, string) (storage.SigningSecret, delivery.Secret, error) {
	f.calls++
	return storage.SigningSecret{}, delivery.Secret{}, f.err
}
func (f *fakeBackend) ListSigningSecrets(context.Context, string, string) ([]storage.SigningSecret, error) {
	return nil, f.err
}
func (f *fakeBackend) ActivateSigningSecret(context.Context, string, string, int) error { return f.err }
func (f *fakeBackend) RevokeSigningSecret(context.Context, string, string, int) error   { return f.err }

func (f *fakeBackend) GetDeliveryHistory(context.Context, string, string) (storage.DeliveryHistory, error) {
	f.calls++
	return storage.DeliveryHistory{Attempts: []storage.AttemptMetadata{}}, f.err
}

func (f *fakeBackend) ListDeliveries(context.Context, string, storage.DeliveryFilter) ([]storage.DeliverySummary, bool, error) {
	f.calls++
	return []storage.DeliverySummary{}, false, f.err
}

func (f *fakeBackend) ReplayDelivery(context.Context, string, string, string, [32]byte) (storage.ReplayReceipt, bool, error) {
	f.calls++
	return storage.ReplayReceipt{}, f.duplicate, f.err
}

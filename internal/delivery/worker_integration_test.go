package delivery_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BoniLuan/relay/internal/delivery"
	"github.com/BoniLuan/relay/internal/httpapi"
	"github.com/BoniLuan/relay/internal/secrets"
	"github.com/BoniLuan/relay/internal/storage"
	"github.com/BoniLuan/relay/internal/worker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationStore(t *testing.T) (*storage.Store, *pgxpool.Pool) {
	t.Helper()
	raw := os.Getenv("RELAY_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("run make test-integration")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "relay-test-db" || u.Path != "/relay_test" || u.User == nil || u.User.Username() != "relay_test" {
		t.Fatal("requires isolated Relay test database")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "relay_test_" + strings.ReplaceAll(storage.NewID(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := storage.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "keyring.json")
	if err = secrets.InitFile(file); err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.LoadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.WithKeyring(keys).RegisterKeyring(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return db, pool
}

func TestIngestionToSignedAttempt(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          int
		body, url, code string
	}{
		{"success", 204, "", "https://example.com/hook?private=marker", ""},
		{"receiver failure", 503, "receiver-sensitive-marker", "https://example.com/hook", "http_status"},
		{"non-standard status", 700, "", "https://example.com/hook", "response"},
		{"redirect", 302, "", "https://example.com/hook", "http_status"},
		{"oversized response", 200, strings.Repeat("x", (16<<10)+1), "https://example.com/hook", "response"},
		{"timeout", 0, "", "https://example.com/hook", "network"},
		{"SSRF rejected", 0, "", "https://127.0.0.1/hook", "destination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, pool := integrationStore(t)
			ctx := context.Background()
			client, token, err := db.ProvisionClient(ctx, "integration")
			if err != nil {
				t.Fatal(err)
			}
			d, err := db.CreateDestination(ctx, client, tc.url)
			if err != nil {
				t.Fatal(err)
			}
			meta, key, err := db.StageSigningSecret(ctx, client, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = db.ActivateSigningSecret(ctx, client, d.ID, meta.Version); err != nil {
				t.Fatal(err)
			}
			const payload = "{ \"z\": 9007199254740993, \"private\":\"payload-sensitive-marker\" }"
			requestBody := `{"destination_id":"` + d.ID + `","payload":` + payload + `}`
			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
			api := httpapi.NewHandler(db, logger)
			ingest := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/api/v1/events", strings.NewReader(requestBody))
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Idempotency-Key", "integration")
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				api.ServeHTTP(w, r)
				return w
			}
			response := ingest()
			if response.Code != 201 {
				t.Fatalf("ingest status=%d", response.Code)
			}
			var event storage.Event
			if err = json.Unmarshal(response.Body.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			var received atomic.Int32
			sender, dials := delivery.NewFixtureSender(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				body, _ := io.ReadAll(r.Body)
				if string(body) != payload || !delivery.Verify(key, r.Header.Get("Relay-Signature"), event.ID, body, time.Now()) || r.Header.Get("Relay-Event-ID") != event.ID {
					t.Error("signed payload mismatch")
				}
				var started bool
				err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM delivery_attempts WHERE event_id=$1 AND state='started' AND signing_version=1)`, event.ID).Scan(&started)
				if err != nil || !started {
					t.Error("HTTP preceded durable attempt start")
				}
				if tc.name == "timeout" {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Location", "https://127.0.0.1/never-follow")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			// Independent workers race for the same event; no second send may occur.
			var wg sync.WaitGroup
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := worker.RunOnce(ctx, db, sender, logger, 30*time.Second); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			if err = worker.RunOnce(ctx, db, sender, logger, 30*time.Second); err != nil {
				t.Fatal(err)
			}
			wantCalls := int32(1)
			if tc.name == "SSRF rejected" {
				wantCalls = 0
			}
			if received.Load() != wantCalls || dials.Load() != wantCalls {
				t.Fatalf("received=%d dials=%d", received.Load(), dials.Load())
			}
			var state, code string
			var status, count int
			err = pool.QueryRow(ctx, `SELECT state,COALESCE(error_code,''),COALESCE(http_status,0),(SELECT count(*) FROM delivery_attempts) FROM delivery_attempts WHERE event_id=$1`, event.ID).Scan(&state, &code, &status, &count)
			wantState := "failed"
			if tc.code == "" {
				wantState = "succeeded"
			}
			wantStatus := tc.status
			if wantStatus > 599 {
				wantStatus = 0
			}
			if err != nil || state != wantState || code != tc.code || status != wantStatus || count != 1 {
				t.Fatalf("state=%s code=%s status=%d count=%d err=%v", state, code, status, count, err)
			}
			// Duplicate ingestion returns the terminal event; it never requeues it.
			duplicate := ingest()
			if duplicate.Code != 200 {
				t.Fatal("duplicate failed")
			}
			var again storage.Event
			json.Unmarshal(duplicate.Body.Bytes(), &again)
			if again.ID != event.ID || again.Status != wantState {
				t.Fatal("duplicate changed identity/status")
			}
		})
	}
}

type unconfirmedQueue struct{ *storage.Store }

func (unconfirmedQueue) FinishAttempt(context.Context, storage.Lease, string, storage.AttemptResult) error {
	return errors.New("synthetic private database detail")
}
func TestHTTPAcceptedButResultLostBecomesUnknown(t *testing.T) {
	db, pool := integrationStore(t)
	ctx := context.Background()
	client, _, err := db.ProvisionClient(ctx, "uncertain")
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.CreateDestination(ctx, client, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	meta, _, err := db.StageSigningSecret(ctx, client, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ActivateSigningSecret(ctx, client, d.ID, meta.Version); err != nil {
		t.Fatal(err)
	}
	event, _, err := db.Ingest(ctx, client, d.ID, "uncertain", [32]byte{}, []byte(`null`))
	if err != nil {
		t.Fatal(err)
	}
	var received atomic.Int32
	sender, _ := delivery.NewFixtureSender(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1); w.WriteHeader(204) }))
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	err = worker.RunOnce(ctx, unconfirmedQueue{db}, sender, logger, 30*time.Second)
	if err == nil || strings.Contains(err.Error(), "synthetic") {
		t.Fatal("unconfirmed result reported success or leaked error")
	}
	if _, err = pool.Exec(ctx, "UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE event_id=$1", event.ID); err != nil {
		t.Fatal(err)
	}
	if err = worker.RunOnce(ctx, db, sender, logger, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err = worker.RunOnce(ctx, db, sender, logger, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	found, err := db.GetEvent(ctx, client, event.ID)
	if err != nil || found.Status != "unknown" || received.Load() != 1 {
		t.Fatal("uncertain delivery was lost or resent")
	}
	if strings.Contains(logs.String(), "synthetic") {
		t.Fatal("raw error leaked")
	}
}

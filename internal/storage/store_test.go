package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

// This suite only accepts the dedicated disposable Compose database. It never
// reads RELAY_DATABASE_URL, truncates tables or connects to development data.
func TestPostgres(t *testing.T) {
	raw := os.Getenv("RELAY_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("run make test-integration for isolated PostgreSQL tests")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "relay-test-db" || u.Path != "/relay_test" || u.User == nil || u.User.Username() != "relay_test" {
		t.Fatal("integration tests require the dedicated relay-test-db/relay_test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	s, err := Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	if err = s.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	// Even errors bypassing API validation must not log JSON context.
	if _, err = s.pool.Exec(ctx, "SELECT $1::jsonb", `{"value":"RELAY_LOG_SENTINEL\u0000"}`); err == nil {
		t.Fatal("expected direct JSONB conversion error")
	}
	client, token, err := s.ProvisionClient(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	authenticated, err := s.Authenticate(ctx, token)
	if err != nil || authenticated != client {
		t.Fatalf("authenticate: %s %v", authenticated, err)
	}
	if _, err = s.Authenticate(ctx, "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong token: %v", err)
	}
	other, _, err := s.ProvisionClient(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDestination(ctx, client, "https://example.com/hook")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"hello":"world"}`)
	hash := sha256.Sum256(payload)
	key := NewID()
	const racers = 12
	type result struct {
		event     Event
		duplicate bool
		err       error
	}
	results := make(chan result, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, dup, err := s.Ingest(ctx, client, d.ID, key, hash, payload)
			results <- result{e, dup, err}
		}()
	}
	wg.Wait()
	close(results)
	var id string
	created := 0
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if id == "" {
			id = r.event.ID
		}
		if id != r.event.ID {
			t.Fatal("duplicates produced different events")
		}
		if !r.duplicate {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created %d events", created)
	}
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM deliveries WHERE event_id=$1", id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("delivery count=%d err=%v", count, err)
	}
	if _, _, err = s.Ingest(ctx, client, d.ID, key, sha256.Sum256([]byte("changed")), payload); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if _, _, err = s.Ingest(ctx, other, d.ID, NewID(), hash, payload); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ownership: %v", err)
	}
	if _, err = s.GetEvent(ctx, other, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("event ownership: %v", err)
	}
	if e, err := s.GetEvent(ctx, client, id); err != nil || e.Status != "pending" {
		t.Fatalf("read event: %+v %v", e, err)
	}
	otherDestination, err := s.CreateDestination(ctx, other, "https://example.org")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Ingest(ctx, other, otherDestination.ID, key, hash, payload); err != nil {
		t.Fatalf("keys should be client scoped: %v", err)
	}

	// A deferred constraint trigger fails at COMMIT, after both INSERTs succeed.
	// The fixture is installed only in the explicitly isolated ephemeral database.
	_, err = s.pool.Exec(ctx, `CREATE FUNCTION fail_test_delivery() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected commit failure' USING DETAIL = 'RELAY_LOG_SENTINEL_private_payload'; END $$;
 CREATE CONSTRAINT TRIGGER fail_test_delivery AFTER INSERT ON deliveries DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_test_delivery()`)
	if err != nil {
		t.Fatal(err)
	}
	failedKey := NewID()
	_, _, ingestErr := s.Ingest(ctx, client, d.ID, failedKey, hash, payload)
	_, cleanupErr := s.pool.Exec(ctx, `DROP TRIGGER fail_test_delivery ON deliveries; DROP FUNCTION fail_test_delivery()`)
	if cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if ingestErr == nil {
		t.Fatal("commit failure acknowledged")
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE client_id=$1 AND idempotency_key=$2", client, failedKey).Scan(&count); err != nil || count != 0 {
		t.Fatalf("event survived rollback: %d %v", count, err)
	}
	if _, dup, err := s.Ingest(ctx, client, d.ID, failedKey, hash, payload); err != nil || dup {
		t.Fatalf("retry after rollback: duplicate=%v err=%v", dup, err)
	}
}

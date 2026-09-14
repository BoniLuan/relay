package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestDeliveryListingKeysetIsolationAndFilters(t *testing.T) {
	s, owner, destination, event, _ := attemptFixture(t)
	ctx := context.Background()
	ids := []string{event.ID}
	for i := 0; i < 5; i++ {
		e, _, err := s.Ingest(ctx, owner, destination, NewID(), sha256.Sum256([]byte("null")), []byte("null"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	// Equal timestamps exercise the UUID tie-breaker, not incidental clock order.
	stamp := time.Date(2000, 1, 1, 12, 0, 0, 123456000, time.UTC)
	if _, err := s.pool.Exec(ctx, "UPDATE events SET created_at=$1 WHERE client_id=$2", stamp, owner); err != nil {
		t.Fatal(err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	foreign, _, err := s.ProvisionClient(ctx, "foreign list owner")
	if err != nil {
		t.Fatal(err)
	}
	fd, err := s.CreateDestination(ctx, foreign, "https://example.com/foreign")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Ingest(ctx, foreign, fd.ID, "foreign", sha256.Sum256([]byte("null")), []byte("null")); err != nil {
		t.Fatal(err)
	}
	filter := DeliveryFilter{Limit: 2}
	seen := []string{}
	for page := 0; page < 3; page++ {
		items, more, err := s.ListDeliveries(ctx, owner, filter)
		if err != nil || len(items) != 2 || more != (page < 2) {
			t.Fatalf("page %d: %v more=%v err=%v", page, items, more, err)
		}
		for _, item := range items {
			if item.DestinationID != destination || item.Status != "pending" || item.AttemptCount != 0 || item.NextAttemptAt != nil {
				t.Fatal("unexpected metadata")
			}
			seen = append(seen, item.EventID)
		}
		raw, _ := json.Marshal(items)
		for _, private := range []string{"payload", "private=value", "lease_token", "secret"} {
			if strings.Contains(string(raw), private) {
				t.Fatal("private field exposed")
			}
		}
		last := items[len(items)-1]
		filter.Before = &DeliveryPosition{CreatedAt: last.CreatedAt, EventID: last.EventID}
		if page == 0 {
			// Newer arrivals do not shift the remainder of this traversal.
			if _, _, err = s.Ingest(ctx, owner, destination, NewID(), sha256.Sum256([]byte("null")), []byte("null")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if strings.Join(seen, ",") != strings.Join(ids, ",") {
		t.Fatalf("gap/duplicate: %v vs %v", seen, ids)
	}
	items, more, err := s.ListDeliveries(ctx, owner, filter)
	if err != nil || items == nil || len(items) != 0 || more {
		t.Fatal("end must be an empty page")
	}
	for _, dest := range []string{fd.ID, NewID()} {
		if _, _, err = s.ListDeliveries(ctx, owner, DeliveryFilter{Limit: 20, DestinationID: dest}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign/missing destination: %v", err)
		}
	}
	// Owner + destination + status compose without bypassing ownership.
	items, more, err = s.ListDeliveries(ctx, owner, DeliveryFilter{Limit: 100, DestinationID: destination, Status: "pending"})
	if err != nil || more || len(items) != 7 {
		t.Fatal("combined filters failed")
	}
	if _, err = s.pool.Exec(ctx, "UPDATE deliveries SET status='failed' WHERE event_id=$1", event.ID); err != nil {
		t.Fatal(err)
	}
	items, _, err = s.ListDeliveries(ctx, owner, DeliveryFilter{Limit: 20, Status: "failed"})
	if err != nil || len(items) != 1 || items[0].EventID != event.ID {
		t.Fatal("status filter failed")
	}
	items, _, err = s.ListDeliveries(ctx, foreign, DeliveryFilter{Limit: 20})
	if err != nil || len(items) != 1 || items[0].DestinationID != fd.ID {
		t.Fatal("foreign client saw another owner's data")
	}
	for _, limit := range []int{0, 101} {
		if _, _, err = s.ListDeliveries(ctx, owner, DeliveryFilter{Limit: limit}); err == nil {
			t.Fatal("unbounded limit accepted")
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err = s.ListDeliveries(canceled, owner, DeliveryFilter{Limit: 20}); err == nil {
		t.Fatal("canceled query succeeded")
	}
}

func TestListingMigrationPreservesVersionSixData(t *testing.T) {
	s, _ := isolatedTestStore(t)
	ctx := context.Background()
	for _, sql := range []string{initialSchema, signingSchema, leaseSchema, attemptSchema, retrySchema, cooldownSchema} {
		if _, err := s.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, "CREATE TABLE schema_migrations(version integer PRIMARY KEY); INSERT INTO schema_migrations VALUES(1),(2),(3),(4),(5),(6)"); err != nil {
		t.Fatal(err)
	}
	owner, _, err := s.ProvisionClient(ctx, "migration list owner")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDestination(ctx, owner, "https://example.com/hook")
	if err != nil {
		t.Fatal(err)
	}
	e, _, err := s.Ingest(ctx, owner, d.ID, "retained", sha256.Sum256([]byte("null")), []byte("null"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ready(ctx); err == nil {
		t.Fatal("old schema ready")
	}
	for i := 0; i < 2; i++ {
		if err = s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.ListDeliveries(ctx, owner, DeliveryFilter{Limit: 20})
	if err != nil || len(items) != 1 || items[0].EventID != e.ID {
		t.Fatal("migration lost existing delivery")
	}
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname IN ('events_client_page','events_client_destination_page')").Scan(&count); err != nil || count != 2 {
		t.Fatal("missing page indexes")
	}
}

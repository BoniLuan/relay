package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Bulk fixtures model retained work without exercising HTTP or contacting receivers.
func seedQuotaEvents(t *testing.T, s *Store, client, destination string, n int, state string) {
	t.Helper()
	hash := sha256.Sum256([]byte(`{}`))
	_, err := s.pool.Exec(context.Background(), `WITH inserted AS (
 INSERT INTO events(id,client_id,destination_id,idempotency_key,request_hash,payload,payload_bytes)
 SELECT md5($1::text || '-quota-' || i)::uuid,$1::uuid,$2,'quota-'||i,$3,'{}',convert_to('{}','UTF8')
 FROM generate_series(1,$4) i RETURNING id)
 INSERT INTO deliveries(event_id,status) SELECT id,$5 FROM inserted`, client, destination, hash[:], n, state)
	if err != nil {
		t.Fatal(err)
	}
}
func wantQuota(t *testing.T, err error, resource string) {
	t.Helper()
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Resource != resource {
		t.Fatalf("expected %s quota, got %v", resource, err)
	}
}
func quotaFixture(t *testing.T) (*Store, string, Destination) {
	t.Helper()
	s := tokenTestStore(t)
	ctx := context.Background()
	client, _, err := s.ProvisionClient(ctx, "quota owner")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDestination(ctx, client, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	return s, client, d
}

func TestDestinationQuotaConcurrentPools(t *testing.T) {
	s, client, _ := quotaFixture(t)
	ctx := context.Background()
	other, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	results := make(chan error, 24)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := s
			if i%2 == 0 {
				db = other
			}
			d, e := db.CreateDestination(ctx, client, "https://example.com")
			if e != nil && d.ID != "" {
				results <- errors.New("failed destination disclosed ID")
				return
			}
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	created := 1
	for e := range results {
		if e == nil {
			created++
		} else {
			wantQuota(t, e, "destinations")
		}
	}
	if created != MaxClientDestinations {
		t.Fatalf("created %d", created)
	}
	usage, err := other.ClientUsage(ctx, client)
	if err != nil || usage.Destinations.Used != created {
		t.Fatalf("usage %+v %v", usage, err)
	}
	foreign, _, err := s.ProvisionClient(ctx, "independent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateDestination(ctx, foreign, "https://example.org"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClientUsage(ctx, NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown client usage accepted")
	}
}

func TestRetainedQuotaDuplicateAndNoPartialWrite(t *testing.T) {
	s, client, d := quotaFixture(t)
	ctx := context.Background()
	hash := sha256.Sum256([]byte(`{}`))
	seedQuotaEvents(t, s, client, d.ID, MaxClientEvents, "succeeded")
	e, _, err := s.Ingest(ctx, client, d.ID, "rejected", hash, []byte(`{}`))
	wantQuota(t, err, "events")
	if e.ID != "" {
		t.Fatal("uncommitted event disclosed")
	}
	e, dup, err := s.Ingest(ctx, client, d.ID, "quota-1", hash, []byte(`{}`))
	if err != nil || !dup || e.Status != "succeeded" {
		t.Fatal("quota hid duplicate")
	}
	if _, _, err = s.Ingest(ctx, client, d.ID, "quota-1", sha256.Sum256([]byte("changed")), []byte(`{}`)); !errors.Is(err, ErrConflict) {
		t.Fatal("quota hid idempotency conflict")
	}
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE client_id=$1 AND idempotency_key='rejected'", client).Scan(&count); err != nil || count != 0 {
		t.Fatal("quota rejection retained event")
	}
	usage, err := s.ClientUsage(ctx, client)
	if err != nil || usage.Events.Used != MaxClientEvents || usage.OpenDeliveries.Used != 0 {
		t.Fatalf("usage %+v %v", usage, err)
	}
}

func TestOpenQuotaCountsEveryLiveStateAndRecovers(t *testing.T) {
	s, client, d := quotaFixture(t)
	ctx := context.Background()
	hash := sha256.Sum256([]byte(`{}`))
	seedQuotaEvents(t, s, client, d.ID, MaxClientOpenDeliveries, "pending")
	for i, state := range []string{"leased", "attempting", "retry_wait"} {
		id := fmt.Sprintf("quota-%d", i+1)
		var err error
		if state == "retry_wait" {
			_, err = s.pool.Exec(ctx, `UPDATE deliveries SET status='retry_wait',attempt_count=1,next_attempt_at=clock_timestamp()+interval '1 hour' WHERE event_id=(SELECT id FROM events WHERE client_id=$1 AND idempotency_key=$2)`, client, id)
		} else {
			_, err = s.pool.Exec(ctx, `UPDATE deliveries SET status=$3,lease_token=$4,lease_owner=$5,lease_expires_at=clock_timestamp()-interval '1 second' WHERE event_id=(SELECT id FROM events WHERE client_id=$1 AND idempotency_key=$2)`, client, id, state, NewID(), NewID())
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Ingest(ctx, client, d.ID, "next", hash, []byte(`{}`)); err != nil {
		wantQuota(t, err, "open_deliveries")
	} else {
		t.Fatal("open work omitted")
	}
	usage, err := s.ClientUsage(ctx, client)
	if err != nil || usage.OpenDeliveries.Used != MaxClientOpenDeliveries {
		t.Fatalf("usage %+v %v", usage, err)
	}
	// A committed terminal transition releases capacity; merely expired leases do not.
	if _, err = s.pool.Exec(ctx, `UPDATE deliveries SET status='succeeded' WHERE event_id=(SELECT id FROM events WHERE client_id=$1 AND idempotency_key='quota-4')`, client); err != nil {
		t.Fatal(err)
	}
	if _, dup, err := s.Ingest(ctx, client, d.ID, "next", hash, []byte(`{}`)); err != nil || dup {
		t.Fatalf("rejected key was reserved: %v", err)
	}
}

func TestIngestionAndReplayCompeteForLastSlot(t *testing.T) {
	s, client, event, _, _ := failedReplayFixture(t)
	ctx := context.Background()
	seedQuotaEvents(t, s, client, event.DestinationID, MaxClientOpenDeliveries-1, "pending")
	other, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	results := make(chan error, 8)
	var wg sync.WaitGroup
	hash := sha256.Sum256([]byte(`{}`))
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var e error
			if i == 0 {
				_, _, e = other.ReplayDelivery(ctx, client, event.ID, "replay", hash)
			} else {
				_, _, e = s.Ingest(ctx, client, event.DestinationID, fmt.Sprint("race-", i), hash, []byte(`{}`))
			}
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for e := range results {
		if e == nil {
			accepted++
		} else {
			wantQuota(t, e, "open_deliveries")
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d into one slot", accepted)
	}
	usage, err := s.ClientUsage(ctx, client)
	if err != nil || usage.OpenDeliveries.Used != MaxClientOpenDeliveries {
		t.Fatalf("usage %+v %v", usage, err)
	}
}

func TestReplayQuotaReceiptAndRollback(t *testing.T) {
	s, client, event, _, _ := failedReplayFixture(t)
	ctx := context.Background()
	hash := sha256.Sum256([]byte(`{}`))
	seedQuotaEvents(t, s, client, event.DestinationID, MaxClientOpenDeliveries, "pending")
	_, _, err := s.ReplayDelivery(ctx, client, event.ID, "replay", hash)
	wantQuota(t, err, "open_deliveries")
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM delivery_replays WHERE event_id=$1", event.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("denied replay retained receipt")
	}
	// Exercise the actual worker transaction: finishing an existing reservation
	// frees capacity without a separate decrement hook or outbound network call.
	lease := claimAttempt(t, s)
	work, err := s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 204}); err != nil {
		t.Fatal(err)
	}
	receipt, dup, err := s.ReplayDelivery(ctx, client, event.ID, "replay", hash)
	if err != nil || dup {
		t.Fatalf("replay after release: %v", err)
	}
	again, dup, err := s.ReplayDelivery(ctx, client, event.ID, "replay", hash)
	if err != nil || !dup || again.ID != receipt.ID {
		t.Fatal("full queue hid replay receipt")
	}
}

func TestQuotaCommitFailureDoesNotConsumeCapacity(t *testing.T) {
	s, client, d := quotaFixture(t)
	ctx := context.Background()
	hash := sha256.Sum256([]byte(`{}`))
	seedQuotaEvents(t, s, client, d.ID, MaxClientOpenDeliveries-1, "pending")
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_quota() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected quota commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER reject_quota AFTER INSERT ON deliveries DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_quota()`); err != nil {
		t.Fatal(err)
	}
	e, _, err := s.Ingest(ctx, client, d.ID, "rollback", hash, []byte(`{}`))
	if err == nil || e.ID != "" {
		t.Fatal("failed commit acknowledged")
	}
	usage, err := s.ClientUsage(ctx, client)
	if err != nil || usage.Events.Used != MaxClientOpenDeliveries-1 || usage.OpenDeliveries.Used != MaxClientOpenDeliveries-1 {
		t.Fatalf("rollback usage %+v %v", usage, err)
	}
	if _, err = s.pool.Exec(ctx, "DROP TRIGGER reject_quota ON deliveries"); err != nil {
		t.Fatal(err)
	}
	if _, dup, err := s.Ingest(ctx, client, d.ID, "rollback", hash, []byte(`{}`)); err != nil || dup {
		t.Fatalf("retry after failed commit: %v", err)
	}
}

func TestRetainedQuotaConcurrentLastSlot(t *testing.T) {
	s, client, d := quotaFixture(t)
	ctx := context.Background()
	hash := sha256.Sum256([]byte(`{}`))
	seedQuotaEvents(t, s, client, d.ID, MaxClientEvents-1, "succeeded")
	other, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := s
			if i%2 == 0 {
				db = other
			}
			_, _, e := db.Ingest(ctx, client, d.ID, fmt.Sprint("last-", i), hash, []byte(`{}`))
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for e := range results {
		if e == nil {
			accepted++
		} else {
			wantQuota(t, e, "events")
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d into last retained slot", accepted)
	}
	usage, err := other.ClientUsage(ctx, client)
	if err != nil || usage.Events.Used != MaxClientEvents || usage.OpenDeliveries.Used != 1 {
		t.Fatalf("usage %+v %v", usage, err)
	}
}

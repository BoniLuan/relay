package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func leaseTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	db, raw := isolatedTestStore(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db, raw
}
func queueEvents(t *testing.T, s *Store, n int) []string {
	t.Helper()
	ctx := context.Background()
	client, _, err := s.ProvisionClient(ctx, "queue test")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDestination(ctx, client, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < n; i++ {
		payload := []byte(`{"synthetic":"queue-test"}`)
		e, _, err := s.Ingest(ctx, client, d.ID, NewID(), sha256.Sum256(payload), payload)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	return ids
}
func TestConcurrentDeliveryClaims(t *testing.T) {
	for _, jobs := range []int{1, 12} {
		t.Run(fmt.Sprintf("%d_jobs", jobs), func(t *testing.T) {
			s, raw := leaseTestStore(t)
			ids := queueEvents(t, s, jobs)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stores := []*Store{s}
			for i := 0; i < 3; i++ {
				db, err := Open(ctx, raw)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				stores = append(stores, db)
			}
			type result struct {
				lease Lease
				err   error
			}
			results := make(chan result, 16)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					l, err := stores[i%len(stores)].ClaimDelivery(ctx, NewID(), time.Minute)
					results <- result{l, err}
				}(i)
			}
			close(start)
			wg.Wait()
			close(results)
			seen := map[string]bool{}
			for r := range results {
				if errors.Is(r.err, ErrNoDelivery) {
					continue
				}
				if r.err != nil {
					t.Fatal(r.err)
				}
				if seen[r.lease.EventID] {
					t.Fatal("same delivery leased concurrently")
				}
				seen[r.lease.EventID] = true
				if r.lease.token == "" || r.lease.Remaining() <= 0 {
					t.Fatal("invalid committed lease")
				}
			}
			if len(seen) != jobs {
				t.Fatalf("claimed %d; want %d", len(seen), jobs)
			}
			for _, id := range ids {
				if !seen[id] {
					t.Fatal("delivery missed")
				}
			}
		})
	}
}
func TestClaimsSkipLockedRows(t *testing.T) {
	s, _ := leaseTestStore(t)
	ids := queueEvents(t, s, 2)
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, "SELECT event_id FROM deliveries WHERE event_id=$1 FOR UPDATE", ids[0]); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	l, err := s.ClaimDelivery(bounded, NewID(), time.Minute)
	if err != nil || l.EventID != ids[1] {
		t.Fatalf("locked row prevented other work: %v", err)
	}
}
func TestLeaseExpiryAndStaleRelease(t *testing.T) {
	s, raw := leaseTestStore(t)
	queueEvents(t, s, 1)
	ctx := context.Background()
	owner := NewID()
	crashed, err := Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	old, err := crashed.ClaimDelivery(ctx, owner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	crashed.Close() // Dropping the process pool must not implicitly release its claim.
	if _, err = s.ClaimDelivery(ctx, NewID(), time.Minute); !errors.Is(err, ErrNoDelivery) {
		t.Fatalf("live lease reclaimed: %v", err)
	}
	// Deterministic database-clock expiration; process-clock changes are irrelevant.
	if _, err = s.pool.Exec(ctx, "UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE event_id=$1", old.EventID); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseDelivery(ctx, old); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("expired owner released reservation")
	}
	fresh, err := s.ClaimDelivery(ctx, owner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.EventID != old.EventID || fresh.token == old.token {
		t.Fatal("reclaim did not replace ownership token")
	}
	if err = s.ReleaseDelivery(ctx, old); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("stale owner released new reservation")
	}
	if _, err = s.ClaimDelivery(ctx, NewID(), time.Minute); !errors.Is(err, ErrNoDelivery) {
		t.Fatal("stale release changed new claim")
	}
	if err = s.ReleaseDelivery(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseDelivery(ctx, fresh); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("duplicate release accepted")
	}
	next, err := s.ClaimDelivery(ctx, NewID(), time.Minute)
	if err != nil || next.EventID != old.EventID {
		t.Fatal("released delivery not available")
	}
}
func TestLeaseExpiresUsingDatabaseClock(t *testing.T) {
	s, _ := leaseTestStore(t)
	queueEvents(t, s, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	old, err := s.ClaimDelivery(ctx, NewID(), 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// pg_sleep advances real DB time; no administrative expiry update is involved.
	if _, err = s.pool.Exec(ctx, "SELECT pg_sleep(0.06)"); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.ClaimDelivery(ctx, NewID(), time.Minute)
	if err != nil || fresh.EventID != old.EventID || fresh.token == old.token {
		t.Fatalf("expired delivery not reclaimed: %v", err)
	}
}
func TestFailedClaimCommitRollsBack(t *testing.T) {
	s, _ := leaseTestStore(t)
	ids := queueEvents(t, s, 1)
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `CREATE FUNCTION fail_claim() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected claim commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER fail_claim AFTER UPDATE ON deliveries DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_claim()`)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.ClaimDelivery(ctx, NewID(), time.Minute)
	if err == nil || lease.EventID != "" || lease.token != "" {
		t.Fatal("failed commit returned an owned lease")
	}
	var clean bool
	if err = s.pool.QueryRow(ctx, "SELECT status='pending' AND lease_token IS NULL AND lease_owner IS NULL AND lease_expires_at IS NULL FROM deliveries WHERE event_id=$1", ids[0]).Scan(&clean); err != nil || !clean {
		t.Fatal("failed claim left a reservation")
	}
	if _, err = s.pool.Exec(ctx, "DROP TRIGGER fail_claim ON deliveries; DROP FUNCTION fail_claim()"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimDelivery(ctx, NewID(), time.Minute); err != nil {
		t.Fatal("retry after rollback failed")
	}
}
func TestLeaseInvariantsAndCancellation(t *testing.T) {
	s, _ := leaseTestStore(t)
	ids := queueEvents(t, s, 1)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, "UPDATE deliveries SET status='leased' WHERE event_id=$1", ids[0]); err == nil {
		t.Fatal("partial lease accepted")
	}
	for _, d := range []time.Duration{0, -time.Second, time.Microsecond, MaxLeaseDuration + time.Second} {
		if _, err := s.ClaimDelivery(ctx, NewID(), d); !errors.Is(err, ErrInvalidLease) {
			t.Fatalf("duration %v accepted", d)
		}
	}
	if _, err := s.ClaimDelivery(ctx, "not-a-uuid", time.Minute); !errors.Is(err, ErrInvalidLease) {
		t.Fatal("invalid owner accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ClaimDelivery(canceled, NewID(), time.Minute); err == nil {
		t.Fatal("canceled claim succeeded")
	}
	l, err := s.ClaimDelivery(ctx, NewID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, l), l.token) {
			t.Fatal("lease token exposed in formatting")
		}
	}
	encoded, err := json.Marshal(l)
	if err != nil || strings.Contains(string(encoded), l.token) {
		t.Fatal("lease token exposed in JSON")
	}
	if err = s.ReleaseDelivery(ctx, Lease{}); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("zero lease accepted")
	}
}

func TestReleaseChecksExpiryAfterLockWait(t *testing.T) {
	s, raw := leaseTestStore(t)
	queueEvents(t, s, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := s.ClaimDelivery(ctx, NewID(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, "SELECT event_id FROM deliveries WHERE event_id=$1 FOR UPDATE", lease.EventID); err != nil {
		t.Fatal(err)
	}
	// Give the releasing connection a unique label so the test can prove it is
	// actually waiting for the lock, rather than depending on goroutine scheduling.
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	label := "lease-wait-" + NewID()
	q := u.Query()
	q.Set("application_name", label)
	u.RawQuery = q.Encode()
	releaser, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- releaser.ReleaseDelivery(ctx, lease) }()
	defer func() { cancel(); rollback(tx); <-finished; releaser.Close() }()
	for {
		var waiting bool
		if err = s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock')", label).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("release never waited for row lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err = tx.Exec(ctx, "SELECT pg_sleep(2.1)"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	releaseErr := <-finished
	finished <- releaseErr // Retain the result for the cleanup join, including failures.
	if !errors.Is(releaseErr, ErrLeaseLost) {
		t.Fatalf("expired release accepted after lock wait: %v", releaseErr)
	}
	var state string
	if err = s.pool.QueryRow(ctx, "SELECT status FROM deliveries WHERE event_id=$1", lease.EventID).Scan(&state); err != nil || state != "leased" {
		t.Fatal("expired release changed state")
	}
}

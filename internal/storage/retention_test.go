package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func retentionSQL(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}
func ageTerminal(t *testing.T, s *Store, event string, hours int) {
	t.Helper()
	retentionSQL(t, s, "UPDATE deliveries SET terminal_at=clock_timestamp()-($2::bigint*interval '1 hour') WHERE event_id=$1", event, hours)
}
func retentionEvent(t *testing.T, s *Store, owner, destination, state string, hours int) (Event, string) {
	t.Helper()
	key := NewID()
	e, _, err := s.Ingest(context.Background(), owner, destination, key, sha256.Sum256([]byte(`{}`)), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	retentionSQL(t, s, `UPDATE events SET created_at=clock_timestamp()-interval '90 days' WHERE id=$1`, e.ID)
	retentionSQL(t, s, `UPDATE deliveries SET status=$2,
 attempt_count=CASE WHEN $2 IN ('pending','leased') THEN 0 ELSE 1 END,
 lease_owner=CASE WHEN $2 IN ('leased','attempting') THEN $3::uuid END,
 lease_token=CASE WHEN $2 IN ('leased','attempting') THEN $3::uuid END,
 lease_expires_at=CASE WHEN $2 IN ('leased','attempting') THEN clock_timestamp()+interval '1 minute' END,
 next_attempt_at=CASE WHEN $2='retry_wait' THEN clock_timestamp()+interval '1 minute' END
 WHERE event_id=$1`, e.ID, state, NewID())
	if state == "succeeded" || state == "failed" || state == "unknown" {
		ageTerminal(t, s, e.ID, hours)
	}
	return e, key
}

func TestRetentionWindowPreviewScopeAndBounds(t *testing.T) {
	s, owner, d := quotaFixture(t)
	ctx := context.Background()
	other, _, err := s.ProvisionClient(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	od, err := s.CreateDestination(ctx, other, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	retentionEvent(t, s, other, od.ID, "succeeded", 721)
	for _, state := range []string{"pending", "leased", "attempting", "retry_wait", "succeeded", "failed", "unknown"} {
		retentionEvent(t, s, owner, d.ID, state, 719)
	}
	for _, state := range []string{"succeeded", "failed", "unknown"} {
		retentionEvent(t, s, owner, d.ID, state, 721)
	}
	usage, err := s.ClientUsage(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PruneHistory(ctx, owner, 100, false)
	if err != nil || preview.Selected != 3 || preview.Deleted != 0 || preview.Applied || preview.RetentionDays != 30 {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	after, err := s.ClientUsage(ctx, owner)
	if err != nil || after != usage {
		t.Fatal("preview mutated data")
	}
	for _, limit := range []int{0, -1, 101} {
		if _, err = s.PruneHistory(ctx, owner, limit, true); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	if _, err = s.PruneHistory(ctx, NewID(), 1, true); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing owner accepted")
	}
	a, err := s.PruneHistory(ctx, owner, 2, true)
	if err != nil || a.Deleted != 2 {
		t.Fatalf("bounded batch: %+v %v", a, err)
	}
	b, err := s.PruneHistory(ctx, owner, 100, true)
	if err != nil || b.Deleted != 1 {
		t.Fatalf("remaining batch: %+v %v", b, err)
	}
	after, err = s.ClientUsage(ctx, owner)
	if err != nil || after.Events.Used != 7 || after.OpenDeliveries.Used != 4 || after.Destinations != usage.Destinations {
		t.Fatal("live/recent work or destinations changed")
	}
	foreign, err := s.ClientUsage(ctx, other)
	if err != nil || foreign.Events.Used != 1 {
		t.Fatal("foreign event deleted")
	}
}

func TestRetentionReplayClockAndCoordinatedExpiry(t *testing.T) {
	s, owner, event, _, _ := failedReplayFixture(t)
	ctx := context.Background()
	ageTerminal(t, s, event.ID, 721)
	body := []byte("{ \"z\": 9007199254740993, \"a\":1 }")
	hash := sha256.Sum256(body)
	// Age alone does not expire the ingestion receipt; it remains until deletion.
	same, dup, err := s.Ingest(ctx, owner, event.DestinationID, "attempt", hash, body)
	if err != nil || !dup || same.ID != event.ID {
		t.Fatal("receipt expired before deletion")
	}
	_, _, err = s.ReplayDelivery(ctx, owner, event.ID, "replay", hash)
	if err != nil {
		t.Fatal(err)
	}
	var terminal *time.Time
	if err = s.pool.QueryRow(ctx, "SELECT terminal_at FROM deliveries WHERE event_id=$1", event.ID).Scan(&terminal); err != nil || terminal != nil {
		t.Fatal("replay did not clear terminal clock")
	}
	result, err := s.PruneHistory(ctx, owner, 100, true)
	if err != nil || result.Deleted != 0 {
		t.Fatal("replay work removed")
	}
	lease := claimAttempt(t, s)
	work, err := s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 204}); err != nil {
		t.Fatal(err)
	}
	result, err = s.PruneHistory(ctx, owner, 100, true)
	if err != nil || result.Deleted != 0 {
		t.Fatal("new terminal outcome did not get a fresh window")
	}
	ageTerminal(t, s, event.ID, 721)
	result, err = s.PruneHistory(ctx, owner, 100, true)
	if err != nil || result.Deleted != 1 {
		t.Fatalf("coordinated deletion: %+v %v", result, err)
	}
	if _, err = s.GetDeliveryHistory(ctx, owner, event.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("history survived expiry")
	}
	if _, _, err = s.ReplayDelivery(ctx, owner, event.ID, "replay", hash); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired replay receipt survived")
	}
	var children int
	if err = s.pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM delivery_attempts)+(SELECT count(*) FROM delivery_replays)").Scan(&children); err != nil || children != 0 {
		t.Fatal("orphaned history")
	}
	if _, _, err = s.ActiveSigningSecret(ctx, owner, event.DestinationID); err != nil {
		t.Fatal("retention deleted signing keys")
	}
	fresh, dup, err := s.Ingest(ctx, owner, event.DestinationID, "attempt", hash, body)
	if err != nil || dup || fresh.ID == event.ID {
		t.Fatal("expired key did not create new event")
	}
}

func TestRetentionCommitFailureRollsBackWholeHistory(t *testing.T) {
	s, owner, event, _, _ := failedReplayFixture(t)
	ctx := context.Background()
	_, _, err := s.ReplayDelivery(ctx, owner, event.ID, "replay", sha256.Sum256([]byte("replay")))
	if err != nil {
		t.Fatal(err)
	}
	lease := claimAttempt(t, s)
	work, err := s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 204}); err != nil {
		t.Fatal(err)
	}
	ageTerminal(t, s, event.ID, 721)
	before, err := s.GetDeliveryHistory(ctx, owner, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	retentionSQL(t, s, `CREATE FUNCTION reject_retention() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected retention commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER reject_retention AFTER DELETE ON events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_retention()`)
	result, err := s.PruneHistory(ctx, owner, 100, true)
	if err == nil || result != (RetentionResult{}) {
		t.Fatal("failed commit reported a successful deletion")
	}
	after, err := s.GetDeliveryHistory(ctx, owner, event.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("failed commit lost delivery/replay/attempt history")
	}
	retentionSQL(t, s, "DROP TRIGGER reject_retention ON events; DROP FUNCTION reject_retention()")
	result, err = s.PruneHistory(ctx, owner, 100, true)
	if err != nil || result.Deleted != 1 {
		t.Fatal("retry after failed commit failed")
	}
}

func TestRetentionConcurrentPrunersAndLockedDelivery(t *testing.T) {
	s, owner, d := quotaFixture(t)
	ctx := context.Background()
	var first Event
	for i := 0; i < 8; i++ {
		e, _ := retentionEvent(t, s, owner, d.ID, "succeeded", 721)
		if i == 0 {
			first = e
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, "SELECT event_id FROM deliveries WHERE event_id=$1 FOR UPDATE", first.ID); err != nil {
		t.Fatal(err)
	}
	outputs := make(chan RetentionResult, 4)
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := s.PruneHistory(ctx, owner, 2, true); outputs <- r; errs <- e }()
	}
	wg.Wait()
	close(outputs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for r := range outputs {
		n += r.Deleted
	}
	if n != 7 {
		t.Fatalf("deleted=%d; locked row should survive", n)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := s.PruneHistory(ctx, owner, 100, true)
	if err != nil || r.Deleted != 1 {
		t.Fatal("skipped row not retried")
	}
}

func TestRetentionRacesIngestionAndReplay(t *testing.T) {
	for _, operation := range []string{"ingest", "replay"} {
		t.Run(operation, func(t *testing.T) {
			s, owner, event, _, _ := failedReplayFixture(t)
			ctx := context.Background()
			ageTerminal(t, s, event.ID, 721)
			var wg sync.WaitGroup
			wg.Add(2)
			var result RetentionResult
			var pruneErr, opErr error
			var duplicate bool
			var receipt Event
			go func() { defer wg.Done(); result, pruneErr = s.PruneHistory(ctx, owner, 100, true) }()
			go func() {
				defer wg.Done()
				if operation == "replay" {
					_, _, opErr = s.ReplayDelivery(ctx, owner, event.ID, "race", [32]byte{})
				} else {
					body := []byte("{ \"z\": 9007199254740993, \"a\":1 }")
					receipt, duplicate, opErr = s.Ingest(ctx, owner, event.DestinationID, "attempt", sha256.Sum256(body), body)
				}
			}()
			wg.Wait()
			if pruneErr != nil {
				t.Fatal(pruneErr)
			}
			if operation == "replay" {
				if result.Deleted == 1 {
					if !errors.Is(opErr, ErrNotFound) {
						t.Fatal("replay recreated deleted work")
					}
				} else {
					if opErr != nil {
						t.Fatal(opErr)
					}
					e, err := s.GetEvent(ctx, owner, event.ID)
					if err != nil || e.Status != "pending" {
						t.Fatal("replayed work deleted")
					}
				}
			} else {
				if opErr != nil {
					t.Fatal(opErr)
				}
				if duplicate && receipt.ID != event.ID {
					t.Fatal("duplicate identity changed")
				}
				if !duplicate && receipt.ID == event.ID {
					t.Fatal("new work reused expired identity")
				}
				// A receipt can be removed immediately after a duplicate response once its
				// window has elapsed. New ingestion must always survive the cleanup race.
				if !duplicate {
					if e, err := s.GetEvent(ctx, owner, receipt.ID); err != nil || e.Status != "pending" {
						t.Fatal("newly ingested work deleted")
					}
				}
			}
		})
	}
}

func TestRetentionMigrationGraceAndTerminalClock(t *testing.T) {
	s, _ := isolatedTestStore(t)
	ctx := context.Background()
	for _, schema := range []string{initialSchema, signingSchema, leaseSchema, attemptSchema, retrySchema, cooldownSchema, listingSchema, replaySchema, clientTokenSchema, requestLimitSchema} {
		retentionSQL(t, s, schema)
	}
	retentionSQL(t, s, "CREATE TABLE schema_migrations(version integer PRIMARY KEY); INSERT INTO schema_migrations SELECT generate_series(1,10)")
	owner, _, err := s.ProvisionClient(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDestination(ctx, owner, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	seedQuotaEvents(t, s, owner, d.ID, 1, "failed")
	retentionSQL(t, s, "UPDATE events SET created_at=clock_timestamp()-interval '90 days'")
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var original time.Time
	if err = s.pool.QueryRow(ctx, "SELECT terminal_at FROM deliveries").Scan(&original); err != nil {
		t.Fatal(err)
	}
	if time.Since(original) > time.Minute {
		t.Fatal("legacy history not granted grace period")
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := s.PruneHistory(ctx, owner, 100, true)
	if err != nil || result.Deleted != 0 {
		t.Fatal("migration immediately expired legacy history")
	}
	retentionSQL(t, s, "UPDATE deliveries SET status='failed'")
	var unchanged time.Time
	if err = s.pool.QueryRow(ctx, "SELECT terminal_at FROM deliveries").Scan(&unchanged); err != nil || !unchanged.Equal(original) {
		t.Fatal("same-state write reset retention")
	}
	// Database constraint rejects a terminal clock on open work.
	if _, err = s.pool.Exec(ctx, "UPDATE deliveries SET status='pending'"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE deliveries SET terminal_at=clock_timestamp()"); err == nil {
		t.Fatal("open work accepted terminal clock")
	}
}

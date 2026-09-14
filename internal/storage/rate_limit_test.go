package storage

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRequestLimitConcurrentPoolsAndRestart(t *testing.T) {
	s := tokenTestStore(t)
	ctx := context.Background()
	client, _, err := s.ProvisionClient(ctx, "limited")
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	// Repeat only if real database time crosses a minute during the burst.
	for attempt := 0; attempt < 3; attempt++ {
		if _, err = s.pool.Exec(ctx, "DELETE FROM client_request_limits WHERE client_id=$1", client); err != nil {
			t.Fatal(err)
		}
		var before, after time.Time
		if err = s.pool.QueryRow(ctx, "SELECT date_trunc('minute',clock_timestamp())").Scan(&before); err != nil {
			t.Fatal(err)
		}
		results := make(chan int, 160)
		failures := make(chan error, 160)
		var wg sync.WaitGroup
		for i := 0; i < 160; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				db := s
				if i%2 == 0 {
					db = other
				}
				retry, e := db.TakeRequest(ctx, client)
				if e != nil {
					failures <- e
					return
				}
				results <- retry
			}(i)
		}
		wg.Wait()
		close(results)
		close(failures)
		for e := range failures {
			t.Fatal(e)
		}
		if err = s.pool.QueryRow(ctx, "SELECT date_trunc('minute',clock_timestamp())").Scan(&after); err != nil {
			t.Fatal(err)
		}
		if !before.Equal(after) {
			continue
		}
		admitted := 0
		for retry := range results {
			if retry == 0 {
				admitted++
			} else if retry < 1 || retry > 60 {
				t.Fatalf("invalid retry %d", retry)
			}
		}
		if admitted != ClientRequestsPerMinute {
			t.Fatalf("admitted %d, want %d", admitted, ClientRequestsPerMinute)
		}
		restarted, e := Open(ctx, s.pool.Config().ConnString())
		if e != nil {
			t.Fatal(e)
		}
		retry, e := restarted.TakeRequest(ctx, client)
		restarted.Close()
		if e != nil {
			t.Fatal(e)
		}
		if err = s.pool.QueryRow(ctx, "SELECT date_trunc('minute',clock_timestamp())").Scan(&after); err != nil {
			t.Fatal(err)
		}
		if !before.Equal(after) {
			continue
		}
		if retry == 0 {
			t.Fatal("restart reset counter")
		}
		return
	}
	t.Fatal("could not complete burst within one database minute")
}

func TestRequestLimitResetIsolationAndRollback(t *testing.T) {
	s := tokenTestStore(t)
	ctx := context.Background()
	client, _, err := s.ProvisionClient(ctx, "limited")
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, err := s.ProvisionClient(ctx, "independent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO client_request_limits VALUES($1,date_trunc('minute',clock_timestamp())-interval '1 minute',120)`, client); err != nil {
		t.Fatal(err)
	}
	if retry, e := s.TakeRequest(ctx, client); e != nil || retry != 0 {
		t.Fatalf("reset: %d %v", retry, e)
	}
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT requests FROM client_request_limits WHERE client_id=$1", client).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reset count %d %v", count, err)
	}
	if retry, e := s.TakeRequest(ctx, foreign); e != nil || retry != 0 {
		t.Fatalf("isolation: %d %v", retry, e)
	}
	if _, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_limit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected admission failure'; END $$;
 CREATE CONSTRAINT TRIGGER reject_limit AFTER INSERT OR UPDATE ON client_request_limits DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_limit()`); err != nil {
		t.Fatal(err)
	}
	if _, e := s.TakeRequest(ctx, client); e == nil {
		t.Fatal("commit failure admitted request")
	}
	if err = s.pool.QueryRow(ctx, "SELECT requests FROM client_request_limits WHERE client_id=$1", client).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rollback count %d %v", count, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e := s.TakeRequest(canceled, client); e == nil {
		t.Fatal("canceled admission succeeded")
	}
	if _, e := s.TakeRequest(ctx, NewID()); e == nil {
		t.Fatal("unknown client admitted")
	}
}

func TestRequestLimitUpgradeFromNine(t *testing.T) {
	s, _ := isolatedTestStore(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, "CREATE TABLE schema_migrations(version integer PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	for i, sql := range []string{initialSchema, signingSchema, leaseSchema, attemptSchema, retrySchema, cooldownSchema, listingSchema, replaySchema, clientTokenSchema} {
		if _, err := s.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, fmt.Sprintf("INSERT INTO schema_migrations VALUES(%d)", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	client, token, err := s.ProvisionClient(ctx, "upgrade")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if id, e := s.Authenticate(ctx, token); e != nil || id != client {
		t.Fatal("migration changed credential")
	}
	if retry, e := s.TakeRequest(ctx, client); e != nil || retry != 0 {
		t.Fatalf("migrated admission: %d %v", retry, e)
	}
}

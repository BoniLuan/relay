package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func makeRetryDue(t *testing.T, s *Store, event string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), "UPDATE deliveries SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE event_id=$1", event); err != nil {
		t.Fatal(err)
	}
}
func TestRetriesStopAtThreeAndSurvivePoolRestart(t *testing.T) {
	s, _, _, e, _ := attemptFixture(t)
	ctx := context.Background()
	var previous Lease
	for n := 1; n <= 3; n++ {
		lease := claimAttempt(t, s)
		work, err := s.StartAttempt(ctx, lease)
		if err != nil {
			t.Fatal(err)
		}
		if work.Number != n {
			t.Fatalf("number=%d want=%d", work.Number, n)
		}
		if n > 1 && lease.token == previous.token {
			t.Fatal("retry reused lease token")
		}
		if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 503, ErrorCode: "http_status"}); err != nil {
			t.Fatal(err)
		}
		want := "retry_wait"
		if n == 3 {
			want = "failed"
		}
		assertAttemptState(t, s, e.ID, want, n)
		if _, err = s.ClaimDelivery(ctx, NewID(), time.Minute); !errors.Is(err, ErrNoDelivery) {
			t.Fatal("early or exhausted retry claimed")
		}
		if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 204}); !errors.Is(err, ErrLeaseLost) {
			t.Fatal("duplicate completion changed scheduling")
		}
		if n < 3 {
			var delay float64
			err = s.pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM d.next_attempt_at-a.finished_at) FROM deliveries d JOIN delivery_attempts a ON a.event_id=d.event_id WHERE d.event_id=$1 AND a.attempt_number=$2`, e.ID, n).Scan(&delay)
			seconds := 5 * (1 << (n - 1))
			low := float64(seconds)
			if err != nil || delay < low-1 || delay >= 2*low {
				t.Fatalf("delay=%f err=%v", delay, err)
			}
			// A fresh independent pool sees exactly the same persisted future schedule.
			other, err := Open(ctx, s.pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			_, err = other.ClaimDelivery(ctx, NewID(), time.Minute)
			other.Close()
			if !errors.Is(err, ErrNoDelivery) {
				t.Fatal("restart bypassed delay")
			}
			makeRetryDue(t, s, e.ID)
		}
		previous = lease
	}
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT attempt_count FROM deliveries WHERE event_id=$1", e.ID).Scan(&count); err != nil || count != 3 {
		t.Fatal("wrong attempt budget")
	}
}
func TestRetryPolicyAndSuccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result AttemptResult
		retry  bool
	}{
		{"408", AttemptResult{408, "http_status"}, true}, {"429", AttemptResult{429, "http_status"}, true},
		{"500", AttemptResult{500, "http_status"}, true}, {"599", AttemptResult{599, "http_status"}, true},
		{"network", AttemptResult{0, "network"}, true}, {"response after acceptance", AttemptResult{200, "response"}, true},
		{"canceled", AttemptResult{0, "canceled"}, true}, {"400", AttemptResult{400, "http_status"}, false},
		{"401", AttemptResult{401, "http_status"}, false}, {"redirect", AttemptResult{302, "http_status"}, false},
		{"SSRF", AttemptResult{0, "destination"}, false}, {"input", AttemptResult{0, "input"}, false},
		{"success", AttemptResult{204, ""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, e, _ := attemptFixture(t)
			ctx := context.Background()
			l := claimAttempt(t, s)
			w, err := s.StartAttempt(ctx, l)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.FinishAttempt(ctx, l, w.ID, tc.result); err != nil {
				t.Fatal(err)
			}
			want := "failed"
			if tc.retry {
				want = "retry_wait"
			}
			if tc.result.ErrorCode == "" {
				want = "succeeded"
			}
			assertAttemptState(t, s, e.ID, want, 1)
		})
	}
}
func TestConcurrentDueRetryClaims(t *testing.T) {
	s, _, _, e, _ := attemptFixture(t)
	ctx := context.Background()
	l := claimAttempt(t, s)
	w, err := s.StartAttempt(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAttempt(ctx, l, w.ID, AttemptResult{0, "network"}); err != nil {
		t.Fatal(err)
	}
	makeRetryDue(t, s, e.ID)
	var wg sync.WaitGroup
	claims := make(chan Lease, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := s.ClaimDelivery(ctx, NewID(), time.Minute)
			if err == nil {
				claims <- lease
			} else if !errors.Is(err, ErrNoDelivery) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(claims)
	n := 0
	for lease := range claims {
		n++
		work, err := s.StartAttempt(ctx, lease)
		if err != nil || work.Number != 2 {
			t.Errorf("retry start: %v", err)
		}
	}
	if n != 1 {
		t.Fatalf("claims=%d", n)
	}
}
func TestCrashRecoveryConsumesBudgetAndStops(t *testing.T) {
	s, _, _, e, _ := attemptFixture(t)
	ctx := context.Background()
	for n := 1; n <= 3; n++ {
		l := claimAttempt(t, s)
		w, err := s.StartAttempt(ctx, l)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(ctx, "UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE event_id=$1", e.ID); err != nil {
			t.Fatal(err)
		}
		recovered, err := s.RecoverAttempt(ctx)
		if err != nil || !recovered {
			t.Fatal("recovery failed")
		}
		want := "retry_wait"
		if n == 3 {
			want = "unknown"
		}
		assertAttemptState(t, s, e.ID, want, n)
		if err = s.FinishAttempt(ctx, l, w.ID, AttemptResult{204, ""}); !errors.Is(err, ErrLeaseLost) {
			t.Fatal("crashed worker overwrote retry")
		}
		if n < 3 {
			makeRetryDue(t, s, e.ID)
		}
	}
	if _, err := s.ClaimDelivery(ctx, NewID(), time.Minute); !errors.Is(err, ErrNoDelivery) {
		t.Fatal("crash loop exceeded budget")
	}
}
func TestRetrySchedulingRollsBackAtCommit(t *testing.T) {
	for _, phase := range []string{"finish", "recover"} {
		t.Run(phase, func(t *testing.T) {
			s, _, _, e, _ := attemptFixture(t)
			ctx := context.Background()
			l := claimAttempt(t, s)
			w, err := s.StartAttempt(ctx, l)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "recover" {
				if _, err = s.pool.Exec(ctx, "UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second'"); err != nil {
					t.Fatal(err)
				}
			}
			_, err = s.pool.Exec(ctx, `CREATE FUNCTION fail_retry() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected retry commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER fail_retry AFTER UPDATE ON delivery_attempts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_retry()`)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "finish" {
				err = s.FinishAttempt(ctx, l, w.ID, AttemptResult{0, "network"})
			} else {
				_, err = s.RecoverAttempt(ctx)
			}
			if err == nil {
				t.Fatal("injected commit succeeded")
			}
			assertAttemptState(t, s, e.ID, "attempting", 1)
			var intact bool
			if err = s.pool.QueryRow(ctx, `SELECT d.next_attempt_at IS NULL AND d.attempt_count=1 AND a.state='started' FROM deliveries d JOIN delivery_attempts a ON a.event_id=d.event_id WHERE d.event_id=$1`, e.ID).Scan(&intact); err != nil || !intact {
				t.Fatal("partial schedule survived rollback")
			}
		})
	}
}
func TestJitterBounds(t *testing.T) {
	for n := 1; n < 3; n++ {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			low := time.Duration(5*(1<<(n-1))) * time.Second
			for i := 0; i < 100; i++ {
				delay := retryDelay(n)
				if delay < low || delay >= 2*low {
					t.Fatalf("delay=%v", delay)
				}
			}
		})
	}
}

func TestV4UpgradePreservesTerminalHistoryAndBudget(t *testing.T) {
	s, _ := isolatedTestStore(t)
	ctx := context.Background()
	for _, schema := range []string{initialSchema, signingSchema, leaseSchema, attemptSchema} {
		if _, err := s.pool.Exec(ctx, schema); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, "CREATE TABLE schema_migrations(version integer PRIMARY KEY); INSERT INTO schema_migrations VALUES(1),(2),(3),(4)"); err != nil {
		t.Fatal(err)
	}
	client, _, err := provisionLegacyClient(ctx, s, "legacy retries")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDestination(ctx, client, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "INSERT INTO encryption_keys(id,canary) VALUES('fixture',decode('00','hex'))"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO signing_secrets(client_id,destination_id,version,key_id,ciphertext,state) VALUES($1,$2,1,'fixture',decode(repeat('00',32),'hex'),'revoked')`, client, d.ID); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, state := range []string{"succeeded", "failed", "unknown", "attempting"} {
		e, _, err := s.Ingest(ctx, client, d.ID, state, [32]byte{}, []byte(`null`))
		if err != nil {
			t.Fatal(err)
		}
		ids[state] = e.ID
		token := NewID()
		attemptState := state
		var status any
		var code any
		var finished any = time.Now()
		switch state {
		case "succeeded":
			status = 204
		case "failed":
			status = 503
			code = "http_status"
		case "unknown":
			code = "interrupted"
		case "attempting":
			attemptState = "started"
			finished = nil
		}
		if _, err = s.pool.Exec(ctx, `INSERT INTO delivery_attempts(id,event_id,destination_id,signing_version,lease_token,state,http_status,error_code,finished_at) VALUES($1,$2,$3,1,$4,$5,$6,$7,$8)`, NewID(), e.ID, d.ID, token, attemptState, status, code, finished); err != nil {
			t.Fatal(err)
		}
		if state == "attempting" {
			_, err = s.pool.Exec(ctx, "UPDATE deliveries SET status=$2,lease_owner=$3,lease_token=$4,lease_expires_at=clock_timestamp()-interval '1 second' WHERE event_id=$1", e.ID, state, NewID(), token)
		} else {
			_, err = s.pool.Exec(ctx, "UPDATE deliveries SET status=$2 WHERE event_id=$1", e.ID, state)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for state, id := range ids {
		assertAttemptState(t, s, id, state, 1)
		var valid bool
		if err = s.pool.QueryRow(ctx, `SELECT d.attempt_count=1 AND d.next_attempt_at IS NULL AND a.attempt_number=1 FROM deliveries d JOIN delivery_attempts a ON a.event_id=d.event_id WHERE d.event_id=$1`, id).Scan(&valid); err != nil || !valid {
			t.Fatal("legacy budget/history lost")
		}
	}
	if _, err = s.ClaimDelivery(ctx, NewID(), time.Minute); !errors.Is(err, ErrNoDelivery) {
		t.Fatal("migration reactivated terminal work")
	}
	if recovered, err := s.RecoverAttempt(ctx); err != nil || !recovered {
		t.Fatal("legacy started work not recovered")
	}
	assertAttemptState(t, s, ids["attempting"], "retry_wait", 1)
	for _, state := range []string{"succeeded", "failed", "unknown"} {
		assertAttemptState(t, s, ids[state], state, 1)
	}
}

func TestRetryBecomesClaimableByDatabaseClock(t *testing.T) {
	s, _, _, e, _ := attemptFixture(t)
	ctx := context.Background()
	lease := claimAttempt(t, s)
	w, err := s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAttempt(ctx, lease, w.ID, AttemptResult{0, "network"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE deliveries SET next_attempt_at=clock_timestamp()+interval '200 milliseconds' WHERE event_id=$1", e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimDelivery(ctx, NewID(), time.Minute); !errors.Is(err, ErrNoDelivery) {
		t.Fatal("future retry claimed")
	}
	if _, err = s.pool.Exec(ctx, "SELECT pg_sleep(0.25)"); err != nil {
		t.Fatal(err)
	}
	next := claimAttempt(t, s)
	if next.EventID != e.ID {
		t.Fatal("due retry not claimed")
	}
}

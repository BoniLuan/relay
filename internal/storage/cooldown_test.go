package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDestinationCooldownSkipsAllItsEventsAndPreservesBudget(t *testing.T) {
	s, client, d, e, _ := attemptFixture(t)
	ctx := context.Background()
	for _, key := range []string{"another blocked event", "third blocked event"} {
		if _, _, err := s.Ingest(ctx, client, d, key, [32]byte{}, []byte(`null`)); err != nil {
			t.Fatal(err)
		}
	}
	good, err := s.CreateDestination(ctx, client, "https://example.com/healthy")
	if err != nil {
		t.Fatal(err)
	}
	healthy, _, err := s.Ingest(ctx, client, good.ID, "healthy", [32]byte{}, []byte(`null`))
	if err != nil {
		t.Fatal(err)
	}
	lease := claimAttempt(t, s)
	if err = s.DeferDestination(ctx, lease); err != nil {
		t.Fatal(err)
	}
	assertAttemptState(t, s, e.ID, "pending", 0)
	var paused bool
	if err = s.pool.QueryRow(ctx, `SELECT delivery_paused_until>clock_timestamp()+interval '55 seconds' FROM destinations WHERE id=$1`, d).Scan(&paused); err != nil || !paused {
		t.Fatal("cooldown missing")
	}
	next := claimAttempt(t, s)
	if next.EventID != healthy.ID {
		t.Fatal("blocked destination starved healthy work")
	}
	if _, err = s.ClaimDelivery(ctx, NewID(), time.Minute); !errors.Is(err, ErrNoDelivery) {
		t.Fatal("another blocked event claimed")
	}
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT sum(attempt_count) FROM deliveries").Scan(&count); err != nil || count != 0 {
		t.Fatal("deferral consumed HTTP budget")
	}
	if _, err = s.pool.Exec(ctx, "UPDATE destinations SET delivery_paused_until=clock_timestamp()-interval '1 second' WHERE id=$1", d); err != nil {
		t.Fatal(err)
	}
	resumed := claimAttempt(t, s)
	if resumed.EventID != e.ID {
		t.Fatal("expired cooldown did not resume work")
	}
}
func TestDestinationDeferralFencesStartedExpiredAndReplacedLeases(t *testing.T) {
	for _, state := range []string{"started", "expired", "replaced"} {
		t.Run(state, func(t *testing.T) {
			s, _, d, e, _ := attemptFixture(t)
			ctx := context.Background()
			lease := claimAttempt(t, s)
			switch state {
			case "started":
				if _, err := s.StartAttempt(ctx, lease); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := s.pool.Exec(ctx, "UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second'"); err != nil {
					t.Fatal(err)
				}
			case "replaced":
				if err := s.ReleaseDelivery(ctx, lease); err != nil {
					t.Fatal(err)
				}
				claimAttempt(t, s)
			}
			if err := s.DeferDestination(ctx, lease); !errors.Is(err, ErrLeaseLost) {
				t.Fatal("stale deferral accepted")
			}
			var untouched bool
			if err := s.pool.QueryRow(ctx, "SELECT delivery_paused_until IS NULL FROM destinations WHERE id=$1", d).Scan(&untouched); err != nil || !untouched {
				t.Fatal("stale worker paused destination")
			}
			want := "leased"
			count := 0
			if state == "started" {
				want = "attempting"
				count = 1
			}
			assertAttemptState(t, s, e.ID, want, count)
		})
	}
}
func TestDeferralCommitFailureRollsBackPauseAndRelease(t *testing.T) {
	s, _, d, e, _ := attemptFixture(t)
	ctx := context.Background()
	lease := claimAttempt(t, s)
	_, err := s.pool.Exec(ctx, `CREATE FUNCTION fail_deferral() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected cooldown commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER fail_deferral AFTER UPDATE ON destinations DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_deferral()`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeferDestination(ctx, lease); err == nil {
		t.Fatal("commit should fail")
	}
	assertAttemptState(t, s, e.ID, "leased", 0)
	var clear bool
	if err = s.pool.QueryRow(ctx, "SELECT delivery_paused_until IS NULL FROM destinations WHERE id=$1", d).Scan(&clear); err != nil || !clear {
		t.Fatal("partial deferral committed")
	}
}

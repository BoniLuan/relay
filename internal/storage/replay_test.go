package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func failedReplayFixture(t *testing.T) (*Store, string, Event, Lease, AttemptWork) {
	t.Helper()
	s, owner, _, event, _ := attemptFixture(t)
	ctx := context.Background()
	lease := claimAttempt(t, s)
	work, err := s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 400, ErrorCode: "http_status"}); err != nil {
		t.Fatal(err)
	}
	return s, owner, event, lease, work
}

func TestReplayOwnershipConcurrencyAndIdempotency(t *testing.T) {
	for _, sameKey := range []bool{true, false} {
		t.Run(fmt.Sprintf("same_key_%t", sameKey), func(t *testing.T) {
			s, owner, event, oldLease, oldWork := failedReplayFixture(t)
			ctx := context.Background()
			hash := sha256.Sum256([]byte("request"))
			for _, ids := range [][2]string{{NewID(), event.ID}, {owner, NewID()}} {
				if _, _, err := s.ReplayDelivery(ctx, ids[0], ids[1], "key", hash); !errors.Is(err, ErrNotFound) {
					t.Fatalf("ownership: %v", err)
				}
			}
			type result struct {
				receipt   ReplayReceipt
				duplicate bool
				err       error
				key       string
			}
			out := make(chan result, 8)
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					key := "key"
					if !sameKey {
						key = fmt.Sprint(i)
					}
					r, d, e := s.ReplayDelivery(ctx, owner, event.ID, key, hash)
					out <- result{r, d, e, key}
				}(i)
			}
			wg.Wait()
			close(out)
			created := 0
			var winner result
			for r := range out {
				if r.err != nil {
					if sameKey || !errors.Is(r.err, ErrReplayConflict) {
						t.Fatal(r.err)
					}
					continue
				}
				if !r.duplicate {
					created++
					winner = r
				}
			}
			if created != 1 || winner.receipt.ID == "" || winner.receipt.MaxAttempts != 4 || winner.receipt.PreviousAttemptCount != 1 {
				t.Fatal("replay budget or winner incorrect")
			}
			h, err := s.GetDeliveryHistory(ctx, owner, event.ID)
			if err != nil || h.Status != "pending" || h.AttemptCount != 1 || len(h.Attempts) != 1 || h.Replay == nil || h.Replay.ID != winner.receipt.ID || h.MaxAttempts != 4 {
				t.Fatalf("lost audit/history: %+v %v", h, err)
			}
			if _, _, err = s.ReplayDelivery(ctx, owner, event.ID, winner.key, sha256.Sum256([]byte("changed"))); !errors.Is(err, ErrReplayConflict) {
				t.Fatal("changed input accepted")
			}
			lease := claimAttempt(t, s)
			if err = s.FinishAttempt(ctx, oldLease, oldWork.ID, AttemptResult{StatusCode: 204}); !errors.Is(err, ErrLeaseLost) {
				t.Fatal("old worker overwrote replay")
			}
			work, err := s.StartAttempt(ctx, lease)
			if err != nil {
				t.Fatal(err)
			}
			if work.Number != 2 || work.EventID != oldWork.EventID || string(work.Payload) != string(oldWork.Payload) {
				t.Fatal("replay changed event or payload")
			}
			if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 204}); err != nil {
				t.Fatal(err)
			}
			// Ambiguous/lost replay HTTP responses can be retried after work completes.
			r, duplicate, err := s.ReplayDelivery(ctx, owner, event.ID, winner.key, hash)
			if err != nil || !duplicate || r.ID != winner.receipt.ID || !r.RequestedAt.Equal(winner.receipt.RequestedAt) {
				t.Fatal("receipt is not durable/idempotent")
			}
			assertAttemptState(t, s, event.ID, "succeeded", 2)
			e, duplicate, err := s.Ingest(ctx, owner, event.DestinationID, "attempt", sha256.Sum256(oldWork.Payload), oldWork.Payload)
			if err != nil || !duplicate || e.ID != event.ID || e.Status != "succeeded" {
				t.Fatal("ingestion idempotency changed")
			}
			var actor string
			if err = s.pool.QueryRow(ctx, "SELECT requested_by::text FROM delivery_replays WHERE event_id=$1", event.ID).Scan(&actor); err != nil || actor != owner {
				t.Fatal("missing authenticated actor")
			}
		})
	}
}

func TestReplayRejectsLiveAndSuccessfulDeliveries(t *testing.T) {
	s, owner, _, event, _ := attemptFixture(t)
	ctx := context.Background()
	reject := func() {
		t.Helper()
		if _, _, err := s.ReplayDelivery(ctx, owner, event.ID, "key", [32]byte{}); !errors.Is(err, ErrReplayState) {
			t.Fatalf("unexpected eligibility: %v", err)
		}
	}
	reject() // pending
	lease := claimAttempt(t, s)
	reject() // leased
	work, err := s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	reject() // attempting
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 503, ErrorCode: "http_status"}); err != nil {
		t.Fatal(err)
	}
	reject() // retry_wait
	makeRetryDue(t, s, event.ID)
	lease = claimAttempt(t, s)
	work, err = s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 204}); err != nil {
		t.Fatal(err)
	}
	reject() // succeeded
}

func TestReplayBudgetSurvivesFailuresCrashesAndRestart(t *testing.T) {
	for _, initial := range []int{1, 2, 3} {
		for _, crash := range []bool{false, true} {
			t.Run(fmt.Sprintf("initial_%d_crash_%t", initial, crash), func(t *testing.T) {
				s, owner, _, event, _ := attemptFixture(t)
				ctx := context.Background()
				for number := 1; number <= initial+3; number++ {
					if number == initial+1 {
						r, dup, err := s.ReplayDelivery(ctx, owner, event.ID, "one-replay", [32]byte{})
						if err != nil || dup || r.MaxAttempts != initial+3 {
							t.Fatal("replay after exhausted budget failed")
						}
						other, err := Open(ctx, s.pool.Config().ConnString())
						if err != nil {
							t.Fatal(err)
						}
						h, err := other.GetDeliveryHistory(ctx, owner, event.ID)
						other.Close()
						if err != nil || h.MaxAttempts != initial+3 || h.Replay == nil {
							t.Fatal("replay did not survive restart")
						}
					}
					lease := claimAttempt(t, s)
					work, err := s.StartAttempt(ctx, lease)
					if err != nil || work.Number != number {
						t.Fatalf("start %d: %v", number, err)
					}
					if crash && number > initial {
						if _, err = s.pool.Exec(ctx, "UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE event_id=$1", event.ID); err != nil {
							t.Fatal(err)
						}
						if _, err = s.RecoverAttempt(ctx); err != nil {
							t.Fatal(err)
						}
					} else {
						status := 503
						if number == initial {
							status = 400
						}
						if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: status, ErrorCode: "http_status"}); err != nil {
							t.Fatal(err)
						}
					}
					if number < initial || (number > initial && number < initial+3) {
						var delay float64
						if err = s.pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM d.next_attempt_at-a.finished_at) FROM deliveries d JOIN delivery_attempts a ON a.event_id=d.event_id WHERE d.event_id=$1 AND a.attempt_number=$2`, event.ID, number).Scan(&delay); err != nil {
							t.Fatal(err)
						}
						roundNumber := number
						if number > initial {
							roundNumber = number - initial
						}
						floorSeconds := 5 * (1 << (roundNumber - 1))
						floor := float64(floorSeconds)
						if delay < floor-1 || delay >= 2*floor {
							t.Fatalf("wrong round delay: %f", delay)
						}
						makeRetryDue(t, s, event.ID)
					}
				}
				state := "failed"
				if crash {
					state = "unknown"
				}
				assertAttemptState(t, s, event.ID, state, initial+3)
				if _, err := s.ClaimDelivery(ctx, NewID(), time.Minute); !errors.Is(err, ErrNoDelivery) {
					t.Fatal("seventh attempt allowed")
				}
				if _, _, err := s.ReplayDelivery(ctx, owner, event.ID, "second-replay", [32]byte{}); !errors.Is(err, ErrReplayConflict) {
					t.Fatal("second replay allowed")
				}
				h, err := s.GetDeliveryHistory(ctx, owner, event.ID)
				if err != nil || len(h.Attempts) != initial+3 || h.Attempts[initial+2].Number != initial+3 {
					t.Fatal("history truncated")
				}
			})
		}
	}
}

func TestReplayRollbackAndUnknownEligibility(t *testing.T) {
	s, owner, event, _, _ := failedReplayFixture(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_replay() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected replay commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER reject_replay AFTER INSERT ON delivery_replays DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_replay()`); err != nil {
		t.Fatal(err)
	}
	receipt, _, err := s.ReplayDelivery(ctx, owner, event.ID, "key", [32]byte{})
	if err == nil || receipt.ID != "" {
		t.Fatal("failed commit disclosed acceptance")
	}
	h, err := s.GetDeliveryHistory(ctx, owner, event.ID)
	if err != nil || h.Status != "failed" || h.MaxAttempts != 3 || h.Replay != nil {
		t.Fatal("partial replay committed")
	}
	if _, err = s.ClaimDelivery(ctx, NewID(), time.Minute); !errors.Is(err, ErrNoDelivery) {
		t.Fatal("rollback left claimable work")
	}
	if _, err = s.pool.Exec(ctx, "DROP TRIGGER reject_replay ON delivery_replays"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ReplayDelivery(ctx, owner, event.ID, "key", [32]byte{}); err != nil {
		t.Fatal(err)
	}
	// Exhaust original budget with crash recovery, without any replay grant.
	other, client, _, unknown, _ := attemptFixture(t)
	for i := 0; i < 3; i++ {
		l := claimAttempt(t, other)
		if _, err = other.StartAttempt(ctx, l); err != nil {
			t.Fatal(err)
		}
		if _, err = other.pool.Exec(ctx, "UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE event_id=$1", unknown.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = other.RecoverAttempt(ctx); err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			makeRetryDue(t, other, unknown.ID)
		}
	}
	if _, _, err = other.ReplayDelivery(ctx, client, unknown.ID, "key", [32]byte{}); !errors.Is(err, ErrReplayState) {
		t.Fatal("unknown terminal outcome replayed")
	}
}

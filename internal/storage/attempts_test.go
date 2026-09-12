package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/BoniLuan/relay/internal/delivery"
)

func attemptFixture(t *testing.T) (*Store, string, string, Event, delivery.Secret) {
	t.Helper()
	s, _, _ := signingTestStore(t)
	ctx := context.Background()
	client, _, err := s.ProvisionClient(ctx, "attempt owner")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDestination(ctx, client, "https://example.com/hook?private=value")
	if err != nil {
		t.Fatal(err)
	}
	meta, secret, err := s.StageSigningSecret(ctx, client, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateSigningSecret(ctx, client, d.ID, meta.Version); err != nil {
		t.Fatal(err)
	}
	body := []byte("{ \"z\": 9007199254740993, \"a\":1 }")
	e, _, err := s.Ingest(ctx, client, d.ID, "attempt", sha256.Sum256(body), body)
	if err != nil {
		t.Fatal(err)
	}
	return s, client, d.ID, e, secret
}
func claimAttempt(t *testing.T, s *Store) Lease {
	t.Helper()
	l, err := s.ClaimDelivery(context.Background(), NewID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func assertAttemptState(t *testing.T, s *Store, event, state string, count int) {
	t.Helper()
	var got string
	var n int
	err := s.pool.QueryRow(context.Background(), "SELECT status,(SELECT count(*) FROM delivery_attempts WHERE event_id=$1) FROM deliveries WHERE event_id=$1", event).Scan(&got, &n)
	if err != nil || got != state || n != count {
		t.Fatalf("state=%s attempts=%d err=%v", got, n, err)
	}
}
func TestAttemptPinsSecretAndExactPayload(t *testing.T) {
	s, client, d, e, secret := attemptFixture(t)
	ctx := context.Background()
	lease := claimAttempt(t, s)
	work, err := s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if work.EventID != e.ID || work.SigningVersion != 1 || work.Secret.Export() != secret.Export() || string(work.Payload) != "{ \"z\": 9007199254740993, \"a\":1 }" {
		t.Fatal("wrong work snapshot")
	}
	encoded, _ := json.Marshal(work)
	for _, safe := range []string{fmt.Sprintf("%#v", work), string(encoded)} {
		if bytes.Contains([]byte(safe), []byte("private")) || bytes.Contains([]byte(safe), work.Payload) || bytes.Contains([]byte(safe), []byte(secret.Export())) {
			t.Fatal("work leaked")
		}
	}
	meta, _, err := s.StageSigningSecret(ctx, client, d)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateSigningSecret(ctx, client, d, meta.Version); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeSigningSecret(ctx, client, d, 1); err != nil {
		t.Fatal(err)
	}
	// Rotation/revocation after start cannot retroactively erase in-flight work.
	if work.SigningVersion != 1 || work.Secret.Export() != secret.Export() {
		t.Fatal("pinned key changed")
	}
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 204}); err != nil {
		t.Fatal(err)
	}
	assertAttemptState(t, s, e.ID, "succeeded", 1)
	if _, err = s.ClaimDelivery(ctx, NewID(), time.Minute); !errors.Is(err, ErrNoDelivery) {
		t.Fatalf("terminal claim=%v", err)
	}
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 500, ErrorCode: "http_status"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("duplicate finish=%v", err)
	}
	// The next event uses the newly active version.
	body := []byte(`null`)
	_, _, err = s.Ingest(ctx, client, d, "next", sha256.Sum256(body), body)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.StartAttempt(ctx, claimAttempt(t, s))
	if err != nil || next.SigningVersion != 2 {
		t.Fatalf("rotation not observed: %v", err)
	}
}
func TestAttemptPreparationFailsClosed(t *testing.T) {
	for _, tc := range []string{"revoked", "corrupt", "stale", "short lease"} {
		t.Run(tc, func(t *testing.T) {
			s, client, d, e, _ := attemptFixture(t)
			ctx := context.Background()
			lease := claimAttempt(t, s)
			switch tc {
			case "revoked":
				if err := s.RevokeSigningSecret(ctx, client, d, 1); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if _, err := s.pool.Exec(ctx, "UPDATE signing_secrets SET ciphertext=decode(repeat('00',32),'hex')"); err != nil {
					t.Fatal(err)
				}
			case "stale":
				lease.token = NewID()
			case "short lease":
				if _, err := s.pool.Exec(ctx, "UPDATE deliveries SET lease_expires_at=clock_timestamp()+interval '1 second'"); err != nil {
					t.Fatal(err)
				}
			}
			work, err := s.StartAttempt(ctx, lease)
			if err == nil || work.ID != "" || work.Secret.Export() != "" {
				t.Fatal("preparation returned work")
			}
			assertAttemptState(t, s, e.ID, "leased", 0)
		})
	}
}
func TestAttemptStartAndFinishCommitFailures(t *testing.T) {
	for _, phase := range []string{"start", "finish"} {
		t.Run(phase, func(t *testing.T) {
			s, _, _, e, _ := attemptFixture(t)
			ctx := context.Background()
			lease := claimAttempt(t, s)
			var work AttemptWork
			var err error
			if phase == "finish" {
				work, err = s.StartAttempt(ctx, lease)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_attempt_commit() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'injected attempt commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER reject_attempt AFTER INSERT OR UPDATE ON delivery_attempts
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_attempt_commit()`)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "start" {
				work, err = s.StartAttempt(ctx, lease)
				if err == nil || work.ID != "" || work.Secret.Export() != "" {
					t.Fatal("failed commit disclosed work")
				}
				assertAttemptState(t, s, e.ID, "leased", 0)
			} else {
				err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 204})
				if err == nil {
					t.Fatal("commit should fail")
				}
				assertAttemptState(t, s, e.ID, "attempting", 1)
				var state string
				s.pool.QueryRow(ctx, "SELECT state FROM delivery_attempts").Scan(&state)
				if state != "started" {
					t.Fatal("partial finish committed")
				}
			}
			if _, err = s.pool.Exec(ctx, "DROP TRIGGER reject_attempt ON delivery_attempts"); err != nil {
				t.Fatal(err)
			}
			if phase == "start" {
				work, err = s.StartAttempt(ctx, lease)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 204}); err != nil {
				t.Fatal(err)
			}
			assertAttemptState(t, s, e.ID, "succeeded", 1)
		})
	}
}
func TestInterruptedAttemptIsUnknownAndNeverRequeued(t *testing.T) {
	s, _, _, e, _ := attemptFixture(t)
	ctx := context.Background()
	lease := claimAttempt(t, s)
	work, err := s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseDelivery(ctx, lease); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("released started work: %v", err)
	}
	if recovered, err := s.RecoverAttempt(ctx); err != nil || recovered {
		t.Fatal("recovered live attempt")
	}
	if _, err = s.pool.Exec(ctx, "UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 204}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired finish=%v", err)
	}
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.RecoverAttempt(ctx)
			if err != nil {
				t.Error(err)
			}
			results <- ok
		}()
	}
	wg.Wait()
	close(results)
	n := 0
	for ok := range results {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("recoveries=%d", n)
	}
	assertAttemptState(t, s, e.ID, "unknown", 1)
	var state, code string
	err = s.pool.QueryRow(ctx, "SELECT state,error_code FROM delivery_attempts").Scan(&state, &code)
	if err != nil || state != "unknown" || code != "interrupted" {
		t.Fatal("missing unknown history")
	}
	if _, err = s.ClaimDelivery(ctx, NewID(), time.Minute); !errors.Is(err, ErrNoDelivery) {
		t.Fatal("unknown attempt requeued")
	}
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 204}); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("stale worker overwrote recovery")
	}
}
func TestAttemptWrongIDRollsBackDeliveryAndLegacyPayload(t *testing.T) {
	s, _, _, e, _ := attemptFixture(t)
	ctx := context.Background()
	lease := claimAttempt(t, s)
	if _, err := s.pool.Exec(ctx, "UPDATE events SET payload_bytes=NULL WHERE id=$1", e.ID); err != nil {
		t.Fatal(err)
	}
	work, err := s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(work.Payload) || !bytes.Contains(work.Payload, []byte("9007199254740993")) {
		t.Fatal("legacy payload corrupted")
	}
	if err = s.FinishAttempt(ctx, lease, NewID(), AttemptResult{StatusCode: 204}); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("wrong attempt accepted")
	}
	assertAttemptState(t, s, e.ID, "attempting", 1)
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 503, ErrorCode: "http_status"}); err != nil {
		t.Fatal(err)
	}
	assertAttemptState(t, s, e.ID, "failed", 1)
}

func TestAttemptRejectsIncompleteUnknownState(t *testing.T) {
	s, _, _, e, _ := attemptFixture(t)
	ctx := context.Background()
	if _, err := s.StartAttempt(ctx, claimAttempt(t, s)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE delivery_attempts SET state='unknown',finished_at=clock_timestamp()"); err == nil {
		t.Fatal("unknown without reason accepted")
	}
	assertAttemptState(t, s, e.ID, "attempting", 1)
}

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDeliveryHistoryLifecycleAndOwnership(t *testing.T) {
	s, owner, _, event, secret := attemptFixture(t)
	ctx := context.Background()
	read := func(status string, count int) DeliveryHistory {
		t.Helper()
		h, err := s.GetDeliveryHistory(ctx, owner, event.ID)
		if err != nil {
			t.Fatal(err)
		}
		if h.EventID != event.ID || h.Status != status || h.AttemptCount != count || h.MaxAttempts != 3 || len(h.Attempts) != count || h.Attempts == nil {
			t.Fatalf("unexpected history: %+v", h)
		}
		for i, a := range h.Attempts {
			if a.Number != i+1 || a.ID == "" || a.StartedAt.IsZero() || a.SigningVersion != 1 {
				t.Fatalf("invalid ordered metadata: %+v", a)
			}
		}
		raw, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{secret.Export(), "private=value", "payload", "ciphertext", "lease_token", "lease_owner", "9007199254740993"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatal("history exposed private data")
			}
		}
		return h
	}
	empty := read("pending", 0)
	if empty.NextAttemptAt != nil {
		t.Fatal("pending event has retry schedule")
	}
	for _, ids := range [][2]string{{NewID(), event.ID}, {owner, NewID()}} {
		if _, err := s.GetDeliveryHistory(ctx, ids[0], ids[1]); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ownership lookup: %v", err)
		}
	}
	lease := claimAttempt(t, s)
	read("leased", 0)
	work, err := s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	started := read("attempting", 1).Attempts[0]
	if started.ID != work.ID || started.FinishedAt != nil || started.HTTPStatus != nil || started.ErrorCode != nil {
		t.Fatal("invented in-flight outcome")
	}
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 503, ErrorCode: "http_status"}); err != nil {
		t.Fatal(err)
	}
	retry := read("retry_wait", 1)
	if retry.NextAttemptAt == nil || retry.Attempts[0].FinishedAt == nil || retry.Attempts[0].HTTPStatus == nil || *retry.Attempts[0].HTTPStatus != 503 || retry.Attempts[0].ErrorCode == nil || *retry.Attempts[0].ErrorCode != "http_status" {
		t.Fatal("missing retry metadata")
	}
	makeRetryDue(t, s, event.ID)
	lease = claimAttempt(t, s)
	if _, err = s.StartAttempt(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE event_id=$1", event.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecoverAttempt(ctx); err != nil {
		t.Fatal(err)
	}
	recovered := read("retry_wait", 2).Attempts[1]
	if recovered.State != "unknown" || recovered.HTTPStatus != nil || recovered.FinishedAt == nil || recovered.ErrorCode == nil || *recovered.ErrorCode != "interrupted" {
		t.Fatal("lost uncertain outcome")
	}
	makeRetryDue(t, s, event.ID)
	lease = claimAttempt(t, s)
	work, err = s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAttempt(ctx, lease, work.ID, AttemptResult{StatusCode: 204}); err != nil {
		t.Fatal(err)
	}
	done := read("succeeded", 3)
	if done.NextAttemptAt != nil || done.Attempts[2].State != "succeeded" || done.Attempts[2].HTTPStatus == nil || *done.Attempts[2].HTTPStatus != 204 || done.Attempts[2].ErrorCode != nil {
		t.Fatal("invalid success metadata")
	}
	// A fresh pool needs no signing keyring to read safe history.
	other, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	h, err := other.GetDeliveryHistory(ctx, owner, event.ID)
	if err != nil || len(h.Attempts) != 3 || h.Attempts[1].State != "unknown" {
		t.Fatal("history did not survive pool restart")
	}
}

func TestDeliveryHistoryCommittedSnapshot(t *testing.T) {
	s, owner, _, event, _ := attemptFixture(t)
	ctx := context.Background()
	lease := claimAttempt(t, s)
	work, err := s.StartAttempt(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	// Hold an in-progress finalization transaction. Readers must neither wait on
	// its row locks nor see its uncommitted result or scheduling changes.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	_, err = tx.Exec(ctx, `UPDATE delivery_attempts SET state='failed',finished_at=clock_timestamp(),http_status=400,error_code='http_status' WHERE id=$1`, work.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `UPDATE deliveries SET status='failed',lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL WHERE event_id=$1`, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	before, err := s.GetDeliveryHistory(readCtx, owner, event.ID)
	if err != nil || before.Status != "attempting" || len(before.Attempts) != 1 || before.Attempts[0].State != "started" {
		t.Fatalf("uncommitted state visible or reader blocked: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetDeliveryHistory(ctx, owner, event.ID)
	if err != nil || after.Status != "failed" || after.Attempts[0].State != "failed" || after.NextAttemptAt != nil {
		t.Fatal("committed failure missing")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err = s.GetDeliveryHistory(canceled, owner, event.ID); err == nil {
		t.Fatal("canceled read succeeded")
	}
}

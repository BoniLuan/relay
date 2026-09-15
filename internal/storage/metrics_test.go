package storage

import (
	"context"
	"testing"
)

func TestOperationalSnapshot(t *testing.T) {
	s, client, d := quotaFixture(t)
	ctx := context.Background()
	m, err := s.OperationalSnapshot(ctx)
	if err != nil || m != (OperationalMetrics{}) {
		t.Fatalf("empty snapshot %+v %v", m, err)
	}
	seedQuotaEvents(t, s, client, d.ID, 3, "pending")
	if _, err = s.pool.Exec(ctx, `UPDATE deliveries SET created_at=clock_timestamp()-interval '10 minutes';
 UPDATE deliveries SET status='leased',lease_owner=event_id,lease_token=event_id,lease_expires_at=clock_timestamp()-interval '1 second'
 WHERE event_id=(SELECT id FROM events WHERE idempotency_key='quota-1');
 UPDATE deliveries SET status='succeeded' WHERE event_id=(SELECT id FROM events WHERE idempotency_key='quota-2')`); err != nil {
		t.Fatal(err)
	}
	m, err = s.OperationalSnapshot(ctx)
	if err != nil || m.Deliveries != ([7]int64{1, 1, 0, 0, 1, 0, 0}) || m.ExpiredLeases != 1 || m.OpenOldestAgeSeconds < 599 {
		t.Fatalf("snapshot %+v %v", m, err)
	}
	other, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	again, err := other.OperationalSnapshot(ctx)
	if err != nil || again.Deliveries != m.Deliveries || again.ExpiredLeases != 1 {
		t.Fatal("reconnection lost metrics")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if m, err = s.OperationalSnapshot(canceled); err == nil || m != (OperationalMetrics{}) {
		t.Fatal("canceled snapshot succeeded")
	}
}
func TestOperationalAttemptOutcomes(t *testing.T) {
	s, _, _, _, _ := failedReplayFixture(t)
	m, err := s.OperationalSnapshot(context.Background())
	if err != nil || m.Deliveries[5] != 1 || m.Attempts[2] != 1 || m.OpenOldestAgeSeconds != 0 {
		t.Fatalf("failed outcome %+v %v", m, err)
	}
}

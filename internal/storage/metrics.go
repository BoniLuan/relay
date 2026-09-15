package storage

import (
	"context"
	"errors"
)

// OperationalMetrics contains aggregate persisted state, never tenant identifiers.
// These are gauges over retained data, not monotonic lifetime counters.
type OperationalMetrics struct {
	Deliveries           [7]int64
	Attempts             [4]int64
	OpenOldestAgeSeconds float64
	ExpiredLeases        int64
}

// OperationalSnapshot uses one statement snapshot and the database clock. It does
// not claim, repair or modify work. All rows contribute, including legacy data.
func (s *Store) OperationalSnapshot(ctx context.Context) (OperationalMetrics, error) {
	var m OperationalMetrics
	err := s.pool.QueryRow(ctx, `SELECT
 count(*) FILTER (WHERE status='pending'),
 count(*) FILTER (WHERE status='leased'),
 count(*) FILTER (WHERE status='attempting'),
 count(*) FILTER (WHERE status='retry_wait'),
 count(*) FILTER (WHERE status='succeeded'),
 count(*) FILTER (WHERE status='failed'),
 count(*) FILTER (WHERE status='unknown'),
 COALESCE(GREATEST(0,extract(epoch FROM statement_timestamp()-min(created_at) FILTER
 (WHERE status IN ('pending','leased','attempting','retry_wait')))),0)::double precision,
 count(*) FILTER (WHERE status IN ('leased','attempting') AND lease_expires_at<=statement_timestamp()),
 (SELECT count(*) FROM delivery_attempts WHERE state='started'),
 (SELECT count(*) FROM delivery_attempts WHERE state='succeeded'),
 (SELECT count(*) FROM delivery_attempts WHERE state='failed'),
 (SELECT count(*) FROM delivery_attempts WHERE state='unknown')
 FROM deliveries`).Scan(&m.Deliveries[0], &m.Deliveries[1], &m.Deliveries[2], &m.Deliveries[3], &m.Deliveries[4], &m.Deliveries[5], &m.Deliveries[6], &m.OpenOldestAgeSeconds, &m.ExpiredLeases, &m.Attempts[0], &m.Attempts[1], &m.Attempts[2], &m.Attempts[3])
	if err != nil {
		return OperationalMetrics{}, errors.New("operational snapshot unavailable")
	}
	return m, nil
}

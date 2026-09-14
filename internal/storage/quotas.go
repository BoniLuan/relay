package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

const (
	MaxClientDestinations   = 20
	MaxClientEvents         = 1000
	MaxClientOpenDeliveries = 100
)

// QuotaError is a capacity conflict, not a time-based rate-limit rejection.
type QuotaError struct {
	Resource string `json:"resource"`
	Limit    int    `json:"limit"`
}

func (*QuotaError) Error() string { return "client quota exceeded" }

type QuotaUsage struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
}
type ClientUsage struct {
	Destinations   QuotaUsage `json:"destinations"`
	Events         QuotaUsage `json:"events"`
	OpenDeliveries QuotaUsage `json:"open_deliveries"`
}

// Admission writers always lock the client before any destination/delivery row.
// NO KEY UPDATE serializes writers without blocking unrelated foreign-key checks.
// Workers only preserve or reduce open work; they do not need this lock.
func lockQuotaClient(ctx context.Context, tx pgx.Tx, client string) error {
	var id string
	err := tx.QueryRow(ctx, "SELECT id::text FROM clients WHERE id=$1 FOR NO KEY UPDATE", client).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// The caller holds the client lock. Ingestion checks before inserting a delivery;
// replay checks before reactivating its existing terminal delivery.
// LIMIT bounds counting for pre-existing clients already above the quota.
func checkOpenQuota(ctx context.Context, tx pgx.Tx, client string) error {
	var full bool
	err := tx.QueryRow(ctx, `SELECT count(*) >= $2 FROM (
 SELECT 1 FROM events e JOIN deliveries d ON d.event_id=e.id
 WHERE e.client_id=$1 AND d.status IN ('pending','leased','attempting','retry_wait') LIMIT $2
 ) open_work`, client, MaxClientOpenDeliveries).Scan(&full)
	if err != nil {
		return err
	}
	if full {
		return &QuotaError{Resource: "open_deliveries", Limit: MaxClientOpenDeliveries}
	}
	return nil
}

// ClientUsage is one statement snapshot, not a capacity reservation. Completed
// events remain retained; querying usage never deletes or expires idempotency.
func (s *Store) ClientUsage(ctx context.Context, client string) (ClientUsage, error) {
	usage := ClientUsage{
		Destinations:   QuotaUsage{Limit: MaxClientDestinations},
		Events:         QuotaUsage{Limit: MaxClientEvents},
		OpenDeliveries: QuotaUsage{Limit: MaxClientOpenDeliveries},
	}
	err := s.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM destinations WHERE client_id=c.id),
 (SELECT count(*) FROM events WHERE client_id=c.id),
 (SELECT count(*) FROM events e JOIN deliveries d ON d.event_id=e.id
 WHERE e.client_id=c.id AND d.status IN ('pending','leased','attempting','retry_wait'))
 FROM clients c WHERE c.id=$1`, client).Scan(&usage.Destinations.Used, &usage.Events.Used, &usage.OpenDeliveries.Used)
	if errors.Is(err, pgx.ErrNoRows) {
		return ClientUsage{}, ErrNotFound
	}
	if err != nil {
		return ClientUsage{}, err
	}
	return usage, nil
}

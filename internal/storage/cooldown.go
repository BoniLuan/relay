package storage

import (
	"context"
)

// DeferDestination pauses new claims for this destination for 60 seconds without
// consuming an HTTP attempt. Lock order matches StartAttempt: delivery, destination.
// A lost start-commit response cannot requeue started work through this method.
func (s *Store) DeferDestination(ctx context.Context, lease Lease) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err = lockDelivery(ctx, tx, lease.EventID); err != nil {
		return err
	}
	var client, destination string
	if err = tx.QueryRow(ctx, "SELECT client_id::text,destination_id::text FROM events WHERE id=$1", lease.EventID).Scan(&client, &destination); err != nil {
		return err
	}
	if err = lockDestination(ctx, tx, client, destination); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE deliveries SET status='pending',lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL
 WHERE event_id=$1 AND status='leased' AND lease_owner=$2 AND lease_token=$3 AND lease_expires_at>clock_timestamp()`, lease.EventID, lease.OwnerID, lease.token)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if _, err = tx.Exec(ctx, `UPDATE destinations SET delivery_paused_until=clock_timestamp()+interval '60 seconds'
 WHERE id=$1`, destination); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

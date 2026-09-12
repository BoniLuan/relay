package storage

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrNoDelivery   = errors.New("no delivery available")
	ErrLeaseLost    = errors.New("delivery lease is no longer owned")
	ErrInvalidLease = errors.New("invalid lease parameters")
)

const MaxLeaseDuration = 5 * time.Minute

var leaseOwnerPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Lease is a committed claim, not proof of completed delivery. The private token
// changes on every claim, including reacquisition by the same owner process.
type Lease struct {
	EventID       string    `json:"event_id"`
	OwnerID       string    `json:"owner_id"`
	ExpiresAt     time.Time `json:"expires_at"`
	token         string
	localDeadline time.Time
}

func (l Lease) Format(s fmt.State, verb rune) {
	fmt.Fprintf(s, "Lease{event_id:%s owner_id:%s expires_at:%s}", l.EventID, l.OwnerID, l.ExpiresAt.Format(time.RFC3339Nano))
}

// Remaining uses a conservative monotonic deadline, not the worker's wall clock.
// PostgreSQL is authoritative for lease ownership; this is only a local wait bound.
func (l Lease) Remaining() time.Duration { return max(0, time.Until(l.localDeadline)) }

// ClaimDelivery atomically selects and reserves one pending or expired delivery.
// SKIP LOCKED avoids waiting on another claimant's row. Commit must succeed before
// the caller receives the lease. An ambiguous commit is recovered by expiration.
func (s *Store) ClaimDelivery(ctx context.Context, owner string, duration time.Duration) (Lease, error) {
	if !leaseOwnerPattern.MatchString(owner) || duration < time.Millisecond || duration > MaxLeaseDuration || duration%time.Millisecond != 0 {
		return Lease{}, ErrInvalidLease
	}
	started := time.Now()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Lease{}, err
	}
	defer rollback(tx)
	lease := Lease{OwnerID: owner, token: NewID(), localDeadline: started.Add(duration)}
	err = tx.QueryRow(ctx, `WITH candidate AS (
  SELECT q.event_id FROM deliveries q
  JOIN events e ON e.id=q.event_id
  JOIN destinations dst ON dst.id=e.destination_id
  WHERE (dst.delivery_paused_until IS NULL OR dst.delivery_paused_until<=statement_timestamp())
   AND q.attempt_count<3 AND (q.status='pending'
   OR (q.status='retry_wait' AND q.next_attempt_at<=statement_timestamp())
   OR (q.status='leased' AND q.lease_expires_at<=statement_timestamp()))
  ORDER BY q.created_at,q.event_id
  LIMIT 1 FOR UPDATE OF q SKIP LOCKED
 )
 UPDATE deliveries AS d SET status='leased',next_attempt_at=NULL,lease_token=$1,lease_owner=$2,
  lease_expires_at=clock_timestamp()+($3::bigint * interval '1 millisecond')
 FROM candidate AS c WHERE d.event_id=c.event_id
 RETURNING d.event_id::text,d.lease_expires_at`, lease.token, owner, duration.Milliseconds()).Scan(&lease.EventID, &lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrNoDelivery
	}
	if err != nil {
		return Lease{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// ReleaseDelivery cannot change an expired or superseded reservation. Every
// future completion/retry update must use the same token-and-expiry condition.
func (s *Store) ReleaseDelivery(ctx context.Context, lease Lease) error {
	if lease.EventID == "" || lease.token == "" || lease.OwnerID == "" {
		return ErrLeaseLost
	}
	// Lock first, then evaluate expiry in a new statement. A predicate evaluated
	// before waiting for a row lock can otherwise become stale during that wait.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	var id string
	err = tx.QueryRow(ctx, "SELECT event_id::text FROM deliveries WHERE event_id=$1 FOR UPDATE", lease.EventID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE deliveries
 SET status='pending',lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL
 WHERE event_id=$1 AND status='leased' AND lease_token=$2 AND lease_owner=$3
 AND lease_expires_at>clock_timestamp()`, lease.EventID, lease.token, lease.OwnerID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return tx.Commit(ctx)
}

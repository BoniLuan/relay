package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrReplayState    = errors.New("delivery is not eligible for replay")
	ErrReplayConflict = errors.New("replay already requested with different input")
)

// ReplayReceipt is immutable audit metadata. It records authorization, not a
// send or a successful outcome. Request hashes and credentials stay private.
type ReplayReceipt struct {
	ID                   string    `json:"id"`
	EventID              string    `json:"event_id"`
	RequestedAt          time.Time `json:"requested_at"`
	PreviousAttemptCount int       `json:"previous_attempt_count"`
	MaxAttempts          int       `json:"max_attempts"`
}

// ReplayDelivery serializes with claim/start/finish/recovery on the delivery row.
// Only one replay record can ever exist per event. The original event, payload,
// ingestion idempotency and all attempt records remain unchanged.
func (s *Store) ReplayDelivery(ctx context.Context, client, event, key string, hash [32]byte) (ReplayReceipt, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReplayReceipt{}, false, err
	}
	defer rollback(tx)
	if err = lockQuotaClient(ctx, tx, client); err != nil {
		return ReplayReceipt{}, false, err
	}
	var state string
	var count, limit int
	err = tx.QueryRow(ctx, `SELECT d.status,d.attempt_count,d.attempt_limit FROM deliveries d
 JOIN events e ON e.id=d.event_id WHERE e.id=$1 AND e.client_id=$2 FOR UPDATE OF d`, event, client).Scan(&state, &count, &limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReplayReceipt{}, false, ErrNotFound
	}
	if err != nil {
		return ReplayReceipt{}, false, err
	}
	keyHash := sha256.Sum256([]byte(key))
	var receipt ReplayReceipt
	var storedKey, storedHash []byte
	err = tx.QueryRow(ctx, `SELECT id::text,event_id::text,requested_at,previous_attempt_count,attempt_limit,idempotency_hash,request_hash
 FROM delivery_replays WHERE event_id=$1`, event).Scan(&receipt.ID, &receipt.EventID, &receipt.RequestedAt, &receipt.PreviousAttemptCount, &receipt.MaxAttempts, &storedKey, &storedHash)
	if err == nil {
		if string(storedKey) != string(keyHash[:]) || string(storedHash) != string(hash[:]) {
			return ReplayReceipt{}, false, ErrReplayConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return ReplayReceipt{}, false, err
		}
		return receipt, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ReplayReceipt{}, false, err
	}
	// Unknown terminal results require a separate future policy. A successful or
	// live delivery cannot be forced back into the queue by this endpoint.
	if state != "failed" || count < 1 || count > maxAttempts || limit != maxAttempts {
		return ReplayReceipt{}, false, ErrReplayState
	}
	if err = checkOpenQuota(ctx, tx, client); err != nil {
		return ReplayReceipt{}, false, err
	}
	receipt = ReplayReceipt{ID: NewID(), EventID: event, PreviousAttemptCount: count, MaxAttempts: count + maxAttempts}
	err = tx.QueryRow(ctx, `INSERT INTO delivery_replays(id,event_id,requested_by,idempotency_hash,request_hash,previous_attempt_count,attempt_limit)
 VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING requested_at`, receipt.ID, event, client, keyHash[:], hash[:], count, receipt.MaxAttempts).Scan(&receipt.RequestedAt)
	if err != nil {
		return ReplayReceipt{}, false, err
	}
	_, err = tx.Exec(ctx, "UPDATE deliveries SET status='pending',attempt_limit=$2 WHERE event_id=$1", event, receipt.MaxAttempts)
	if err != nil {
		return ReplayReceipt{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ReplayReceipt{}, false, err
	}
	return receipt, false, nil
}

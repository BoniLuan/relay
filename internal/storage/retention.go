package storage

import (
	"context"
	"errors"
	"time"
)

const (
	HistoryRetentionDays = 30
	MaxRetentionBatch    = 100
)

// RetentionResult reports one bounded batch, not total outstanding work. No event
// IDs, payloads or idempotency keys are returned. Deleted is set only after commit.
type RetentionResult struct {
	Applied       bool      `json:"applied"`
	RetentionDays int       `json:"retention_days"`
	Cutoff        time.Time `json:"cutoff"`
	Limit         int       `json:"limit"`
	Selected      int       `json:"selected"`
	Deleted       int       `json:"deleted"`
}

// PruneHistory expires history and its ingestion receipt together. Admission and
// replay take the same client lock before delivery locks; workers never take the
// client lock. Only old terminal deliveries can be removed, and locked ones are
// skipped. Preview uses the same selection but rolls back without deleting.
func (s *Store) PruneHistory(ctx context.Context, client string, limit int, apply bool) (RetentionResult, error) {
	if limit < 1 || limit > MaxRetentionBatch {
		return RetentionResult{}, errors.New("invalid retention batch size")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RetentionResult{}, err
	}
	defer rollback(tx)
	if err = lockQuotaClient(ctx, tx, client); err != nil {
		return RetentionResult{}, err
	}
	result := RetentionResult{Applied: apply, RetentionDays: HistoryRetentionDays, Limit: limit}
	// Hours make the period exactly 30*24 hours, independent of session timezone/DST.
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()-($1::bigint*interval '1 hour')", HistoryRetentionDays*24).Scan(&result.Cutoff); err != nil {
		return RetentionResult{}, err
	}
	rows, err := tx.Query(ctx, `SELECT d.event_id::text FROM deliveries d JOIN events e ON e.id=d.event_id
 WHERE e.client_id=$1 AND d.status IN ('succeeded','failed','unknown') AND d.terminal_at<=$2
 ORDER BY d.terminal_at,d.event_id LIMIT $3 FOR UPDATE OF d SKIP LOCKED`, client, result.Cutoff, limit)
	if err != nil {
		return RetentionResult{}, err
	}
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return RetentionResult{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return RetentionResult{}, err
	}
	result.Selected = len(ids)
	if !apply {
		return result, nil
	}
	if len(ids) > 0 {
		// Explicit child-first deletion keeps the scope visible, without broadening
		// foreign-key cascades for other administrative operations.
		for _, query := range []string{
			"DELETE FROM delivery_replays WHERE event_id=ANY($1::uuid[])",
			"DELETE FROM delivery_attempts WHERE event_id=ANY($1::uuid[])",
			"DELETE FROM deliveries WHERE event_id=ANY($1::uuid[])",
			"DELETE FROM events WHERE id=ANY($1::uuid[])",
		} {
			if _, err = tx.Exec(ctx, query, ids); err != nil {
				return RetentionResult{}, err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return RetentionResult{}, err
	}
	result.Deleted = len(ids)
	return result, nil
}

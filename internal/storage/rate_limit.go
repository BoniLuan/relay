package storage

import (
	"context"
	"math"
	"time"
)

// ClientRequestsPerMinute is shared by every authenticated API route and token.
const ClientRequestsPerMinute = 120

// TakeRequest returns zero after committing admission, or a positive Retry-After
// in seconds. The database clock is sampled after locking so queued requests use
// the window in which they actually obtain admission. No HTTP work holds the lock.
func (s *Store) TakeRequest(ctx context.Context, client string) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	_, err = tx.Exec(ctx, `INSERT INTO client_request_limits(client_id,window_start,requests)
 VALUES($1,'epoch',0) ON CONFLICT (client_id) DO NOTHING`, client)
	if err != nil {
		return 0, err
	}
	var start time.Time
	var count int
	err = tx.QueryRow(ctx, `SELECT window_start,requests FROM client_request_limits WHERE client_id=$1 FOR UPDATE`, client).Scan(&start, &count)
	if err != nil {
		return 0, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return 0, err
	}
	window := now.UTC().Truncate(time.Minute)
	if !start.Equal(window) {
		count = 0
	}
	if count >= ClientRequestsPerMinute {
		return max(1, int(math.Ceil(window.Add(time.Minute).Sub(now).Seconds()))), nil
	}
	_, err = tx.Exec(ctx, `UPDATE client_request_limits SET window_start=$2,requests=$3 WHERE client_id=$1`, client, window, count+1)
	if err != nil {
		return 0, err
	}
	return 0, tx.Commit(ctx)
}

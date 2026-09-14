package storage

import (
	"math/rand/v2"
	"time"
)

// maxAttempts is the size of one automatic round. An explicit replay grants
// one more round, but never resets the global attempt counter.
const maxAttempts = 3

func (r AttemptResult) retryable() bool {
	switch r.ErrorCode {
	case "network", "response", "canceled":
		return true
	case "http_status":
		return r.StatusCode == 408 || r.StatusCode == 429 || r.StatusCode >= 500
	default:
		return false
	}
}

// Equal jitter: after attempts 1 and 2, wait [5s,10s) and [10s,20s).
// This randomness spreads load; it is not a security primitive. The chosen delay
// is persisted relative to PostgreSQL's clock, never recalculated during claims.
func retryDelay(attempt int) time.Duration {
	ceiling := 10 * time.Second << (min(max(attempt, 1), maxAttempts) - 1)
	half := ceiling / 2
	return half + time.Duration(rand.Int64N(int64(half)))
}

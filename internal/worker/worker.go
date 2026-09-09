// Package worker exercises committed leases only. It deliberately has no sender,
// credential lookup or delivery-completion behavior in this milestone.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/BoniLuan/relay/internal/storage"
)

type Queue interface {
	ClaimDelivery(context.Context, string, time.Duration) (storage.Lease, error)
	ReleaseDelivery(context.Context, storage.Lease) error
}

// Run claims at most one delivery, waits until its local lease budget ends, and
// exits without sending. Graceful cancellation releases an unexpired lease;
// abrupt process death leaves recovery to database expiration and a later claim.
func Run(ctx context.Context, queue Queue, logger *slog.Logger, duration time.Duration) error {
	if duration < time.Millisecond || duration > storage.MaxLeaseDuration || duration%time.Millisecond != 0 {
		return storage.ErrInvalidLease
	}
	if ctx.Err() != nil {
		return nil
	}
	owner := storage.NewID()
	logger.Info("lease-only worker starting", "worker_id", owner, "outbound_enabled", false)
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	lease, err := queue.ClaimDelivery(claimCtx, owner, duration)
	cancel()
	if errors.Is(err, storage.ErrNoDelivery) {
		logger.Info("no delivery available", "worker_id", owner)
		return nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("delivery claim failed; any uncertain claim expires automatically")
	}
	logger.Info("delivery lease acquired", "worker_id", owner, "event_id", lease.EventID, "expires_at", lease.ExpiresAt)
	timer := time.NewTimer(lease.Remaining())
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	if ctx.Err() != nil {
		// Signal cancellation must not cancel cleanup itself. Cleanup is bounded.
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer releaseCancel()
		err = queue.ReleaseDelivery(releaseCtx, lease)
		if errors.Is(err, storage.ErrLeaseLost) {
			logger.Info("lease already expired or reassigned", "event_id", lease.EventID)
			return nil
		}
		if err != nil {
			return errors.New("delivery lease release failed; recovery uses expiration")
		}
		logger.Info("delivery lease released", "event_id", lease.EventID)
	} else {
		// Never label a lease probe as delivered. Remaining() may be conservative;
		// PostgreSQL, not this timer, determines when another worker can reclaim.
		logger.Info("lease observation ended; no delivery performed", "event_id", lease.EventID)
	}
	return nil
}

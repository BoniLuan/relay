package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/BoniLuan/relay/internal/storage"
)

func validateSendDuration(d time.Duration) error {
	if d < 15*time.Second || d > storage.MaxLeaseDuration || d%time.Millisecond != 0 {
		return errors.New("sending requires a whole-millisecond lease from 15s to 5m")
	}
	return nil
}

// RunContinuous is deliberately sequential: one bounded cycle, then a cancellable
// wait. Even a busy queue cannot spin; scale with independent processes when needed.
func RunContinuous(ctx context.Context, q AttemptQueue, sender Sender, logger *slog.Logger, lease, poll time.Duration) error {
	if err := validateSendDuration(lease); err != nil {
		return err
	}
	if poll < time.Second || poll > 30*time.Second || poll%time.Millisecond != 0 {
		return errors.New("poll interval must be whole milliseconds from 1s to 30s")
	}
	logger.Info("continuous worker starting", "poll_interval", poll, "lease_duration", lease)
	defer logger.Info("continuous worker stopped")
	runLoop(ctx, func(ctx context.Context) error { return RunOnce(ctx, q, sender, logger, lease) }, waitFor, logger, poll)
	return nil
}

func runLoop(ctx context.Context, cycle func(context.Context) error, wait func(context.Context, time.Duration) bool, logger *slog.Logger, poll time.Duration) {
	delay := poll
	for ctx.Err() == nil {
		err := cycle(ctx)
		if err != nil {
			delay = min(30*time.Second, delay*2)
			// Do not forward arbitrary dependency errors, URLs or database parameters.
			logger.Warn("worker cycle failed; backing off", "retry_delay", delay)
		} else {
			delay = poll
		}
		if ctx.Err() != nil || !wait(ctx, delay) {
			return
		}
	}
}
func waitFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

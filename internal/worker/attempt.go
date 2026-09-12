package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/BoniLuan/relay/internal/delivery"
	"github.com/BoniLuan/relay/internal/storage"
)

type AttemptQueue interface {
	Queue
	StartAttempt(context.Context, storage.Lease) (storage.AttemptWork, error)
	FinishAttempt(context.Context, storage.Lease, string, storage.AttemptResult) error
	RecoverAttempt(context.Context) (bool, error)
}
type Sender interface {
	Send(context.Context, string, string, []byte, delivery.Secret) (delivery.Outcome, error)
}

// RunOnce recovers one interrupted attempt OR sends at most one event. There is
// no polling/retry loop. The production CLI supplies the policy-enforcing sender.
func RunOnce(ctx context.Context, q AttemptQueue, sender Sender, logger *slog.Logger, duration time.Duration) error {
	if duration < 15*time.Second || duration > storage.MaxLeaseDuration || duration%time.Millisecond != 0 {
		return errors.New("sending requires a whole-millisecond lease from 15s to 5m")
	}
	if ctx.Err() != nil {
		return nil
	}
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	recovered, err := q.RecoverAttempt(opCtx)
	cancel()
	if err != nil {
		return errors.New("attempt recovery failed")
	}
	if recovered {
		logger.Info("expired attempt marked unknown; no resend performed")
		return nil
	}
	opCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
	lease, err := q.ClaimDelivery(opCtx, storage.NewID(), duration)
	cancel()
	if errors.Is(err, storage.ErrNoDelivery) {
		logger.Info("no delivery available")
		return nil
	}
	if err != nil {
		return errors.New("delivery claim failed; uncertain claims expire")
	}
	opCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
	work, err := q.StartAttempt(opCtx, lease)
	cancel()
	if err != nil {
		// A lost commit response might conceal a started attempt. Release only accepts
		// 'leased', so it cannot requeue that uncertain outbound work.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		releaseErr := q.ReleaseDelivery(cleanupCtx, lease)
		cleanupCancel()
		if releaseErr != nil && !errors.Is(releaseErr, storage.ErrLeaseLost) {
			logger.Error("preparation cleanup failed; reservation expires")
		}
		if errors.Is(err, storage.ErrSigningUnavailable) {
			return errors.New("no active signing secret; no HTTP performed")
		}
		return errors.New("attempt preparation failed; no HTTP performed; inspect attempt state")
	}
	logger.Info("delivery attempt started", "event_id", work.EventID, "attempt_id", work.ID, "signing_version", work.SigningVersion)
	outcome := delivery.Outcome{}
	var sendErr error
	// Conservatively require the complete send/finalize budget AFTER start commit.
	if ctx.Err() != nil || lease.Remaining() < 8*time.Second {
		sendErr = context.Canceled
	} else {
		sendCtx, sendCancel := context.WithTimeout(ctx, 5*time.Second)
		outcome, sendErr = sender.Send(sendCtx, work.URL, work.EventID, work.Payload, work.Secret)
		sendCancel()
	}
	result := classify(outcome, sendErr)
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 3*time.Second)
	err = q.FinishAttempt(finishCtx, lease, work.ID, result)
	finishCancel()
	if err != nil {
		return errors.New("attempt result not confirmed; do not assume delivery or resend; recovery uses unknown")
	}
	logger.Info("delivery attempt recorded", "event_id", work.EventID, "attempt_id", work.ID, "http_status", result.StatusCode, "error_code", result.ErrorCode)
	return nil
}
func classify(outcome delivery.Outcome, err error) storage.AttemptResult {
	r := storage.AttemptResult{StatusCode: outcome.StatusCode}
	// Go can parse non-standard three-digit statuses. Keep receiver-controlled
	// values outside our history domain from turning into a storage failure.
	if r.StatusCode != 0 && (r.StatusCode < 100 || r.StatusCode > 599) {
		return storage.AttemptResult{ErrorCode: "response"}
	}
	switch {
	case err == nil:
		if outcome.StatusCode < 200 || outcome.StatusCode > 299 {
			r.ErrorCode = "http_status"
		}
	case errors.Is(err, context.Canceled):
		r.ErrorCode = "canceled"
	case errors.Is(err, delivery.ErrDestination):
		r.ErrorCode = "destination"
	case errors.Is(err, delivery.ErrResponse):
		r.ErrorCode = "response"
	case errors.Is(err, delivery.ErrInput):
		r.ErrorCode = "input"
	default:
		r.ErrorCode = "network"
	}
	return r
}

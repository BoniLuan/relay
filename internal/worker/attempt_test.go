package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/BoniLuan/relay/internal/delivery"
	"github.com/BoniLuan/relay/internal/storage"
)

type attemptQueueFake struct {
	fakeQueue
	start            func(context.Context, storage.Lease) (storage.AttemptWork, error)
	finish           func(context.Context, storage.Lease, string, storage.AttemptResult) error
	recover          func(context.Context) (bool, error)
	deferDestination func(context.Context, storage.Lease) error
}

func (q attemptQueueFake) StartAttempt(ctx context.Context, l storage.Lease) (storage.AttemptWork, error) {
	return q.start(ctx, l)
}
func (q attemptQueueFake) FinishAttempt(ctx context.Context, l storage.Lease, id string, r storage.AttemptResult) error {
	return q.finish(ctx, l, id, r)
}
func (q attemptQueueFake) RecoverAttempt(ctx context.Context) (bool, error) { return q.recover(ctx) }

func (q attemptQueueFake) DeferDestination(ctx context.Context, l storage.Lease) error {
	return q.deferDestination(ctx, l)
}

type forbiddenSender struct{ t *testing.T }

func (s forbiddenSender) Send(context.Context, string, string, []byte, delivery.Secret) (delivery.Outcome, error) {
	s.t.Fatal("unexpected HTTP send")
	return delivery.Outcome{}, nil
}
func TestCanceledAfterStartFinalizesWithoutSendingOrReleasing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := false
	q := attemptQueueFake{
		fakeQueue: fakeQueue{claim: func(context.Context, string, time.Duration) (storage.Lease, error) {
			return storage.Lease{EventID: "event"}, nil
		}, release: func(context.Context, storage.Lease) error { t.Fatal("released started work"); return nil }},
		recover: func(context.Context) (bool, error) { return false, nil },
		start: func(context.Context, storage.Lease) (storage.AttemptWork, error) {
			cancel()
			return storage.AttemptWork{ID: "attempt"}, nil
		},
		finish: func(ctx context.Context, l storage.Lease, id string, r storage.AttemptResult) error {
			finished = true
			deadline, ok := ctx.Deadline()
			if ctx.Err() != nil || !ok || time.Until(deadline) > 3*time.Second || id != "attempt" || r.ErrorCode != "canceled" {
				t.Fatal("unsafe shutdown finalization")
			}
			return nil
		},
	}
	if err := RunOnce(ctx, q, forbiddenSender{t}, slog.New(slog.NewTextHandler(io.Discard, nil)), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if !finished {
		t.Fatal("started attempt abandoned on cancellation")
	}
}
func TestUnconfirmedStartNeverSends(t *testing.T) {
	released := false
	q := attemptQueueFake{
		fakeQueue: fakeQueue{claim: func(context.Context, string, time.Duration) (storage.Lease, error) { return storage.Lease{}, nil }, release: func(context.Context, storage.Lease) error { released = true; return storage.ErrLeaseLost }},
		recover:   func(context.Context) (bool, error) { return false, nil },
		start: func(context.Context, storage.Lease) (storage.AttemptWork, error) {
			return storage.AttemptWork{}, errors.New("private commit detail")
		},
		finish: func(context.Context, storage.Lease, string, storage.AttemptResult) error {
			t.Fatal("finished unconfirmed start")
			return nil
		},
	}
	err := RunOnce(context.Background(), q, forbiddenSender{t}, slog.New(slog.NewTextHandler(io.Discard, nil)), 30*time.Second)
	if err == nil || strings.Contains(err.Error(), "private") || !released {
		t.Fatal("unsafe preparation failure")
	}
}
func TestRecoveryDoesNotAlsoClaimOrSend(t *testing.T) {
	q := attemptQueueFake{recover: func(context.Context) (bool, error) { return true, nil }}
	if err := RunOnce(context.Background(), q, forbiddenSender{t}, slog.New(slog.NewTextHandler(io.Discard, nil)), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{0, 14 * time.Second, 15*time.Second + time.Microsecond, 6 * time.Minute} {
		if err := RunOnce(context.Background(), attemptQueueFake{}, forbiddenSender{t}, slog.Default(), d); err == nil {
			t.Fatal("invalid send duration accepted")
		}
	}
}

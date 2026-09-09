package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/BoniLuan/relay/internal/storage"
)

type fakeQueue struct {
	claim   func(context.Context, string, time.Duration) (storage.Lease, error)
	release func(context.Context, storage.Lease) error
}

func (f fakeQueue) ClaimDelivery(ctx context.Context, id string, d time.Duration) (storage.Lease, error) {
	return f.claim(ctx, id, d)
}
func (f fakeQueue) ReleaseDelivery(ctx context.Context, l storage.Lease) error {
	return f.release(ctx, l)
}
func TestGracefulCancellationReleasesWithFreshContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := false
	var logs bytes.Buffer
	q := fakeQueue{
		claim: func(c context.Context, owner string, d time.Duration) (storage.Lease, error) {
			if _, ok := c.Deadline(); !ok || owner == "" {
				t.Fatal("claim not bounded or identified")
			}
			cancel()
			return storage.Lease{EventID: "event"}, nil
		},
		release: func(c context.Context, l storage.Lease) error {
			released = true
			if c.Err() != nil {
				t.Fatal("cleanup inherited canceled context")
			}
			deadline, ok := c.Deadline()
			if !ok || time.Until(deadline) > 3*time.Second {
				t.Fatal("cleanup not bounded")
			}
			return nil
		},
	}
	if err := Run(ctx, q, slog.New(slog.NewJSONHandler(&logs, nil)), time.Second); err != nil {
		t.Fatal(err)
	}
	if !released || !strings.Contains(logs.String(), "delivery lease released") {
		t.Fatal("lease was not released on shutdown")
	}
}
func TestWorkerDoesNotClaimAgainOrInventCompletion(t *testing.T) {
	for _, claimErr := range []error{nil, storage.ErrNoDelivery, errors.New("password=private database detail")} {
		calls := 0
		var logs bytes.Buffer
		q := fakeQueue{claim: func(context.Context, string, time.Duration) (storage.Lease, error) {
			calls++
			return storage.Lease{}, claimErr
		}, release: func(context.Context, storage.Lease) error { t.Fatal("unexpected release"); return nil }}
		err := Run(context.Background(), q, slog.New(slog.NewJSONHandler(&logs, nil)), time.Second)
		if calls != 1 {
			t.Fatal("worker claimed repeatedly")
		}
		if claimErr != nil && !errors.Is(claimErr, storage.ErrNoDelivery) && err == nil {
			t.Fatal("claim failure hidden")
		}
		combined := logs.String()
		if err != nil {
			combined += err.Error()
		}
		if strings.Contains(combined, "private") || strings.Contains(combined, "delivered") {
			t.Fatal("worker leaked error or invented delivery")
		}
	}
}
func TestLostLeaseDuringShutdownIsExpected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	q := fakeQueue{claim: func(context.Context, string, time.Duration) (storage.Lease, error) {
		cancel()
		return storage.Lease{}, nil
	}, release: func(context.Context, storage.Lease) error { return storage.ErrLeaseLost }}
	if err := Run(ctx, q, slog.New(slog.NewJSONHandler(&logs, nil)), time.Second); err != nil {
		t.Fatal(err)
	}
}
func TestCanceledWorkerNeverClaims(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, fakeQueue{}, slog.Default(), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), fakeQueue{}, slog.Default(), 0); !errors.Is(err, storage.ErrInvalidLease) {
		t.Fatal("invalid duration accepted")
	}
}

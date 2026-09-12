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

func TestLoopBackoffIsBoundedAndResets(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	calls := 0
	var waits []time.Duration
	runLoop(context.Background(), func(context.Context) error {
		calls++
		if calls <= 6 {
			return errors.New("private credential")
		}
		return nil
	},
		func(ctx context.Context, d time.Duration) bool { waits = append(waits, d); return len(waits) < 7 }, logger, time.Second)
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, time.Second}
	if calls != 7 || len(waits) != len(want) {
		t.Fatal("unexpected cycle count")
	}
	for i, d := range waits {
		if d != want[i] {
			t.Fatalf("wait %d=%v", i, d)
		}
	}
	if strings.Contains(logs.String(), "private") {
		t.Fatal("dependency error leaked")
	}
}
func TestLoopAlwaysWaitsAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cycles, waits := 0, 0
	runLoop(ctx, func(context.Context) error { cycles++; return nil }, func(c context.Context, d time.Duration) bool {
		waits++
		if d != time.Second {
			t.Fatal("unexpected poll interval")
		}
		cancel()
		return true
	}, slog.Default(), time.Second)
	if cycles != 1 || waits != 1 {
		t.Fatal("loop spun or continued after cancellation")
	}
	started := time.Now()
	if waitFor(ctx, 30*time.Second) {
		t.Fatal("canceled wait completed normally")
	}
	if time.Since(started) > time.Second {
		t.Fatal("shutdown waited for poll timer")
	}
}
func TestContinuousRejectsInvalidConfiguration(t *testing.T) {
	for _, poll := range []time.Duration{0, time.Millisecond, 31 * time.Second, time.Second + time.Nanosecond} {
		if err := RunContinuous(context.Background(), attemptQueueFake{}, forbiddenSender{t}, slog.Default(), 30*time.Second, poll); err == nil {
			t.Fatal("invalid polling accepted")
		}
	}
	if err := RunContinuous(context.Background(), attemptQueueFake{}, forbiddenSender{t}, slog.Default(), time.Second, time.Second); err == nil {
		t.Fatal("invalid lease accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunContinuous(ctx, attemptQueueFake{}, forbiddenSender{t}, slog.Default(), 30*time.Second, time.Second); err != nil {
		t.Fatal(err)
	}
}
func TestMissingKeyDefersWithFreshContextAndNoSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deferred := false
	q := attemptQueueFake{
		fakeQueue: fakeQueue{claim: func(context.Context, string, time.Duration) (storage.Lease, error) { return storage.Lease{}, nil }, release: func(context.Context, storage.Lease) error {
			t.Fatal("released without destination cooldown")
			return nil
		}},
		recover: func(context.Context) (bool, error) { return false, nil },
		start: func(context.Context, storage.Lease) (storage.AttemptWork, error) {
			cancel()
			return storage.AttemptWork{}, storage.ErrSigningUnavailable
		},
		deferDestination: func(ctx context.Context, l storage.Lease) error {
			deferred = true
			deadline, ok := ctx.Deadline()
			if ctx.Err() != nil || !ok || time.Until(deadline) > 3*time.Second {
				t.Fatal("unbounded or canceled deferral")
			}
			return nil
		},
	}
	if err := RunOnce(ctx, q, forbiddenSender{t}, slog.Default(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if !deferred {
		t.Fatal("missing key did not pause destination")
	}
}

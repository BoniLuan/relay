package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/BoniLuan/relay/internal/storage"
)

type fakeMetrics struct {
	err   error
	calls int
}

func (f *fakeMetrics) OperationalSnapshot(ctx context.Context) (storage.OperationalMetrics, error) {
	f.calls++
	if _, ok := ctx.Deadline(); !ok {
		return storage.OperationalMetrics{}, errors.New("missing deadline")
	}
	return storage.OperationalMetrics{Deliveries: [7]int64{3}, Attempts: [4]int64{0, 2}, OpenOldestAgeSeconds: 12.5, ExpiredLeases: 1}, f.err
}
func TestMetricsExpositionAndFailures(t *testing.T) {
	var out bytes.Buffer
	db := &fakeMetrics{}
	if err := runMetrics(context.Background(), db, nil, &out); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`relay_deliveries{state="pending"} 3`, `relay_deliveries{state="unknown"} 0`, `relay_delivery_attempts{state="succeeded"} 2`, `relay_open_delivery_oldest_age_seconds 12.5`, `relay_expired_delivery_leases 1`} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Fatalf("missing series %s", line)
		}
	}
	count := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "relay_") {
			count++
		}
	}
	if count != 13 {
		t.Fatalf("unexpected cardinality %d", count)
	}
	out.Reset()
	db.err = errors.New("private credential detail")
	if err := runMetrics(context.Background(), db, nil, &out); err == nil || strings.Contains(err.Error(), "private") || out.Len() != 0 {
		t.Fatal("failed collection leaked or emitted snapshot")
	}
	db.calls = 0
	if err := runMetrics(context.Background(), db, []string{"extra"}, &out); err == nil || db.calls != 0 {
		t.Fatal("invalid arguments reached database")
	}
}

func TestMetricsStartupFailureUsesStderr(t *testing.T) {
	if os.Getenv("RELAY_METRICS_TEST_CHILD") == "1" {
		os.Args = []string{"relay", "metrics"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMetricsStartupFailureUsesStderr$")
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "RELAY_DATABASE_URL=") && !strings.HasPrefix(value, "RELAY_METRICS_TEST_CHILD=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "RELAY_METRICS_TEST_CHILD=1", "RELAY_DATABASE_URL=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("startup failure exited successfully")
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "RELAY_DATABASE_URL is required") {
		t.Fatal("startup failure contaminated metric output or lost diagnostic")
	}
}

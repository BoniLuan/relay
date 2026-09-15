package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/BoniLuan/relay/internal/storage"
)

type metricsReader interface {
	OperationalSnapshot(context.Context) (storage.OperationalMetrics, error)
}

// Build the complete exposition before writing, so a database failure cannot
// masquerade as a zero-valued or partial successful snapshot.
func runMetrics(ctx context.Context, db metricsReader, args []string, out io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: relay metrics")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m, err := db.OperationalSnapshot(ctx)
	if err != nil {
		return errors.New("operational metrics unavailable")
	}
	var b bytes.Buffer
	fmt.Fprintln(&b, "# HELP relay_deliveries Retained deliveries by current state.")
	fmt.Fprintln(&b, "# TYPE relay_deliveries gauge")
	for i, state := range []string{"pending", "leased", "attempting", "retry_wait", "succeeded", "failed", "unknown"} {
		fmt.Fprintf(&b, "relay_deliveries{state=%q} %d\n", state, m.Deliveries[i])
	}
	fmt.Fprintln(&b, "# HELP relay_delivery_attempts Retained delivery attempts by current state.")
	fmt.Fprintln(&b, "# TYPE relay_delivery_attempts gauge")
	for i, state := range []string{"started", "succeeded", "failed", "unknown"} {
		fmt.Fprintf(&b, "relay_delivery_attempts{state=%q} %d\n", state, m.Attempts[i])
	}
	fmt.Fprintln(&b, "# HELP relay_open_delivery_oldest_age_seconds Age since creation of the oldest open delivery, zero when empty.")
	fmt.Fprintln(&b, "# TYPE relay_open_delivery_oldest_age_seconds gauge")
	fmt.Fprintf(&b, "relay_open_delivery_oldest_age_seconds %g\n", m.OpenOldestAgeSeconds)
	fmt.Fprintln(&b, "# HELP relay_expired_delivery_leases Open leases whose expiry has passed, awaiting worker recovery.")
	fmt.Fprintln(&b, "# TYPE relay_expired_delivery_leases gauge")
	fmt.Fprintf(&b, "relay_expired_delivery_leases %d\n", m.ExpiredLeases)
	if _, err = out.Write(b.Bytes()); err != nil {
		return errors.New("operational metrics output failed")
	}
	return nil
}

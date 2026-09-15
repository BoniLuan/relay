package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BoniLuan/relay/internal/storage"
)

type blockingMetrics struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingMetrics) OperationalSnapshot(ctx context.Context) (storage.OperationalMetrics, error) {
	b.calls.Add(1)
	close(b.started)
	select {
	case <-b.release:
		return storage.OperationalMetrics{}, nil
	case <-ctx.Done():
		return storage.OperationalMetrics{}, ctx.Err()
	}
}
func TestExporterLimitsConcurrentCollections(t *testing.T) {
	db := &blockingMetrics{started: make(chan struct{}), release: make(chan struct{})}
	h := newMetricsHandler(db)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
	}()
	<-db.started
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 503 || db.calls.Load() != 1 {
		t.Fatal("overlapping database collections")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/livez", nil))
	if w.Code != 200 {
		t.Fatal("liveness blocked on collection")
	}
	close(db.release)
	<-done
}
func TestExporterContractAndRecovery(t *testing.T) {
	db := &fakeMetrics{}
	h := newMetricsHandler(db)
	for _, failed := range []bool{false, true, false} {
		db.err = nil
		if failed {
			db.err = errors.New("private dependency value")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
		if failed {
			if w.Code != 503 || strings.Contains(w.Body.String(), "relay_deliveries") || strings.Contains(w.Body.String(), "private") {
				t.Fatal("failed scrape leaked or returned stale data")
			}
		} else {
			if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "version=0.0.4") || !strings.Contains(w.Body.String(), `relay_deliveries{state="pending"} 3`) {
				t.Fatal("invalid scrape")
			}
		}
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/api/v1/usage", 404}, {"POST", "/metrics", 405}, {"GET", "/", 404}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatal("exporter exposed unrelated route")
		}
	}
}

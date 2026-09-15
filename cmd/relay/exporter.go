package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// The exporter runs in its own process/network boundary, never on the client API.
// One in-flight collection bounds database load. Failed scrapes never reuse data.
func newMetricsHandler(db metricsReader) http.Handler {
	gate := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		default:
			http.Error(w, "collection busy", 503)
			return
		}
		var data bytes.Buffer
		if err := runMetrics(r.Context(), db, nil, &data); err != nil {
			http.Error(w, "metrics unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Write(data.Bytes())
	})
	return mux
}

func runMetricsServer(ctx context.Context, db metricsReader, addr string, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.New("cannot bind metrics listener")
	}
	server := &http.Server{Handler: newMetricsHandler(db), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 7 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.Info("private metrics exporter started")
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("metrics server stopped unexpectedly")
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("metrics shutdown incomplete")
		}
		return nil
	}
}

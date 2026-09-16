// relay-demo is an optional receiver for synthetic portfolio integration tests.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BoniLuan/relay/internal/delivery"
)

type receipt struct {
	Hash       string    `json:"sha256"`
	ReceivedAt time.Time `json:"received_at"`
}
type receiver struct {
	mu       sync.Mutex
	secret   delivery.Secret
	file     string
	receipts map[string]receipt
}

var eventID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var receiptHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

const maxReceiverState = 256 << 10

func openReceiver(secret delivery.Secret, file string) (*receiver, error) {
	r := &receiver{secret: secret, file: file, receipts: make(map[string]receipt)}
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxReceiverState {
		return nil, errors.New("invalid receiver state")
	}
	decoder := json.NewDecoder(io.LimitReader(f, maxReceiverState+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&r.receipts); err != nil || r.receipts == nil || len(r.receipts) > 1000 {
		return nil, errors.New("invalid receiver state")
	}
	if err = decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid receiver state")
	}
	for id, stored := range r.receipts {
		if !eventID.MatchString(id) || !receiptHash.MatchString(stored.Hash) || stored.ReceivedAt.IsZero() {
			return nil, errors.New("invalid receiver state")
		}
	}
	return r, nil
}
func (r *receiver) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if q.URL.Path == "/livez" && (q.Method == http.MethodGet || q.Method == http.MethodHead) {
		w.WriteHeader(200)
		return
	}
	if q.URL.Path != "/demo/hook" {
		http.NotFound(w, q)
		return
	}
	if q.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, q.Body, 64<<10))
	if err != nil {
		w.WriteHeader(413)
		return
	}
	id := q.Header.Get("Relay-Event-ID")
	if !eventID.MatchString(id) || len(q.Header.Values("Relay-Signature")) != 1 || !delivery.Verify(r.secret, q.Header.Get("Relay-Signature"), id, body, time.Now()) {
		w.WriteHeader(401)
		return
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.receipts[id]; ok {
		if old.Hash != hash {
			w.WriteHeader(409)
			return
		}
		w.WriteHeader(204)
		return
	}
	if len(r.receipts) >= 1000 {
		w.WriteHeader(507)
		return
	}
	next := make(map[string]receipt, len(r.receipts)+1)
	for k, v := range r.receipts {
		next[k] = v
	}
	next[id] = receipt{Hash: hash, ReceivedAt: time.Now().UTC()}
	if err = r.persist(next); err != nil {
		w.WriteHeader(503)
		return
	}
	r.receipts = next
	w.WriteHeader(204)
}
func (r *receiver) persist(next map[string]receipt) error {
	f, err := os.CreateTemp(filepath.Dir(r.file), ".receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = json.NewEncoder(f).Encode(next); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), r.file); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(r.file))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func run() error {
	path := os.Getenv("RELAY_DEMO_SECRET_FILE")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 128 {
		return errors.New("invalid secret file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	secret, err := delivery.ParseSecret(strings.TrimSpace(string(raw)))
	if err != nil {
		return err
	}
	r, err := openReceiver(secret, "/data/receipts.json")
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: ":8080", Handler: r, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, ErrorLog: log.New(io.Discard, "", 0)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(c)
	}
}
func main() {
	if run() != nil {
		log.Print("demo receiver stopped; check private configuration and storage")
		os.Exit(1)
	}
}

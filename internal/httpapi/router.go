// Package httpapi validates requests and maps persistence outcomes to HTTP.
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BoniLuan/relay/internal/storage"
)

// Backend is the small persistence boundary exercised by HTTP tests.
type Backend interface {
	Ready(context.Context) error
	Authenticate(context.Context, string) (string, error)
	CreateDestination(context.Context, string, string) (storage.Destination, error)
	Ingest(context.Context, string, string, string, [32]byte, []byte) (storage.Event, bool, error)
	GetEvent(context.Context, string, string) (storage.Event, error)
}

type api struct {
	db     Backend
	logger *slog.Logger
}

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

const maxBody = 64 << 10

func NewHandler(db Backend, logger *slog.Logger) http.Handler {
	a := api{db: db, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]string{"service": "relay", "stage": "durable-ingestion", "message": "Events are stored; outbound delivery is not implemented."})
	})
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := db.Ready(ctx); err != nil {
			problem(w, 503, "not ready")
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /api/v1/destinations", a.auth(a.destination))
	mux.HandleFunc("POST /api/v1/events", a.auth(a.ingest))
	mux.HandleFunc("GET /api/v1/events/{id}", a.auth(a.event))
	return mux
}
func (a api) auth(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("Authorization")
		var parts []string
		if len(values) == 1 {
			parts = strings.Fields(values[0])
		}
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) != 70 || !strings.HasPrefix(parts[1], "relay_") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			problem(w, 401, "unauthorized")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		client, err := a.db.Authenticate(ctx, parts[1])
		if errors.Is(err, storage.ErrUnauthorized) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			problem(w, 401, "unauthorized")
			return
		}
		if err != nil {
			a.failure(w, r, err)
			return
		}
		next(w, r, client)
	}
}
func readJSON(w http.ResponseWriter, r *http.Request, target any) ([]byte, error) {
	if strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/json" {
		return nil, errors.New("content type")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid UTF-8")
	}
	// Limit nesting before handing JSON to PostgreSQL's recursive input parser.
	depth := 0
	tokens := json.NewDecoder(strings.NewReader(string(raw)))
	tokens.UseNumber()
	for {
		token, err := tokens.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
				if depth > 64 {
					return nil, errors.New("JSON nesting exceeds 64 levels")
				}
			case '}', ']':
				depth--
			}
		}
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		return nil, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("expected one JSON object")
	}
	return raw, nil
}
func invalidBody(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		problem(w, 413, "body exceeds 64 KiB")
		return
	}
	problem(w, 400, "expected a valid application/json request")
}
func (a api) destination(w http.ResponseWriter, r *http.Request, client string) {
	var input struct {
		URL string `json:"url"`
	}
	if _, err := readJSON(w, r, &input); err != nil {
		invalidBody(w, err)
		return
	}
	u, err := url.Parse(input.URL)
	if err != nil || len(input.URL) > 2048 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		problem(w, 400, "url must be an absolute HTTPS URL without credentials or fragment")
		return
	}
	d, err := a.db.CreateDestination(r.Context(), client, input.URL)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	respond(w, 201, d)
}
func (a api) ingest(w http.ResponseWriter, r *http.Request, client string) {
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || !keyPattern.MatchString(keys[0]) {
		problem(w, 400, "Idempotency-Key must be 1-128 letters, digits, dots, underscores, colons or hyphens")
		return
	}
	var input struct {
		DestinationID string          `json:"destination_id"`
		Payload       json.RawMessage `json:"payload"`
	}
	raw, err := readJSON(w, r, &input)
	if err != nil {
		invalidBody(w, err)
		return
	}
	if !uuid.MatchString(input.DestinationID) || len(input.Payload) == 0 {
		problem(w, 400, "destination_id must be a lowercase UUID and payload is required")
		return
	}
	e, duplicate, err := a.db.Ingest(r.Context(), client, input.DestinationID, keys[0], sha256.Sum256(raw), input.Payload)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/events/"+e.ID)
	status := http.StatusCreated
	if duplicate {
		status = http.StatusOK
		w.Header().Set("Idempotency-Replayed", "true")
	}
	respond(w, status, e)
}
func (a api) event(w http.ResponseWriter, r *http.Request, client string) {
	if !uuid.MatchString(r.PathValue("id")) {
		problem(w, 404, "not found")
		return
	}
	e, err := a.db.GetEvent(r.Context(), client, r.PathValue("id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	respond(w, 200, e)
}
func (a api) failure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrInvalidPayload):
		problem(w, 400, "payload must be representable as PostgreSQL JSONB")
	case errors.Is(err, storage.ErrNotFound):
		problem(w, 404, "not found")
	case errors.Is(err, storage.ErrConflict):
		problem(w, 409, "idempotency key reused with a different request")
	default:
		// Database errors may contain payloads, SQL parameters or credentials.
		a.logger.Error("database operation failed", "method", r.Method, "operation", r.Pattern)
		problem(w, 503, "storage unavailable; retry with the same idempotency key and body")
	}
}
func problem(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// Package storage owns PostgreSQL persistence and ingestion transactions.
package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/BoniLuan/relay/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidPayload = errors.New("payload is not representable as JSONB")
	ErrUnauthorized   = errors.New("unauthorized")
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("idempotency key reused with a different request")
)

type Store struct {
	pool    *pgxpool.Pool
	keyring *secrets.Keyring
}

func Open(ctx context.Context, url string) (*Store, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	config.MaxConns = 5
	config.ConnConfig.ConnectTimeout = 3 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("cannot create database pool")
	}
	return &Store{pool: pool}, nil
}
func (s *Store) Close() { s.pool.Close() }
func (s *Store) Ready(ctx context.Context) error {
	var version int
	if err := s.pool.QueryRow(ctx, "SELECT version FROM schema_migrations WHERE version = $1", schemaVersion).Scan(&version); err != nil {
		return err
	}
	if s.keyring != nil {
		return s.CheckKeyring(ctx)
	}
	return nil
}

// NewID uses a random UUID, avoiding a separate ID library or database extension.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type Destination struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) CreateDestination(ctx context.Context, client, url string) (Destination, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Destination{}, err
	}
	defer rollback(tx)
	if err = lockQuotaClient(ctx, tx, client); err != nil {
		return Destination{}, err
	}
	var full bool
	err = tx.QueryRow(ctx, `SELECT count(*) >= $2 FROM (SELECT 1 FROM destinations WHERE client_id=$1 LIMIT $2) owned`, client, MaxClientDestinations).Scan(&full)
	if err != nil {
		return Destination{}, err
	}
	if full {
		return Destination{}, &QuotaError{Resource: "destinations", Limit: MaxClientDestinations}
	}
	d := Destination{ID: NewID(), URL: url}
	err = tx.QueryRow(ctx, "INSERT INTO destinations (id,client_id,url) VALUES ($1,$2,$3) RETURNING created_at", d.ID, client, url).Scan(&d.CreatedAt)
	if err != nil {
		return Destination{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Destination{}, err
	}
	return d, nil
}

type Event struct {
	ID            string    `json:"id"`
	DestinationID string    `json:"destination_id"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// Ingest atomically persists an event and its future delivery. A unique constraint
// backs up the client admission lock; READ COMMITTED lets the following SELECT see
// the winner after ON CONFLICT waits for that transaction to finish.
func (s *Store) Ingest(ctx context.Context, client, destination, key string, hash [32]byte, payload []byte) (Event, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Event{}, false, err
	}
	defer rollback(tx)
	if err = lockQuotaClient(ctx, tx, client); err != nil {
		return Event{}, false, err
	}
	var owned bool
	err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM destinations WHERE id=$1 AND client_id=$2)", destination, client).Scan(&owned)
	if err != nil {
		return Event{}, false, err
	}
	if !owned {
		return Event{}, false, ErrNotFound
	}
	// A soft validation failure does not abort the transaction or emit a server
	// error containing JSON context. Pass text, not a JSONB-typed parameter.
	var valid bool
	if err = tx.QueryRow(ctx, "SELECT pg_input_is_valid($1, 'jsonb')", string(payload)).Scan(&valid); err != nil {
		return Event{}, false, err
	}
	if !valid {
		return Event{}, false, ErrInvalidPayload
	}
	e := Event{ID: NewID(), DestinationID: destination, Status: "pending"}
	err = tx.QueryRow(ctx, `INSERT INTO events (id,client_id,destination_id,idempotency_key,request_hash,payload,payload_bytes)
 VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (client_id,idempotency_key) DO NOTHING RETURNING created_at`, e.ID, client, destination, key, hash[:], payload, payload).Scan(&e.CreatedAt)
	duplicate := errors.Is(err, pgx.ErrNoRows)
	if duplicate {
		var storedHash []byte
		err = tx.QueryRow(ctx, `SELECT e.id::text,e.destination_id::text,e.created_at,d.status,e.request_hash
   FROM events e JOIN deliveries d ON d.event_id=e.id WHERE e.client_id=$1 AND e.idempotency_key=$2`, client, key).Scan(&e.ID, &e.DestinationID, &e.CreatedAt, &e.Status, &storedHash)
		if err != nil {
			return Event{}, false, err
		}
		if string(storedHash) != string(hash[:]) {
			return Event{}, false, ErrConflict
		}
	} else if err != nil {
		return Event{}, false, err
	} else {
		// Count the provisional event too. A rejection rolls back its insertion;
		// duplicate receipts bypass quotas because they add no retained work.
		var full bool
		err = tx.QueryRow(ctx, `SELECT count(*) > $2 FROM (SELECT 1 FROM events WHERE client_id=$1 LIMIT $3) retained`, client, MaxClientEvents, MaxClientEvents+1).Scan(&full)
		if err != nil {
			return Event{}, false, err
		}
		if full {
			return Event{}, false, &QuotaError{Resource: "events", Limit: MaxClientEvents}
		}
		if err = checkOpenQuota(ctx, tx, client); err != nil {
			return Event{}, false, err
		}
		_, err = tx.Exec(ctx, "INSERT INTO deliveries (event_id) VALUES ($1)", e.ID)
		if err != nil {
			return Event{}, false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Event{}, false, err
	}
	return e, duplicate, nil
}
func (s *Store) GetEvent(ctx context.Context, client, id string) (Event, error) {
	var e Event
	err := s.pool.QueryRow(ctx, `SELECT e.id::text,e.destination_id::text,d.status,e.created_at
 FROM events e JOIN deliveries d ON d.event_id=e.id WHERE e.id=$1 AND e.client_id=$2`, id, client).Scan(&e.ID, &e.DestinationID, &e.Status, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	return e, err
}

// Cleanup gets a fresh, bounded context even when the request was canceled.
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// WithKeyring is startup-only configuration; do not mutate a running Store.
func (s *Store) WithKeyring(keyring *secrets.Keyring) *Store { s.keyring = keyring; return s }

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// AttemptMetadata is an explicit allowlist for owner-visible history. Never add
// request/response bodies, destination URLs, credentials or lease tokens here.
type AttemptMetadata struct {
	ID             string     `json:"id"`
	Number         int        `json:"attempt_number"`
	State          string     `json:"state"`
	SigningVersion int        `json:"signing_version"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	HTTPStatus     *int       `json:"http_status"`
	ErrorCode      *string    `json:"error_code"`
}

type DeliveryHistory struct {
	EventID       string            `json:"event_id"`
	Status        string            `json:"status"`
	AttemptCount  int               `json:"attempt_count"`
	MaxAttempts   int               `json:"max_attempts"`
	NextAttemptAt *time.Time        `json:"next_attempt_at"`
	Attempts      []AttemptMetadata `json:"attempts"`
}

// GetDeliveryHistory reads ownership, scheduling and attempts in one statement
// snapshot. Separate queries could mix a pre-completion delivery with its newly
// committed result. The unique (event_id, attempt_number) constraint and 1..3
// number range bound this response without pagination or a new index/migration.
func (s *Store) GetDeliveryHistory(ctx context.Context, client, event string) (DeliveryHistory, error) {
	history := DeliveryHistory{MaxAttempts: maxAttempts}
	var attempts []byte
	err := s.pool.QueryRow(ctx, `SELECT e.id::text,d.status,d.attempt_count,d.next_attempt_at,
 COALESCE((SELECT jsonb_agg(jsonb_build_object(
 'id',a.id,'attempt_number',a.attempt_number,'state',a.state,
 'signing_version',a.signing_version,'started_at',a.started_at,
 'finished_at',a.finished_at,'http_status',a.http_status,'error_code',a.error_code
 ) ORDER BY a.attempt_number) FROM delivery_attempts a WHERE a.event_id=e.id),'[]'::jsonb)
 FROM events e JOIN deliveries d ON d.event_id=e.id
 WHERE e.id=$1 AND e.client_id=$2`, event, client).Scan(
		&history.EventID, &history.Status, &history.AttemptCount, &history.NextAttemptAt, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeliveryHistory{}, ErrNotFound
	}
	if err != nil {
		return DeliveryHistory{}, err
	}
	if err = json.Unmarshal(attempts, &history.Attempts); err != nil {
		return DeliveryHistory{}, err
	}
	return history, nil
}

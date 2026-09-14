package storage

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DeliverySummary deliberately excludes payloads, URLs, credentials and leases.
type DeliverySummary struct {
	EventID       string     `json:"event_id"`
	DestinationID string     `json:"destination_id"`
	CreatedAt     time.Time  `json:"created_at"`
	Status        string     `json:"status"`
	AttemptCount  int        `json:"attempt_count"`
	NextAttemptAt *time.Time `json:"next_attempt_at"`
}

type DeliveryPosition struct {
	CreatedAt time.Time `json:"created_at"`
	EventID   string    `json:"event_id"`
}

type DeliveryFilter struct {
	Limit         int
	Status        string
	DestinationID string
	Before        *DeliveryPosition
}

// ListDeliveries uses keyset ordering on immutable event creation time and ID.
// The extra row tells the caller whether another page exists in this snapshot.
// Status is live: filtering is not a transaction snapshot across HTTP requests.
func (s *Store) ListDeliveries(ctx context.Context, client string, filter DeliveryFilter) ([]DeliverySummary, bool, error) {
	if filter.Limit < 1 || filter.Limit > 100 {
		return nil, false, errors.New("invalid delivery page limit")
	}
	if filter.DestinationID != "" {
		var owned bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM destinations WHERE id=$1 AND client_id=$2)", filter.DestinationID, client).Scan(&owned); err != nil {
			return nil, false, err
		}
		if !owned {
			return nil, false, ErrNotFound
		}
	}
	query := `SELECT e.id::text,e.destination_id::text,e.created_at,d.status,d.attempt_count,d.next_attempt_at
 FROM events e JOIN deliveries d ON d.event_id=e.id WHERE e.client_id=$1`
	args := []any{client}
	if filter.DestinationID != "" {
		args = append(args, filter.DestinationID)
		query += fmt.Sprintf(" AND e.destination_id=$%d", len(args))
	}
	if filter.Status != "" {
		args = append(args, filter.Status)
		query += fmt.Sprintf(" AND d.status=$%d", len(args))
	}
	if filter.Before != nil {
		args = append(args, filter.Before.CreatedAt, filter.Before.EventID)
		query += fmt.Sprintf(" AND (e.created_at,e.id)<($%d,$%d)", len(args)-1, len(args))
	}
	args = append(args, filter.Limit+1)
	query += fmt.Sprintf(" ORDER BY e.created_at DESC,e.id DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := make([]DeliverySummary, 0, filter.Limit+1)
	for rows.Next() {
		var item DeliverySummary
		if err = rows.Scan(&item.EventID, &item.DestinationID, &item.CreatedAt, &item.Status, &item.AttemptCount, &item.NextAttemptAt); err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(items) > filter.Limit
	if more {
		items = items[:filter.Limit]
	}
	return items, more, nil
}

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/BoniLuan/relay/internal/delivery"
	"github.com/BoniLuan/relay/internal/secrets"
	"github.com/jackc/pgx/v5"
)

var ErrSigningUnavailable = errors.New("active signing secret unavailable")

// AttemptWork exists only after the start transaction commits. Do not log work:
// URL, payload and credential are private application data, not attempt history.
type AttemptWork struct {
	ID             string          `json:"id"`
	EventID        string          `json:"event_id"`
	Number         int             `json:"attempt_number"`
	SigningVersion int             `json:"signing_version"`
	URL            string          `json:"-"`
	Payload        []byte          `json:"-"`
	Secret         delivery.Secret `json:"-"`
}

func (AttemptWork) Format(s fmt.State, verb rune) { io.WriteString(s, "[REDACTED delivery work]") }

func lockDelivery(ctx context.Context, tx pgx.Tx, event string) error {
	var id string
	err := tx.QueryRow(ctx, "SELECT event_id::text FROM deliveries WHERE event_id=$1 FOR UPDATE", event).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

// StartAttempt pins the currently active version while holding the destination
// lock shared with rotation/revocation. Those operations cannot cancel work that
// already committed. No database transaction is held during HTTP.
func (s *Store) StartAttempt(ctx context.Context, lease Lease) (AttemptWork, error) {
	if s.keyring == nil {
		return AttemptWork{}, secrets.ErrKeyring
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AttemptWork{}, err
	}
	defer rollback(tx)
	if err = lockDelivery(ctx, tx, lease.EventID); err != nil {
		return AttemptWork{}, err
	}
	var client, destination string
	work := AttemptWork{ID: NewID(), EventID: lease.EventID}
	err = tx.QueryRow(ctx, `SELECT e.client_id::text,e.destination_id::text,d.url,
 COALESCE(e.payload_bytes,convert_to(e.payload::text,'UTF8'))
 FROM events e JOIN destinations d ON d.id=e.destination_id AND d.client_id=e.client_id
 WHERE e.id=$1`, lease.EventID).Scan(&client, &destination, &work.URL, &work.Payload)
	if err != nil {
		return AttemptWork{}, err
	}
	if err = lockDestination(ctx, tx, client, destination); err != nil {
		return AttemptWork{}, err
	}
	var keyID string
	var blob []byte
	err = tx.QueryRow(ctx, `SELECT version,key_id,ciphertext FROM signing_secrets
 WHERE client_id=$1 AND destination_id=$2 AND state='active'`, client, destination).Scan(&work.SigningVersion, &keyID, &blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttemptWork{}, ErrSigningUnavailable
	}
	if err != nil {
		return AttemptWork{}, err
	}
	plain, err := s.keyring.Open(keyID, blob, secretAAD(client, destination, work.SigningVersion, keyID))
	if err != nil {
		return AttemptWork{}, secrets.ErrKeyring
	}
	work.Secret, err = delivery.ParseSecret(string(plain))
	if err != nil {
		return AttemptWork{}, secrets.ErrKeyring
	}
	// Check after all lock waits, with room for 5s HTTP and 3s finalization.
	err = tx.QueryRow(ctx, `UPDATE deliveries SET status='attempting',attempt_count=attempt_count+1
 WHERE event_id=$1 AND status='leased' AND lease_owner=$2 AND lease_token=$3
 AND attempt_count<3 AND lease_expires_at>clock_timestamp()+interval '8 seconds'
 RETURNING attempt_count`, lease.EventID, lease.OwnerID, lease.token).Scan(&work.Number)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttemptWork{}, ErrLeaseLost
	}
	if err != nil {
		return AttemptWork{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO delivery_attempts(id,event_id,destination_id,signing_version,lease_token,state,attempt_number)
 VALUES($1,$2,$3,$4,$5,'started',$6)`, work.ID, work.EventID, destination, work.SigningVersion, lease.token, work.Number)
	if err != nil {
		return AttemptWork{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return AttemptWork{}, err
	}
	return work, nil
}

// AttemptResult contains a bounded classification, never a raw error or body.
// A failed attempt does not prove the receiver performed no side effect.
type AttemptResult struct {
	StatusCode int
	ErrorCode  string
}

func (r AttemptResult) state() (string, error) {
	if r.StatusCode != 0 && (r.StatusCode < 100 || r.StatusCode > 599) {
		return "", errors.New("invalid attempt status")
	}
	if r.ErrorCode == "" && r.StatusCode >= 200 && r.StatusCode <= 299 {
		return "succeeded", nil
	}
	switch r.ErrorCode {
	case "http_status":
		if r.StatusCode == 0 || (r.StatusCode >= 200 && r.StatusCode <= 299) {
			return "", errors.New("invalid HTTP failure")
		}
	case "destination", "network", "response", "input", "canceled":
	default:
		return "", errors.New("invalid attempt error code")
	}
	return "failed", nil
}

// FinishAttempt commits history and either a retry schedule or terminal state together. Repeating
// completion, or completion after expiry/recovery, fails closed with ErrLeaseLost.
func (s *Store) FinishAttempt(ctx context.Context, lease Lease, id string, outcome AttemptResult) error {
	state, err := outcome.state()
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err = lockDelivery(ctx, tx, lease.EventID); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRow(ctx, "SELECT attempt_count FROM deliveries WHERE event_id=$1", lease.EventID).Scan(&count); err != nil {
		return err
	}
	deliveryState := state
	var delay time.Duration
	if outcome.retryable() && count < maxAttempts {
		deliveryState = "retry_wait"
		delay = retryDelay(count)
	}
	result, err := tx.Exec(ctx, `UPDATE deliveries SET status=$4,lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,
 next_attempt_at=CASE WHEN $4='retry_wait' THEN clock_timestamp()+($5::bigint*interval '1 millisecond') ELSE NULL END
 WHERE event_id=$1 AND status='attempting' AND lease_owner=$2 AND lease_token=$3
 AND lease_expires_at>clock_timestamp()`, lease.EventID, lease.OwnerID, lease.token, deliveryState, delay.Milliseconds())
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	result, err = tx.Exec(ctx, `UPDATE delivery_attempts SET state=$4,finished_at=clock_timestamp(),
 http_status=NULLIF($5,0),error_code=NULLIF($6,'')
 WHERE id=$1 AND event_id=$2 AND lease_token=$3 AND state='started'`, id, lease.EventID, lease.token, state, outcome.StatusCode, outcome.ErrorCode)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return tx.Commit(ctx)
}

// RecoverAttempt retains an unknown history record and schedules at most one
// bounded retry. Remote side effects may already exist; receivers must deduplicate.
func (s *Store) RecoverAttempt(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	var event, token string
	var count int
	err = tx.QueryRow(ctx, `SELECT event_id::text,lease_token::text,attempt_count FROM deliveries
 WHERE status='attempting' AND lease_expires_at<=statement_timestamp()
 ORDER BY lease_expires_at,event_id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&event, &token, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, err := tx.Exec(ctx, `UPDATE delivery_attempts SET state='unknown',finished_at=clock_timestamp(),error_code='interrupted'
 WHERE event_id=$1 AND lease_token=$2 AND state='started'`, event, token)
	if err != nil {
		return false, err
	}
	if result.RowsAffected() != 1 {
		return false, errors.New("attempt state inconsistent")
	}
	state := "unknown"
	var delay time.Duration
	if count < maxAttempts {
		state = "retry_wait"
		delay = retryDelay(count)
	}
	_, err = tx.Exec(ctx, `UPDATE deliveries SET status=$2,lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,
 next_attempt_at=CASE WHEN $2='retry_wait' THEN clock_timestamp()+($3::bigint*interval '1 millisecond') ELSE NULL END
 WHERE event_id=$1`, event, state, delay.Milliseconds())
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

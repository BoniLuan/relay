package storage

import (
	"context"
	"errors"
	"fmt"
	"io"

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
	result, err := tx.Exec(ctx, `UPDATE deliveries SET status='attempting'
 WHERE event_id=$1 AND status='leased' AND lease_owner=$2 AND lease_token=$3
 AND lease_expires_at>clock_timestamp()+interval '8 seconds'`, lease.EventID, lease.OwnerID, lease.token)
	if err != nil {
		return AttemptWork{}, err
	}
	if result.RowsAffected() != 1 {
		return AttemptWork{}, ErrLeaseLost
	}
	_, err = tx.Exec(ctx, `INSERT INTO delivery_attempts(id,event_id,destination_id,signing_version,lease_token,state)
 VALUES($1,$2,$3,$4,$5,'started')`, work.ID, work.EventID, destination, work.SigningVersion, lease.token)
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

// FinishAttempt commits history and terminal delivery state together. Repeating
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
	result, err := tx.Exec(ctx, `UPDATE deliveries SET status=$4,lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL
 WHERE event_id=$1 AND status='attempting' AND lease_owner=$2 AND lease_token=$3
 AND lease_expires_at>clock_timestamp()`, lease.EventID, lease.OwnerID, lease.token, state)
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

// RecoverAttempt closes at most one expired started attempt as unknown. It never
// makes it claimable again: HTTP may already have reached the receiver.
func (s *Store) RecoverAttempt(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	var event, token string
	err = tx.QueryRow(ctx, `SELECT event_id::text,lease_token::text FROM deliveries
 WHERE status='attempting' AND lease_expires_at<=statement_timestamp()
 ORDER BY lease_expires_at,event_id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&event, &token)
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
	_, err = tx.Exec(ctx, `UPDATE deliveries SET status='unknown',lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL
 WHERE event_id=$1`, event)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

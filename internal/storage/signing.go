package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/BoniLuan/relay/internal/delivery"
	"github.com/BoniLuan/relay/internal/secrets"
	"github.com/jackc/pgx/v5"
)

var ErrSecretState = errors.New("signing secret state conflict")

const canaryText = "relay-keyring-check-v1"

func keyAAD(id string) []byte { return []byte("relay:master-key:" + id) }
func secretAAD(client, destination string, version int, keyID string) []byte {
	return []byte(fmt.Sprintf("relay:signing:v1:%s:%s:%d:%s", client, destination, version, keyID))
}

// RegisterKeyring is explicit administrative work. Verify existing canaries before
// registering new keys, under a lock shared by concurrent registration commands.
func (s *Store) RegisterKeyring(ctx context.Context) error {
	if s.keyring == nil {
		return secrets.ErrKeyring
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(7283519402)"); err != nil {
		return err
	}
	if err = s.checkKeys(ctx, tx, false); err != nil {
		return err
	}
	for _, id := range s.keyring.IDs() {
		blob, err := s.keyring.Seal(id, []byte(canaryText), keyAAD(id))
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO encryption_keys(id,canary) VALUES($1,$2) ON CONFLICT(id) DO NOTHING", id, blob); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type keyQuery interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (s *Store) checkKeys(ctx context.Context, q keyQuery, requireActive bool) error {
	if s.keyring == nil {
		return secrets.ErrKeyring
	}
	rows, err := q.Query(ctx, "SELECT id,canary FROM encryption_keys")
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id string
		var blob []byte
		if err = rows.Scan(&id, &blob); err != nil {
			return err
		}
		plain, err := s.keyring.Open(id, blob, keyAAD(id))
		if err != nil || string(plain) != canaryText {
			return secrets.ErrKeyring
		}
		if id == s.keyring.Active() {
			found = true
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if requireActive && !found {
		return secrets.ErrKeyring
	}
	return nil
}
func (s *Store) CheckKeyring(ctx context.Context) error { return s.checkKeys(ctx, s.pool, true) }

type SigningSecret struct {
	Version   int       `json:"version"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

func lockDestination(ctx context.Context, tx pgx.Tx, client, destination string) error {
	var id string
	err := tx.QueryRow(ctx, "SELECT id::text FROM destinations WHERE client_id=$1 AND id=$2 FOR UPDATE", client, destination).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// StageSigningSecret returns plaintext only after commit. A lost response is
// recovered by revoking this staged version and generating a fresh one.
func (s *Store) StageSigningSecret(ctx context.Context, client, destination string) (SigningSecret, delivery.Secret, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SigningSecret{}, delivery.Secret{}, err
	}
	defer rollback(tx)
	if err = lockDestination(ctx, tx, client, destination); err != nil {
		return SigningSecret{}, delivery.Secret{}, err
	}
	if err = s.checkKeys(ctx, tx, true); err != nil {
		return SigningSecret{}, delivery.Secret{}, err
	}
	var staged bool
	var version int
	err = tx.QueryRow(ctx, "SELECT COALESCE(max(version),0)+1,COALESCE(bool_or(state='staged'),false) FROM signing_secrets WHERE destination_id=$1", destination).Scan(&version, &staged)
	if err != nil {
		return SigningSecret{}, delivery.Secret{}, err
	}
	if staged {
		return SigningSecret{}, delivery.Secret{}, ErrSecretState
	}
	secret, err := delivery.NewSecret()
	if err != nil {
		return SigningSecret{}, delivery.Secret{}, err
	}
	keyID := s.keyring.Active()
	blob, err := s.keyring.Seal(keyID, []byte(secret.Export()), secretAAD(client, destination, version, keyID))
	if err != nil {
		return SigningSecret{}, delivery.Secret{}, err
	}
	meta := SigningSecret{Version: version, State: "staged"}
	err = tx.QueryRow(ctx, `INSERT INTO signing_secrets(client_id,destination_id,version,key_id,ciphertext,state)
 VALUES($1,$2,$3,$4,$5,'staged') RETURNING created_at`, client, destination, version, keyID, blob).Scan(&meta.CreatedAt)
	if err != nil {
		return SigningSecret{}, delivery.Secret{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return SigningSecret{}, delivery.Secret{}, err
	}
	return meta, secret, nil
}
func (s *Store) ListSigningSecrets(ctx context.Context, client, destination string) ([]SigningSecret, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM destinations WHERE id=$1 AND client_id=$2)", destination, client).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, "SELECT version,state,created_at FROM signing_secrets WHERE destination_id=$1 AND client_id=$2 ORDER BY version", destination, client)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SigningSecret{}
	for rows.Next() {
		var meta SigningSecret
		if err = rows.Scan(&meta.Version, &meta.State, &meta.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, meta)
	}
	return out, rows.Err()
}

// Activate is idempotent for the active version, but never resurrects retired or
// revoked versions. Locking the destination serializes lifecycle operations.
func (s *Store) ActivateSigningSecret(ctx context.Context, client, destination string, version int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err = lockDestination(ctx, tx, client, destination); err != nil {
		return err
	}
	var state, keyID string
	var blob []byte
	err = tx.QueryRow(ctx, "SELECT state,key_id,ciphertext FROM signing_secrets WHERE destination_id=$1 AND version=$2", destination, version).Scan(&state, &keyID, &blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != "staged" && state != "active" {
		return ErrSecretState
	}
	if err = s.checkKeys(ctx, tx, true); err != nil {
		return err
	}
	raw, err := s.keyring.Open(keyID, blob, secretAAD(client, destination, version, keyID))
	if err != nil {
		return err
	}
	if _, err = delivery.ParseSecret(string(raw)); err != nil {
		return secrets.ErrKeyring
	}
	if state == "staged" {
		if _, err = tx.Exec(ctx, "UPDATE signing_secrets SET state='retired' WHERE destination_id=$1 AND state='active'", destination); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE signing_secrets SET state='active' WHERE destination_id=$1 AND version=$2", destination, version); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) RevokeSigningSecret(ctx context.Context, client, destination string, version int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err = lockDestination(ctx, tx, client, destination); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, "UPDATE signing_secrets SET state='revoked' WHERE destination_id=$1 AND version=$2", destination, version)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// ActiveSigningSecret is the future worker's credential boundary, not an HTTP
// read API. Re-read before every attempt; never fall back to an older version.
func (s *Store) ActiveSigningSecret(ctx context.Context, client, destination string) (int, delivery.Secret, error) {
	var version int
	var keyID string
	var blob []byte
	err := s.pool.QueryRow(ctx, "SELECT version,key_id,ciphertext FROM signing_secrets WHERE client_id=$1 AND destination_id=$2 AND state='active'", client, destination).Scan(&version, &keyID, &blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, delivery.Secret{}, ErrNotFound
	}
	if err != nil {
		return 0, delivery.Secret{}, err
	}
	raw, err := s.keyring.Open(keyID, blob, secretAAD(client, destination, version, keyID))
	if err != nil {
		return 0, delivery.Secret{}, err
	}
	secret, err := delivery.ParseSecret(string(raw))
	if err != nil {
		return 0, delivery.Secret{}, secrets.ErrKeyring
	}
	return version, secret, nil
}

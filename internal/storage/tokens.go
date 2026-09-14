package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrTokenLimit = errors.New("client already has two active tokens")

// ClientToken contains safe administrative metadata, never the bearer or digest.
type ClientToken struct {
	ID        string     `json:"id"`
	ClientID  string     `json:"client_id"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

func insertClientToken(ctx context.Context, tx pgx.Tx, client, id string, slot int) (ClientToken, string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ClientToken{}, "", err
	}
	value := "relay_" + hex.EncodeToString(random[:])
	digest := sha256.Sum256([]byte(value))
	metadata := ClientToken{ID: id, ClientID: client, State: "active"}
	err := tx.QueryRow(ctx, `INSERT INTO client_tokens(id,client_id,token_hash,slot) VALUES($1,$2,$3,$4) RETURNING created_at`, id, client, digest[:], slot).Scan(&metadata.CreatedAt)
	if err != nil {
		return ClientToken{}, "", err
	}
	return metadata, value, nil
}

// ProvisionClient returns its first token only after both records commit. All
// error paths return empty credentials, including an ambiguous commit failure.
func (s *Store) ProvisionClient(ctx context.Context, name string) (string, string, error) {
	if len(name) < 1 || len(name) > 100 {
		return "", "", errors.New("client name must be 1-100 bytes")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer rollback(tx)
	client := NewID()
	if _, err = tx.Exec(ctx, "INSERT INTO clients(id,name) VALUES($1,$2)", client, name); err != nil {
		return "", "", err
	}
	_, token, err := insertClientToken(ctx, tx, client, client, 1)
	if err != nil {
		return "", "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return client, token, nil
}

func lockClient(ctx context.Context, tx pgx.Tx, client string) error {
	var id string
	err := tx.QueryRow(ctx, "SELECT id::text FROM clients WHERE id=$1 FOR UPDATE", client).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// IssueClientToken grants a second live credential for a controlled cutover.
// Client row locks serialize issuance/revocation; unique slots also enforce the
// two-token ceiling in PostgreSQL. No token is returned before commit succeeds.
func (s *Store) IssueClientToken(ctx context.Context, client string) (ClientToken, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ClientToken{}, "", err
	}
	defer rollback(tx)
	if err = lockClient(ctx, tx, client); err != nil {
		return ClientToken{}, "", err
	}
	var slot int
	err = tx.QueryRow(ctx, `SELECT n FROM generate_series(1,2) n WHERE NOT EXISTS
 (SELECT 1 FROM client_tokens WHERE client_id=$1 AND slot=n) ORDER BY n LIMIT 1`, client).Scan(&slot)
	if errors.Is(err, pgx.ErrNoRows) {
		return ClientToken{}, "", ErrTokenLimit
	}
	if err != nil {
		return ClientToken{}, "", err
	}
	metadata, token, err := insertClientToken(ctx, tx, client, NewID(), slot)
	if err != nil {
		return ClientToken{}, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return ClientToken{}, "", err
	}
	return metadata, token, nil
}

// RevokeClientToken is idempotent and may revoke the final live credential.
// Recovery is administrative issuance, never an HTTP bearer privilege.
func (s *Store) RevokeClientToken(ctx context.Context, client, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err = lockClient(ctx, tx, client); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE client_tokens SET revoked_at=COALESCE(revoked_at,clock_timestamp()),slot=NULL
 WHERE client_id=$1 AND id=$2`, client, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// ListClientTokens returns active credentials first, then newest revoked records,
// with a hard limit of 50 metadata entries. It is an administrative inspection,
// not an exhaustive audit export; both active tokens always fit in the result.
func (s *Store) ListClientTokens(ctx context.Context, client string) ([]ClientToken, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM clients WHERE id=$1)", client).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,client_id::text,
 CASE WHEN revoked_at IS NULL THEN 'active' ELSE 'revoked' END,created_at,revoked_at
 FROM client_tokens WHERE client_id=$1 ORDER BY (revoked_at IS NULL) DESC,created_at DESC,id DESC LIMIT 50`, client)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ClientToken{}
	for rows.Next() {
		var token ClientToken
		if err = rows.Scan(&token.ID, &token.ClientID, &token.State, &token.CreatedAt, &token.RevokedAt); err != nil {
			return nil, err
		}
		result = append(result, token)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// Authentication uses only the current database state, without a token cache.
// A request authenticated before revocation commits may still finish afterward.
func (s *Store) Authenticate(ctx context.Context, token string) (string, error) {
	hash := sha256.Sum256([]byte(token))
	var client string
	err := s.pool.QueryRow(ctx, "SELECT client_id::text FROM client_tokens WHERE token_hash=$1 AND revoked_at IS NULL", hash[:]).Scan(&client)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnauthorized
	}
	if err != nil {
		return "", err
	}
	return client, nil
}

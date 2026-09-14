package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// Used only to populate schemas predating migration 009. Production provisioning
// must never fall back to the legacy column or accept its credential after removal.
func provisionLegacyClient(ctx context.Context, s *Store, name string) (string, string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", "", err
	}
	token := "relay_" + hex.EncodeToString(random[:])
	hash := sha256.Sum256([]byte(token))
	id := NewID()
	_, err := s.pool.Exec(ctx, "INSERT INTO clients(id,name,token_hash) VALUES($1,$2,$3)", id, name, hash[:])
	return id, token, err
}

func tokenTestStore(t *testing.T) *Store {
	t.Helper()
	s, _ := isolatedTestStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestClientTokenRotationAndRecovery(t *testing.T) {
	s := tokenTestStore(t)
	ctx := context.Background()
	client, first, err := s.ProvisionClient(ctx, "rotation")
	if err != nil {
		t.Fatal(err)
	}
	second, next, err := s.IssueClientToken(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{first, next} {
		id, e := s.Authenticate(ctx, value)
		if e != nil || id != client {
			t.Fatal("overlap credential invalid")
		}
	}
	if len(next) != 70 || next == first {
		t.Fatal("invalid issued credential")
	}
	if _, value, err := s.IssueClientToken(ctx, client); !errors.Is(err, ErrTokenLimit) || value != "" {
		t.Fatal("third credential issued")
	}
	foreign, _, err := s.ProvisionClient(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeClientToken(ctx, foreign, second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign token revoked")
	}
	if err = s.RevokeClientToken(ctx, client, client); err != nil {
		t.Fatal(err)
	}
	metadata, err := s.ListClientTokens(ctx, client)
	if err != nil || len(metadata) != 2 || metadata[0].ID != second.ID || metadata[1].RevokedAt == nil {
		t.Fatal("bad metadata ordering")
	}
	firstRevocation := *metadata[1].RevokedAt
	if err = s.RevokeClientToken(ctx, client, client); err != nil {
		t.Fatal(err)
	}
	again, err := s.ListClientTokens(ctx, client)
	if err != nil || !again[1].RevokedAt.Equal(firstRevocation) {
		t.Fatal("revocation was not idempotent")
	}
	if _, err = s.Authenticate(ctx, first); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked credential accepted")
	}
	if err = s.RevokeClientToken(ctx, client, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(ctx, next); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("last revoked credential accepted")
	}
	recovery, value, err := s.IssueClientToken(ctx, client)
	if err != nil {
		t.Fatal("administrative recovery failed")
	}
	// Slot 1 must be reusable even when slot 2 is the surviving active token.
	if recovery.ID == client || recovery.ID == second.ID {
		t.Fatal("metadata identity reused")
	}
	other, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if id, err := other.Authenticate(ctx, value); err != nil || id != client {
		t.Fatal("recovery lost after restart")
	}
	if _, err = other.Authenticate(ctx, first); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revocation lost after restart")
	}
	records, err := other.ListClientTokens(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(records)
	for _, secret := range []string{first, next, value, "token_hash"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("metadata disclosed credential")
		}
	}
	if _, err = s.ListClientTokens(ctx, NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing client not rejected")
	}
	if _, _, err = s.IssueClientToken(ctx, NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatal("issued token for missing client")
	}
}

func TestConcurrentClientTokenIssuance(t *testing.T) {
	s := tokenTestStore(t)
	ctx := context.Background()
	client, _, err := s.ProvisionClient(ctx, "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		metadata ClientToken
		value    string
		err      error
	}
	out := make(chan result, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m, v, e := s.IssueClientToken(ctx, client); out <- result{m, v, e} }()
	}
	wg.Wait()
	close(out)
	winners := 0
	var second ClientToken
	for r := range out {
		if r.err == nil {
			winners++
			second = r.metadata
		} else if !errors.Is(r.err, ErrTokenLimit) || r.value != "" || r.metadata.ID != "" {
			t.Fatal("invalid failed issuance")
		}
	}
	if winners != 1 {
		t.Fatal("concurrency exceeded two active credentials")
	}
	if err = s.RevokeClientToken(ctx, client, client); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.IssueClientToken(ctx, client); err != nil {
		t.Fatal("vacant first slot not reused")
	}
	// Race revocation against creation while both slots are occupied. The issue
	// either sees the old full state or the freed slot; it never exceeds the cap.
	errorsOut := make(chan error, 2)
	go func() { errorsOut <- s.RevokeClientToken(ctx, client, second.ID) }()
	go func() { _, _, err := s.IssueClientToken(ctx, client); errorsOut <- err }()
	for i := 0; i < 2; i++ {
		err := <-errorsOut
		if err != nil && !errors.Is(err, ErrTokenLimit) {
			t.Fatal(err)
		}
	}
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM client_tokens WHERE client_id=$1 AND revoked_at IS NULL", client).Scan(&count); err != nil || count > 2 {
		t.Fatal("concurrent mutation broke cap")
	}
}

func TestClientTokenTransactionsFailClosed(t *testing.T) {
	s := tokenTestStore(t)
	ctx := context.Background()
	client, old, err := s.ProvisionClient(ctx, "rollback owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_token() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected client token commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER reject_token AFTER INSERT OR UPDATE ON client_tokens DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_token()`); err != nil {
		t.Fatal(err)
	}
	id, value, err := s.ProvisionClient(ctx, "rollback sentinel")
	if err == nil || id != "" || value != "" {
		t.Fatal("failed client creation disclosed credential")
	}
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM clients WHERE name='rollback sentinel'").Scan(&count); err != nil || count != 0 {
		t.Fatal("partial client committed")
	}
	m, value, err := s.IssueClientToken(ctx, client)
	if err == nil || value != "" || m.ID != "" {
		t.Fatal("failed issuance disclosed credential")
	}
	if err = s.RevokeClientToken(ctx, client, client); err == nil {
		t.Fatal("revocation commit should fail")
	}
	if id, err = s.Authenticate(ctx, old); err != nil || id != client {
		t.Fatal("failed revocation changed credential")
	}
	if _, err = s.pool.Exec(ctx, "DROP TRIGGER reject_token ON client_tokens"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.IssueClientToken(ctx, client); err != nil {
		t.Fatal("rollback consumed active slot")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, value, err = s.IssueClientToken(canceled, client); err == nil || value != "" {
		t.Fatal("canceled issue succeeded")
	}
}

func TestClientTokenMigrationPreservesCredentials(t *testing.T) {
	s, _ := isolatedTestStore(t)
	ctx := context.Background()
	schemas := []string{initialSchema, signingSchema, leaseSchema, attemptSchema, retrySchema, cooldownSchema, listingSchema, replaySchema}
	if _, err := s.pool.Exec(ctx, "CREATE TABLE schema_migrations(version integer PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	for i, sql := range schemas {
		if _, err := s.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, "INSERT INTO schema_migrations VALUES($1)", i+1); err != nil {
			t.Fatal(err)
		}
	}
	client, token, err := provisionLegacyClient(ctx, s, "migrated")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := s.Authenticate(ctx, token); err != nil || id != client {
		t.Fatal("existing credential lost")
	}
	metadata, err := s.ListClientTokens(ctx, client)
	if err != nil || len(metadata) != 1 || metadata[0].ID != client || metadata[0].State != "active" {
		t.Fatal("missing migrated token metadata")
	}
	var columns int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='clients' AND column_name='token_hash'").Scan(&columns); err != nil || columns != 0 {
		t.Fatal("legacy auth fallback still exists")
	}
	if err = s.RevokeClientToken(ctx, client, client); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("legacy token bypassed revocation")
	}
}

func TestTokenMetadataBoundAndDatabaseSlotConstraints(t *testing.T) {
	s := tokenTestStore(t)
	ctx := context.Background()
	client, _, err := s.ProvisionClient(ctx, "metadata bound")
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic revoked history; the old live token must still be listed first.
	for i := 0; i < 55; i++ {
		hash := sha256.Sum256([]byte(fmt.Sprint(i)))
		if _, err = s.pool.Exec(ctx, "INSERT INTO client_tokens(id,client_id,token_hash,revoked_at) VALUES($1,$2,$3,clock_timestamp())", NewID(), client, hash[:]); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.ListClientTokens(ctx, client)
	if err != nil || len(items) != 50 || items[0].ID != client {
		t.Fatal("metadata exceeded bound or hid active token")
	}
	// PostgreSQL CHECK must not silently accept NULL via three-valued logic.
	_, err = s.pool.Exec(ctx, "INSERT INTO client_tokens(id,client_id,token_hash) VALUES($1,$2,$3)", NewID(), client, sha256.New().Sum(nil))
	if err == nil {
		t.Fatal("active token without a slot accepted")
	}
}

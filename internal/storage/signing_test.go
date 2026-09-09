package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BoniLuan/relay/internal/secrets"
	"github.com/jackc/pgx/v5"
)

func isolatedTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	raw := os.Getenv("RELAY_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("run make test-integration")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "relay-test-db" || u.Path != "/relay_test" || u.User == nil || u.User.Username() != "relay_test" {
		t.Fatal("requires isolated Relay test database")
	}
	ctx := context.Background()
	admin, err := Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "relay_test_" + strings.ReplaceAll(NewID(), "-", "")
	if _, err = admin.pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		admin.pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	return db, u.String()
}
func signingTestStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	db, raw := isolatedTestStore(t)
	ctx := context.Background()
	var err error
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err = secrets.InitFile(path); err != nil {
		t.Fatal(err)
	}
	k, err := secrets.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	db.WithKeyring(k)
	if err = db.RegisterKeyring(ctx); err != nil {
		t.Fatal(err)
	}
	return db, raw, path
}
func TestSigningLifecycle(t *testing.T) {
	s, raw, path := signingTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, _, err := s.ProvisionClient(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.ProvisionClient(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDestination(ctx, client, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StageSigningSecret(ctx, other, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign stage: %v", err)
	}
	const n = 8
	results := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, err := s.StageSigningSecret(ctx, client, d.ID); results <- err }()
	}
	wg.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
		} else if !errors.Is(err, ErrSecretState) {
			t.Fatal(err)
		}
	}
	if created != 1 {
		t.Fatalf("created=%d", created)
	}
	if _, _, err = s.ActiveSigningSecret(ctx, client, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("staged version used as active")
	}
	if err = s.RevokeSigningSecret(ctx, client, d.ID, 1); err != nil {
		t.Fatal(err)
	}
	meta, secret, err := s.StageSigningSecret(ctx, client, d.ID)
	if err != nil || meta.Version != 2 {
		t.Fatalf("replacement: %+v %v", meta, err)
	}
	if err = s.ActivateSigningSecret(ctx, other, d.ID, 2); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign activation")
	}
	if err = s.RevokeSigningSecret(ctx, other, d.ID, 2); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign revocation")
	}
	if _, err = s.ListSigningSecrets(ctx, other, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign metadata")
	}
	if err = s.ActivateSigningSecret(ctx, client, d.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateSigningSecret(ctx, client, d.ID, 2); err != nil {
		t.Fatal("activation retry failed")
	}
	var blob []byte
	if err = s.pool.QueryRow(ctx, "SELECT ciphertext FROM signing_secrets WHERE destination_id=$1 AND version=2", d.ID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte(secret.Export())) {
		t.Fatal("plaintext persisted")
	}
	reloaded, err := secrets.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.WithKeyring(reloaded)
	if err = restarted.CheckKeyring(ctx); err != nil {
		t.Fatal(err)
	}
	version, recovered, err := restarted.ActiveSigningSecret(ctx, client, d.ID)
	if err != nil || version != 2 || recovered != secret {
		t.Fatal("secret did not survive restart")
	}
	next, _, err := s.StageSigningSecret(ctx, client, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateSigningSecret(ctx, client, d.ID, next.Version); err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateSigningSecret(ctx, client, d.ID, 2); !errors.Is(err, ErrSecretState) {
		t.Fatal("retired version resurrected")
	}
	if err = s.RevokeSigningSecret(ctx, client, d.ID, next.Version); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeSigningSecret(ctx, client, d.ID, next.Version); err != nil {
		t.Fatal("revocation retry failed")
	}
	if _, _, err = s.ActiveSigningSecret(ctx, client, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("revocation fell back to retired key")
	}
	if err = s.ActivateSigningSecret(ctx, client, d.ID, next.Version); !errors.Is(err, ErrSecretState) {
		t.Fatal("revoked version resurrected")
	}
}
func TestSigningTamperAndWrongKey(t *testing.T) {
	s, raw, _ := signingTestStore(t)
	ctx := context.Background()
	client, _, err := s.ProvisionClient(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateDestination(ctx, client, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateDestination(ctx, client, "https://example.org")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if _, _, err = s.StageSigningSecret(ctx, client, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.pool.Exec(ctx, "UPDATE signing_secrets SET ciphertext=(SELECT ciphertext FROM signing_secrets WHERE destination_id=$1) WHERE destination_id=$2", a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateSigningSecret(ctx, client, b.ID, 1); !errors.Is(err, secrets.ErrKeyring) {
		t.Fatal("cross-destination ciphertext accepted")
	}
	if _, err = s.pool.Exec(ctx, "UPDATE signing_secrets SET ciphertext=set_byte(ciphertext,0,get_byte(ciphertext,0)#1) WHERE destination_id=$1", a.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateSigningSecret(ctx, client, a.ID, 1); !errors.Is(err, secrets.ErrKeyring) {
		t.Fatal("tampered nonce accepted")
	}
	path := filepath.Join(t.TempDir(), "wrong.json")
	if err = secrets.InitFile(path); err != nil {
		t.Fatal(err)
	}
	wrong, err := secrets.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.WithKeyring(wrong)
	if err = other.CheckKeyring(ctx); !errors.Is(err, secrets.ErrKeyring) {
		t.Fatal("wrong key passed canary")
	}
	if err = other.RegisterKeyring(ctx); !errors.Is(err, secrets.ErrKeyring) {
		t.Fatal("wrong key overwrote canary")
	}
	if err = s.CheckKeyring(ctx); err != nil {
		t.Fatal("original key no longer works")
	}
}
func TestStageCommitFailureDoesNotExposeSecret(t *testing.T) {
	s, _, _ := signingTestStore(t)
	ctx := context.Background()
	client, _, err := s.ProvisionClient(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDestination(ctx, client, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `CREATE FUNCTION fail_secret() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected secret commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER fail_secret AFTER INSERT ON signing_secrets DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_secret()`)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, stageErr := s.StageSigningSecret(ctx, client, d.ID)
	if stageErr == nil || secret.Export() != "" {
		t.Fatal("failed commit exposed a credential")
	}
	metadata, err := s.ListSigningSecrets(ctx, client, d.ID)
	if err != nil || len(metadata) != 0 {
		t.Fatal("failed commit persisted metadata")
	}
	if _, err = s.pool.Exec(ctx, "DROP TRIGGER fail_secret ON signing_secrets; DROP FUNCTION fail_secret()"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StageSigningSecret(ctx, client, d.ID); err != nil {
		t.Fatal("retry after failed commit")
	}
}

func TestMasterKeyRollover(t *testing.T) {
	s, raw, path := signingTestStore(t)
	ctx := context.Background()
	client, _, err := s.ProvisionClient(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDestination(ctx, client, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, original, err := s.StageSigningSecret(ctx, client, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateSigningSecret(ctx, client, d.ID, 1); err != nil {
		t.Fatal(err)
	}
	nextPath := filepath.Join(t.TempDir(), "next.json")
	if err = secrets.InitFile(nextPath); err != nil {
		t.Fatal(err)
	}
	type config struct {
		Active string            `json:"active"`
		Keys   map[string]string `json:"keys"`
	}
	var cfg, next config
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(nextPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &next); err != nil {
		t.Fatal(err)
	}
	cfg.Active = "key-2"
	cfg.Keys["key-2"] = next.Keys["key-1"]
	data, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	keyring, err := secrets.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer rotated.Close()
	rotated.WithKeyring(keyring)
	if err = rotated.CheckKeyring(ctx); !errors.Is(err, secrets.ErrKeyring) {
		t.Fatal("unregistered active key accepted")
	}
	if err = rotated.RegisterKeyring(ctx); err != nil {
		t.Fatal(err)
	}
	if _, recovered, err := rotated.ActiveSigningSecret(ctx, client, d.ID); err != nil || recovered != original {
		t.Fatal("old ciphertext lost after rollover")
	}
	meta, _, err := rotated.StageSigningSecret(ctx, client, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	var storedKey string
	if err = rotated.pool.QueryRow(ctx, "SELECT key_id FROM signing_secrets WHERE destination_id=$1 AND version=$2", d.ID, meta.Version).Scan(&storedKey); err != nil || storedKey != "key-2" {
		t.Fatal("new encryption did not use active master key")
	}
	if err = s.CheckKeyring(ctx); !errors.Is(err, secrets.ErrKeyring) {
		t.Fatal("incomplete keyring accepted")
	}
}

func TestMigrationUpgradePreservesExistingData(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			s, _ := isolatedTestStore(t)
			ctx := context.Background()
			if _, err := s.pool.Exec(ctx, initialSchema); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(ctx, "CREATE TABLE schema_migrations(version integer PRIMARY KEY); INSERT INTO schema_migrations VALUES(1)"); err != nil {
				t.Fatal(err)
			}
			if version == 2 {
				if _, err := s.pool.Exec(ctx, signingSchema); err != nil {
					t.Fatal(err)
				}
				if _, err := s.pool.Exec(ctx, "INSERT INTO schema_migrations VALUES(2)"); err != nil {
					t.Fatal(err)
				}
			}
			client, _, err := s.ProvisionClient(ctx, "legacy")
			if err != nil {
				t.Fatal(err)
			}
			d, err := s.CreateDestination(ctx, client, "https://example.com")
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte(`null`)
			e, _, err := s.Ingest(ctx, client, d.ID, "legacy", sha256.Sum256(payload), payload)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if err = s.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			found, err := s.GetEvent(ctx, client, e.ID)
			if err != nil || found.ID != e.ID || found.Status != "pending" {
				t.Fatal("migration lost or changed existing event")
			}
			metadata, err := s.ListSigningSecrets(ctx, client, d.ID)
			if err != nil || len(metadata) != 0 {
				t.Fatal("migration generated an undisclosed secret")
			}
			lease, err := s.ClaimDelivery(ctx, NewID(), time.Minute)
			if err != nil || lease.EventID != e.ID {
				t.Fatalf("legacy delivery not claimable: %v", err)
			}
		})
	}
}

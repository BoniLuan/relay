package delivery_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BoniLuan/relay/internal/delivery"
	"github.com/BoniLuan/relay/internal/secrets"
	"github.com/BoniLuan/relay/internal/storage"
	"github.com/BoniLuan/relay/internal/worker"
)

// This test deliberately uses fixed, disposable database endpoints, never a
// caller-supplied URL. Run only through make test-restore's internal network.
func TestBackupRestoreDrill(t *testing.T) {
	if os.Getenv("RELAY_RESTORE_DRILL") != "isolated" {
		t.Skip("run make test-restore")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	open := func(host string) *storage.Store {
		t.Helper()
		db, err := storage.Open(ctx, "postgres://relay_drill:relay_drill_ephemeral@"+host+":5432/relay_drill?sslmode=disable")
		must(err)
		t.Cleanup(db.Close)
		return db
	}
	command := func(name string, args ...string) {
		t.Helper()
		// Tool stderr can contain SQL or sensitive backup details. Retain only status.
		if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
			t.Fatalf("%s failed", name)
		}
	}
	root := t.TempDir()
	keyFile := filepath.Join(root, "live.json")
	must(secrets.InitFile(keyFile))
	load := func(path string) *secrets.Keyring { t.Helper(); k, err := secrets.LoadFile(path); must(err); return k }
	source := open("source")
	must(source.Migrate(ctx))
	source.WithKeyring(load(keyFile))
	must(source.RegisterKeyring(ctx))
	client, token, err := source.ProvisionClient(ctx, "restore fixture")
	must(err)
	const payload = `{ "restore":9007199254740993 }`
	hash := sha256.Sum256([]byte(payload))
	type fixture struct {
		destination storage.Destination
		event       storage.Event
		secret      delivery.Secret
		idempotency string
	}
	var fixtures []fixture
	seed := func(idempotency string) {
		t.Helper()
		d, err := source.CreateDestination(ctx, client, "https://example.com/hook")
		must(err)
		meta, key, err := source.StageSigningSecret(ctx, client, d.ID)
		must(err)
		must(source.ActivateSigningSecret(ctx, client, d.ID, meta.Version))
		event, duplicate, err := source.Ingest(ctx, client, d.ID, idempotency, hash, []byte(payload))
		must(err)
		if duplicate {
			t.Fatal("unexpected seed duplicate")
		}
		fixtures = append(fixtures, fixture{d, event, key, idempotency})
	}
	seed("old-master")
	// Rotate the active master while preserving the historical master. Each
	// destination then requires a different key from the independent key backup.
	type keyConfig struct {
		Active string            `json:"active"`
		Keys   map[string]string `json:"keys"`
	}
	readConfig := func(path string) keyConfig {
		t.Helper()
		raw, err := os.ReadFile(path)
		must(err)
		var c keyConfig
		must(json.Unmarshal(raw, &c))
		return c
	}
	old := readConfig(keyFile)
	extra := filepath.Join(root, "extra.json")
	must(secrets.InitFile(extra))
	combined := readConfig(keyFile)
	combined.Active = "key-2"
	combined.Keys["key-2"] = readConfig(extra).Keys["key-1"]
	writeConfig := func(path string, c keyConfig) {
		t.Helper()
		raw, err := json.Marshal(c)
		must(err)
		must(os.WriteFile(path, raw, 0600))
	}
	writeConfig(keyFile, combined)
	source.WithKeyring(load(keyFile))
	must(source.RegisterKeyring(ctx))
	seed("new-master")
	// Seed completed history as well as pending work, with real TLS and HMAC.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var calls atomic.Int32
	sender, _ := delivery.NewFixtureSender(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error("receiver read failed")
		}
		valid := false
		for _, f := range fixtures {
			if r.Header.Get("Relay-Event-ID") == f.event.ID {
				valid = string(body) == payload && delivery.Verify(f.secret, r.Header.Get("Relay-Signature"), f.event.ID, body, time.Now())
			}
		}
		if !valid {
			t.Error("restored signature, identity or bytes mismatch")
			w.WriteHeader(400)
			return
		}
		calls.Add(1)
		w.WriteHeader(204)
	}))
	must(worker.RunOnce(ctx, source, sender, logger, 30*time.Second))
	if calls.Load() != 1 {
		t.Fatal("seed history absent")
	}
	before := make(map[string]string)
	histories := make(map[string]storage.DeliveryHistory)
	for _, f := range fixtures {
		e, err := source.GetEvent(ctx, client, f.event.ID)
		must(err)
		before[e.ID] = e.Status
		h, err := source.GetDeliveryHistory(ctx, client, e.ID)
		must(err)
		histories[e.ID] = h
	}
	// Separate database and secret artifacts; neither is a production backup.
	dataBackup := filepath.Join(root, "database")
	secretBackup := filepath.Join(root, "secret-backup")
	must(os.Mkdir(dataBackup, 0700))
	must(os.Mkdir(secretBackup, 0700))
	dump := filepath.Join(dataBackup, "relay.dump")
	command("pg_dump", "-h", "source", "-U", "relay_drill", "-d", "relay_drill", "-Fc", "-f", dump)
	must(os.Chmod(dump, 0600))
	savedKeys := filepath.Join(secretBackup, "keyring.json")
	raw, err := os.ReadFile(keyFile)
	must(err)
	must(os.WriteFile(savedKeys, raw, 0600))
	// Remove the source database and live key files before restoring, proving the
	// recovered service cannot silently depend on the original database/key files.
	source.Close()
	command("dropdb", "-h", "source", "-U", "relay_drill", "relay_drill")
	must(os.Remove(keyFile))
	must(os.Remove(extra))
	command("pg_restore", "-h", "restored", "-U", "relay_drill", "-d", "relay_drill", "--no-owner", "--no-acl", "--exit-on-error", "--single-transaction", dump)
	restored := open("restored")
	if restored.CheckKeyring(ctx) == nil {
		t.Fatal("missing keyring accepted")
	}
	if _, err := secrets.LoadFile(keyFile); err == nil {
		t.Fatal("missing key file accepted")
	}
	wrong := filepath.Join(root, "wrong.json")
	must(secrets.InitFile(wrong))
	restored.WithKeyring(load(wrong))
	if restored.CheckKeyring(ctx) == nil || restored.RegisterKeyring(ctx) == nil {
		t.Fatal("wrong master accepted")
	}
	// A backup containing only the new active key must not discard the old one.
	incomplete := keyConfig{Active: "key-2", Keys: map[string]string{"key-2": combined.Keys["key-2"]}}
	writeConfig(wrong, incomplete)
	restored.WithKeyring(load(wrong))
	if restored.CheckKeyring(ctx) == nil {
		t.Fatal("missing historical master accepted")
	}
	// An old backup missing the new active master must fail as well.
	old.Active = "key-1"
	writeConfig(wrong, old)
	restored.WithKeyring(load(wrong))
	if restored.CheckKeyring(ctx) == nil {
		t.Fatal("missing active master accepted")
	}
	restoredKeys := filepath.Join(root, "restored.json")
	raw, err = os.ReadFile(savedKeys)
	must(err)
	must(os.WriteFile(restoredKeys, raw, 0600))
	restored.WithKeyring(load(restoredKeys))
	must(restored.Migrate(ctx))
	must(restored.RegisterKeyring(ctx))
	must(restored.Ready(ctx))
	owner, err := restored.Authenticate(ctx, token)
	must(err)
	if owner != client {
		t.Fatal("restored authentication changed")
	}
	for _, f := range fixtures {
		history, err := restored.GetDeliveryHistory(ctx, client, f.event.ID)
		must(err)
		if !reflect.DeepEqual(history, histories[f.event.ID]) {
			t.Fatal("restored attempt history changed")
		}
		version, key, err := restored.ActiveSigningSecret(ctx, client, f.destination.ID)
		must(err)
		if version != 1 || key != f.secret {
			t.Fatal("restored signing secret mismatch")
		}
		e, duplicate, err := restored.Ingest(ctx, client, f.destination.ID, f.idempotency, hash, []byte(payload))
		must(err)
		if !duplicate || e.ID != f.event.ID || e.Status != before[e.ID] {
			t.Fatal("restore lost identity, state or idempotency")
		}
		changed := sha256.Sum256([]byte(`{"different":true}`))
		_, _, err = restored.Ingest(ctx, client, f.destination.ID, f.idempotency, changed, []byte(`{"different":true}`))
		if !errors.Is(err, storage.ErrConflict) {
			t.Fatal("restored idempotency accepted conflicting content")
		}
	}
	must(worker.RunOnce(ctx, restored, sender, logger, 30*time.Second))
	must(worker.RunOnce(ctx, restored, sender, logger, 30*time.Second))
	if calls.Load() != 2 {
		t.Fatal("restored work lost or completed work resent")
	}
	for _, f := range fixtures {
		e, err := restored.GetEvent(ctx, client, f.event.ID)
		must(err)
		if e.Status != "succeeded" {
			t.Fatal("restored delivery incomplete")
		}
	}
	t.Log("dump/restore, two master keys, negative key checks, auth, idempotency, history and signed TLS delivery passed")
}

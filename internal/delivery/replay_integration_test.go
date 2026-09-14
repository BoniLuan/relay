package delivery_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BoniLuan/relay/internal/delivery"
	"github.com/BoniLuan/relay/internal/httpapi"
	"github.com/BoniLuan/relay/internal/storage"
	"github.com/BoniLuan/relay/internal/worker"
)

func TestControlledReplayToSignedReceiver(t *testing.T) {
	db, pool := integrationStore(t)
	ctx := context.Background()
	client, token, err := db.ProvisionClient(ctx, "replay HTTP owner")
	if err != nil {
		t.Fatal(err)
	}
	_, foreign, err := db.ProvisionClient(ctx, "foreign replay owner")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := db.CreateDestination(ctx, client, "https://example.com/hook?private=replay-url")
	if err != nil {
		t.Fatal(err)
	}
	first, key1, err := db.StageSigningSecret(ctx, client, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ActivateSigningSecret(ctx, client, dst.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	api := httpapi.NewHandler(db, logger)
	request := func(method, path, body, key, auth string, want int) []byte {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+auth)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.Bytes()
	}
	const payload = `{ "private":"replay-payload-marker", "n":9007199254740993 }`
	body := `{"destination_id":"` + dst.ID + `","payload":` + payload + `}`
	var event storage.Event
	if err = json.Unmarshal(request("POST", "/api/v1/events", body, "ingest-key", token, 201), &event); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/events/" + event.ID + "/replay"
	const replayBody = `{"acknowledge_duplicate_risk":true}`
	request("POST", path, replayBody, "replay-key", token, 409)
	request("POST", path, replayBody, "replay-key", foreign, 404)
	var requests, effects atomic.Int32
	key2 := key1
	var receiverMu sync.Mutex
	seen := map[string]bool{}
	sender, _ := delivery.NewFixtureSender(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receiverMu.Lock()
		defer receiverMu.Unlock()
		n := requests.Add(1)
		raw, _ := io.ReadAll(r.Body)
		key := key1
		if n > 1 {
			key = key2
		}
		if string(raw) != payload || r.Header.Get("Relay-Event-ID") != event.ID || !delivery.Verify(key, r.Header.Get("Relay-Signature"), event.ID, raw, time.Now()) {
			t.Error("replay changed identity, bytes or signing key")
		}
		// Model a receiver that committed a business effect but returned an error.
		// Stable event IDs allow its next request to be deduplicated.
		if n == 1 {
			if !seen[r.Header.Get("Relay-Event-ID")] {
				seen[r.Header.Get("Relay-Event-ID")] = true
				effects.Add(1)
			}
			w.WriteHeader(400)
		} else {
			w.WriteHeader(204)
		}
	}))
	if err = worker.RunOnce(ctx, db, sender, logger, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `CREATE FUNCTION reject_replay_http() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected replay HTTP commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER reject_replay_http AFTER INSERT ON delivery_replays DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_replay_http()`); err != nil {
		t.Fatal(err)
	}
	request("POST", path, replayBody, "replay-key", token, 503)
	if _, err = pool.Exec(ctx, "DROP TRIGGER reject_replay_http ON delivery_replays"); err != nil {
		t.Fatal(err)
	}
	var receipt storage.ReplayReceipt
	if err = json.Unmarshal(request("POST", path, replayBody, "replay-key", token, 201), &receipt); err != nil || receipt.ID == "" || receipt.MaxAttempts != 4 {
		t.Fatal("missing durable receipt")
	}
	if requests.Load() != 1 {
		t.Fatal("API sent replay synchronously")
	}
	meta, newKey, err := db.StageSigningSecret(ctx, client, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	receiverMu.Lock()
	key2 = newKey
	receiverMu.Unlock()
	if err = db.ActivateSigningSecret(ctx, client, dst.ID, meta.Version); err != nil {
		t.Fatal(err)
	}
	if err = db.RevokeSigningSecret(ctx, client, dst.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	request("POST", path, replayBody, "replay-key", token, 200)
	request("POST", path, replayBody+" ", "replay-key", token, 409)
	if err = worker.RunOnce(ctx, db, sender, logger, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	request("POST", path, replayBody, "replay-key", token, 200)
	request("POST", path, replayBody, "another-key", token, 409)
	var history storage.DeliveryHistory
	if err = json.Unmarshal(request("GET", "/api/v1/events/"+event.ID+"/attempts", "", "", token, 200), &history); err != nil {
		t.Fatal(err)
	}
	if history.Status != "succeeded" || len(history.Attempts) != 2 || history.Replay == nil || history.Replay.ID != receipt.ID || history.Attempts[0].SigningVersion != 1 || history.Attempts[1].SigningVersion != 2 || requests.Load() != 2 || effects.Load() != 1 {
		t.Fatal("lost replay audit, deduplication or key rotation")
	}
	for _, private := range []string{payload, token, key1.Export(), key2.Export(), "replay-url"} {
		if strings.Contains(logs.String(), private) {
			t.Fatal("private replay data logged")
		}
	}
}

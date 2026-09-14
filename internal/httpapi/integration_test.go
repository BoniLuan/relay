package httpapi

import (
	"context"
	"encoding/json"
	"github.com/BoniLuan/relay/internal/secrets"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BoniLuan/relay/internal/storage"
)

func TestPostgresHTTP(t *testing.T) {
	raw := os.Getenv("RELAY_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("run make test-integration")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "relay-test-db" || u.Path != "/relay_test" || u.User == nil || u.User.Username() != "relay_test" {
		t.Fatal("expected isolated test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := storage.Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "keyring.json")
	if err = secrets.InitFile(keyPath); err != nil {
		t.Fatal(err)
	}
	keyring, err := secrets.LoadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	db.WithKeyring(keyring)
	if err = db.RegisterKeyring(ctx); err != nil {
		t.Fatal(err)
	}
	client, token, err := db.ProvisionClient(ctx, "http-test")
	if err != nil {
		t.Fatal(err)
	}
	_, otherToken, err := db.ProvisionClient(ctx, "other-http-test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewHandler(db, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer server.Close()
	request := func(method, path, body, key, credential string, want int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		result, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatalf("%s %s: %d %s; want %d", method, path, response.StatusCode, result, want)
		}
		return result
	}
	var d storage.Destination
	if err = json.Unmarshal(request("POST", "/api/v1/destinations", `{"url":"https://example.com/hook"}`, "", token, 201), &d); err != nil {
		t.Fatal(err)
	}
	secretPath := "/api/v1/destinations/" + d.ID + "/signing-secrets"
	request("POST", secretPath, `{}`, "", otherToken, 404)
	var credential struct {
		Version int    `json:"version"`
		Secret  string `json:"secret"`
	}
	if err = json.Unmarshal(request("POST", secretPath, `{}`, "", token, 201), &credential); err != nil || credential.Secret == "" {
		t.Fatal("missing staged credential")
	}
	request("POST", secretPath, `{}`, "", token, 409)
	listing := request("GET", secretPath, "", "", token, 200)
	if strings.Contains(string(listing), credential.Secret) || strings.Contains(string(listing), "ciphertext") {
		t.Fatal("metadata exposed secret")
	}
	request("POST", secretPath+"/1/activate", `{}`, "", otherToken, 404)
	request("POST", secretPath+"/1/activate", `{}`, "", token, 204)
	request("POST", secretPath+"/1/activate", `{}`, "", token, 204)
	request("DELETE", secretPath+"/1", "", "", otherToken, 404)
	request("DELETE", secretPath+"/1", "", "", token, 204)
	request("POST", secretPath+"/1/activate", `{}`, "", token, 409)
	body := `{"destination_id":"` + d.ID + `","payload":{"value":42}}`
	// All are syntactically valid JSON but cannot be represented as JSONB.
	for _, payload := range []string{`{"value":"RELAY_LOG_SENTINEL\u0000"}`, `1e1000000`, `1e-1000000`, `"\ud800"`} {
		invalidKey := storage.NewID()
		invalidBody := `{"destination_id":"` + d.ID + `","payload":` + payload + `}`
		response := request("POST", "/api/v1/events", invalidBody, invalidKey, token, 400)
		if strings.Contains(string(response), "RELAY_LOG_SENTINEL") || strings.Contains(string(response), "retry") {
			t.Fatal("invalid payload leaked or marked retryable")
		}
		// Rejection must not reserve the key or persist a partial event.
		request("POST", "/api/v1/events", body, invalidKey, token, 201)
	}
	for _, payload := range []string{`null`, `"\ud83d\ude00"`, `9007199254740993`, `1e1000`, `"literal \\u0000"`} {
		accepted := `{"destination_id":"` + d.ID + `","payload":` + payload + `}`
		request("POST", "/api/v1/events", accepted, storage.NewID(), token, 201)
	}
	key := storage.NewID()
	var event, replayed storage.Event
	if err = json.Unmarshal(request("POST", "/api/v1/events", body, key, token, 201), &event); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(request("POST", "/api/v1/events", body, key, token, 200), &replayed); err != nil {
		t.Fatal(err)
	}
	if event.ID == "" || event.ID != replayed.ID {
		t.Fatal("unstable event ID")
	}
	request("POST", "/api/v1/events", body+" ", key, token, 409)
	request("POST", "/api/v1/events", body, key, otherToken, 404)
	request("GET", "/api/v1/events/"+event.ID, "", "", otherToken, 404)
	historyPath := "/api/v1/events/" + event.ID + "/attempts"
	request("GET", historyPath, "", "", otherToken, 404)
	request("GET", historyPath, "", "", "", 401)
	request("GET", "/api/v1/events/"+storage.NewID()+"/attempts", "", "", token, 404)
	var history storage.DeliveryHistory
	if err = json.Unmarshal(request("GET", historyPath, "", "", token, 200), &history); err != nil || history.EventID != event.ID || history.Status != "pending" || history.Attempts == nil || len(history.Attempts) != 0 || history.MaxAttempts != 3 {
		t.Fatalf("unexpected owner history: %+v, %v", history, err)
	}
	// Traverse real HTTP cursors against PostgreSQL, preserving owner filtering.
	var baseline deliveryPage
	if err = json.Unmarshal(request("GET", "/api/v1/deliveries?limit=100", "", "", token, 200), &baseline); err != nil || len(baseline.Items) < 3 {
		t.Fatal("missing owner deliveries")
	}
	var cursor *string
	seen := map[string]bool{}
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		path := "/api/v1/deliveries?limit=2&status=pending&destination_id=" + d.ID
		if cursor != nil {
			path += "&cursor=" + *cursor
		}
		var page deliveryPage
		if err = json.Unmarshal(request("GET", path, "", "", token, 200), &page); err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if seen[item.EventID] || item.DestinationID != d.ID {
				t.Fatal("duplicate or foreign item")
			}
			seen[item.EventID] = true
		}
		if pageNumber == 0 && page.NextCursor != nil {
			request("GET", "/api/v1/deliveries?status=pending&destination_id="+d.ID+"&cursor="+*page.NextCursor, "", "", otherToken, 400)
			request("POST", "/api/v1/events", body, storage.NewID(), token, 201)
		}
		cursor = page.NextCursor
		if cursor == nil {
			break
		}
	}
	if cursor != nil || len(seen) != len(baseline.Items) {
		t.Fatal("pagination skipped or repeated events")
	}
	request("GET", "/api/v1/deliveries?destination_id="+d.ID, "", "", otherToken, 404)
	var empty deliveryPage
	if err = json.Unmarshal(request("GET", "/api/v1/deliveries", "", "", otherToken, 200), &empty); err != nil || len(empty.Items) != 0 || empty.Items == nil {
		t.Fatal("owner isolation failed")
	}
	// Reopen the pool and HTTP server: acceptance must survive API restarts.
	db.Close()
	request("GET", "/readyz", "", "", token, 503)
	request("GET", historyPath, "", "", token, 503)
	request("GET", "/livez", "", "", token, 200)
	request("POST", "/api/v1/events", body, key, token, 503)
	server.Close()
	db, err = storage.Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.WithKeyring(keyring)
	server = httptest.NewServer(NewHandler(db, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer server.Close()
	request("GET", "/api/v1/events/"+event.ID, "", "", token, 200)
	request("GET", historyPath, "", "", token, 200)
	request("POST", "/api/v1/events", body, key, token, 200)
	// Rotation keeps identity/idempotency stable; revocation takes effect on the
	// next authenticated request, including through an already running handler.
	second, newToken, err := db.IssueClientToken(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	request("GET", historyPath, "", "", newToken, 200)
	request("GET", historyPath, "", "", token, 200)
	request("POST", "/api/v1/events", body, key, newToken, 200)
	if err = db.RevokeClientToken(ctx, client, client); err != nil {
		t.Fatal(err)
	}
	request("GET", historyPath, "", "", token, 401)
	request("GET", historyPath, "", "", newToken, 200)
	request("GET", historyPath, "", "", otherToken, 404)
	if err = db.RevokeClientToken(ctx, client, second.ID); err != nil {
		t.Fatal(err)
	}
	request("GET", historyPath, "", "", newToken, 401)
	_, recoveredToken, err := db.IssueClientToken(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	request("GET", historyPath, "", "", recoveredToken, 200)
	t.Run("durable rate limit across rotated tokens", func(t *testing.T) { testHTTPRateLimit(t, ctx, db) })

}

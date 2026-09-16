package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BoniLuan/relay/internal/delivery"
)

func TestReceiverSignatureDurabilityAndDeduplication(t *testing.T) {
	secret, err := delivery.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "receipts.json")
	r, err := openReceiver(secret, file)
	if err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	request := func(target *receiver, body string, valid bool, stamp time.Time) int {
		q := httptest.NewRequest("POST", "/demo/hook", bytes.NewBufferString(body))
		q.Header.Set("Relay-Event-ID", id)
		key, _ := base64.RawURLEncoding.DecodeString(secret.Export())
		mac := hmac.New(sha256.New, key)
		timestamp := strconv.FormatInt(stamp.Unix(), 10)
		mac.Write([]byte(timestamp + "." + id + "." + body))
		if valid {
			q.Header.Set("Relay-Signature", "t="+timestamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
		}
		w := httptest.NewRecorder()
		target.ServeHTTP(w, q)
		return w.Code
	}
	if request(r, "{}", false, time.Now()) != 401 || request(r, "{}", true, time.Now().Add(-10*time.Minute)) != 401 {
		t.Fatal("invalid signature accepted")
	}
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("unsigned request persisted")
	}
	if request(r, "{}", true, time.Now()) != 204 {
		t.Fatal("signed request failed")
	}
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := openReceiver(secret, file)
	if err != nil {
		t.Fatal(err)
	}
	if request(restored, "{}", true, time.Now()) != 204 || request(restored, "null", true, time.Now()) != 409 {
		t.Fatal("deduplication failed")
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(original, after) {
		t.Fatal("duplicate changed durable receipt")
	}
	info, _ := os.Stat(file)
	if info.Mode().Perm() != 0600 {
		t.Fatal("receipt permissions")
	}
	broken, _ := openReceiver(secret, filepath.Join(t.TempDir(), "missing", "receipts.json"))
	if request(broken, "{}", true, time.Now()) != 503 || len(broken.receipts) != 0 {
		t.Fatal("storage failure acknowledged")
	}
}

func TestReceiverRejectsUnsafeState(t *testing.T) {
	secret, err := delivery.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	hash := strings.Repeat("a", 64)
	stamp := "2026-09-16T12:00:00Z"
	cases := []string{
		fmt.Sprintf(`{"%s":{"sha256":"%s","received_at":"%s","unexpected":true}}`, id, hash, stamp),
		fmt.Sprintf(`{"not-an-event-id":{"sha256":"%s","received_at":"%s"}}`, hash, stamp),
		fmt.Sprintf(`{"%s":{"sha256":"short","received_at":"%s"}}`, id, stamp),
		fmt.Sprintf(`{"%s":{"sha256":"%s","received_at":"0001-01-01T00:00:00Z"}}`, id, hash),
	}
	for index, raw := range cases {
		file := filepath.Join(t.TempDir(), "receipts.json")
		if err = os.WriteFile(file, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = openReceiver(secret, file); err == nil {
			t.Fatalf("unsafe state case %d accepted", index)
		}
	}

	public := filepath.Join(t.TempDir(), "receipts.json")
	valid := fmt.Sprintf(`{"%s":{"sha256":"%s","received_at":"%s"}}`, id, hash, stamp)
	if err = os.WriteFile(public, []byte(valid), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = openReceiver(secret, public); err == nil {
		t.Fatal("public receiver state accepted")
	}

	large := filepath.Join(t.TempDir(), "receipts.json")
	if err = os.WriteFile(large, bytes.Repeat([]byte("x"), maxReceiverState+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = openReceiver(secret, large); err == nil {
		t.Fatal("oversized receiver state accepted")
	}
}

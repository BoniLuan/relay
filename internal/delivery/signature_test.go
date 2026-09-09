package delivery

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSignatureKnownVectorAndTampering(t *testing.T) {
	// Independently computed using Python hmac/sha256 over the documented bytes.
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	secret, err := ParseSecret(base64.RawURLEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0)
	body := []byte(`{"hello":"world"}`)
	want := "t=1800000000,v1=82dd9fa998966cad876160cf0cb1c8da094733d871d43804343d5252b66eb95c"
	if got := signature(secret, testEventID, body, now); got != want {
		t.Fatalf("signature=%s", got)
	}
	for _, tc := range []struct {
		name, header, id string
		body             []byte
		now              time.Time
		secret           Secret
		valid            bool
	}{
		{"valid", want, testEventID, body, now, secret, true},
		{"boundary past", want, testEventID, body, now.Add(300 * time.Second), secret, true},
		{"boundary future", want, testEventID, body, now.Add(-300 * time.Second), secret, true},
		{"expired", want, testEventID, body, now.Add(301 * time.Second), secret, false},
		{"future", want, testEventID, body, now.Add(-301 * time.Second), secret, false},
		{"body changed", want, testEventID, append(append([]byte{}, body...), ' '), now, secret, false},
		{"ID changed", want, "22222222-2222-4222-8222-222222222222", body, now, secret, false},
		{"wrong key", want, testEventID, body, now, testSecret(t), false},
		{"uninitialized key", want, testEventID, body, now, Secret{}, false},
		{"malformed hex", "t=1800000000,v1=nothex", testEventID, body, now, secret, false},
		{"extra signature", want + ",v1=00", testEventID, body, now, secret, false},
		{"noncanonical timestamp", strings.Replace(want, "t=1", "t=01", 1), testEventID, body, now, secret, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Verify(tc.secret, tc.header, tc.id, tc.body, tc.now); got != tc.valid {
				t.Fatalf("valid=%v", got)
			}
		})
	}
}
func TestSecretExportAndRedaction(t *testing.T) {
	secret := testSecret(t)
	restored, err := ParseSecret(secret.Export())
	if err != nil || restored != secret {
		t.Fatal("secret export did not round trip")
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%x"} {
		if got := fmt.Sprintf(format, secret); got != "[REDACTED]" {
			t.Fatalf("unsafe formatting: %s", format)
		}
	}
	raw, err := json.Marshal(secret)
	if err != nil || string(raw) != "{}" {
		t.Fatal("JSON exposed secret fields")
	}
	for _, raw := range []string{"", "not-a-secret", base64.RawURLEncoding.EncodeToString(make([]byte, 31))} {
		if _, err := ParseSecret(raw); err == nil {
			t.Fatal("invalid secret accepted")
		}
	}
}

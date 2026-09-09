package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthenticatedEncryption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := InitFile(path); err != nil {
		t.Fatal(err)
	}
	k, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("synthetic signing credential")
	aad := []byte("client:destination:1:key-1")
	a, err := k.Seal(k.Active(), plain, aad)
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.Seal(k.Active(), plain, aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) || bytes.Contains(a, plain) {
		t.Fatal("nonce reused or plaintext retained")
	}
	restored, err := k.Open(k.Active(), a, aad)
	if err != nil || !bytes.Equal(restored, plain) {
		t.Fatal("round trip failed")
	}
	for _, index := range []int{0, len(a) - 1} {
		tampered := bytes.Clone(a)
		tampered[index] ^= 1
		if _, err = k.Open(k.Active(), tampered, aad); !errors.Is(err, ErrKeyring) {
			t.Fatal("tamper accepted")
		}
	}
	if _, err = k.Open(k.Active(), a, []byte("other destination")); !errors.Is(err, ErrKeyring) {
		t.Fatal("AAD mismatch accepted")
	}
	otherPath := filepath.Join(t.TempDir(), "other.json")
	if err = InitFile(otherPath); err != nil {
		t.Fatal(err)
	}
	other, err := LoadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Open(k.Active(), a, aad); !errors.Is(err, ErrKeyring) {
		t.Fatal("wrong master key accepted")
	}
	if _, err = k.Open("missing", a, aad); !errors.Is(err, ErrKeyring) {
		t.Fatal("missing key accepted")
	}
	if _, err = k.Open(k.Active(), a[:3], aad); !errors.Is(err, ErrKeyring) {
		t.Fatal("truncated ciphertext accepted")
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if fmt.Sprintf(format, k) != "[REDACTED KEYRING]" {
			t.Fatal("format leaked keyring")
		}
	}
	raw, _ := json.Marshal(k)
	if string(raw) != "{}" {
		t.Fatal("JSON leaked keyring")
	}
}
func TestPrivateFileAndNoOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	if _, err := LoadFile(path); !errors.Is(err, ErrKeyring) {
		t.Fatal("missing file accepted")
	}
	if err := InitFile(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = InitFile(path); !errors.Is(err, ErrKeyring) {
		t.Fatal("existing key replaced")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("existing key changed")
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadFile(path); !errors.Is(err, ErrKeyring) {
		t.Fatal("world-readable key accepted")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(`{"active":"missing","keys":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadFile(path); !errors.Is(err, ErrKeyring) {
		t.Fatal("malformed keyring accepted")
	}
}

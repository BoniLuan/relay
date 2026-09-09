// Package secrets encrypts signing credentials with a file-backed AES-256 keyring.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

var ErrKeyring = errors.New("signing keyring unavailable or invalid")
var keyIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

type fileConfig struct {
	Active string            `json:"active"`
	Keys   map[string]string `json:"keys"`
}
type Keyring struct {
	active string
	keys   map[string]cipher.AEAD
}

func (*Keyring) Format(s fmt.State, verb rune) { io.WriteString(s, "[REDACTED KEYRING]") }
func (k *Keyring) Active() string {
	if k == nil {
		return ""
	}
	return k.active
}
func (k *Keyring) IDs() []string {
	var ids []string
	if k != nil {
		for id := range k.keys {
			ids = append(ids, id)
		}
	}
	return ids
}

// InitFile never overwrites an existing encryption key. A lost key cannot be
// regenerated from a database backup, so it must be backed up independently.
func InitFile(path string) error {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return ErrKeyring
	}
	raw, err := json.Marshal(fileConfig{Active: "key-1", Keys: map[string]string{"key-1": base64.RawURLEncoding.EncodeToString(key[:])}})
	if err != nil {
		return ErrKeyring
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrKeyring
	}
	_, writeErr := f.Write(append(raw, '\n'))
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return ErrKeyring
	}
	return nil
}
func LoadFile(path string) (*Keyring, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrKeyring
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrKeyring
	}
	raw, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(raw) > 16384 {
		return nil, ErrKeyring
	}
	var cfg fileConfig
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF || len(cfg.Keys) == 0 || len(cfg.Keys) > 8 {
		return nil, ErrKeyring
	}
	k := &Keyring{active: cfg.Active, keys: make(map[string]cipher.AEAD)}
	for id, encoded := range cfg.Keys {
		raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
		if !keyIDPattern.MatchString(id) || err != nil || len(raw) != 32 {
			return nil, ErrKeyring
		}
		block, err := aes.NewCipher(raw)
		if err != nil {
			return nil, ErrKeyring
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, ErrKeyring
		}
		k.keys[id] = aead
	}
	if k.keys[k.active] == nil {
		return nil, ErrKeyring
	}
	return k, nil
}
func (k *Keyring) Seal(keyID string, plaintext, aad []byte) ([]byte, error) {
	if k == nil || k.keys[keyID] == nil {
		return nil, ErrKeyring
	}
	aead := k.keys[keyID]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrKeyring
	}
	// The random nonce is stored in front of the authenticated ciphertext.
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}
func (k *Keyring) Open(keyID string, blob, aad []byte) ([]byte, error) {
	if k == nil || k.keys[keyID] == nil {
		return nil, ErrKeyring
	}
	aead := k.keys[keyID]
	if len(blob) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrKeyring
	}
	plain, err := aead.Open(nil, blob[:aead.NonceSize()], blob[aead.NonceSize():], aad)
	if err != nil {
		return nil, ErrKeyring
	}
	return plain, nil
}

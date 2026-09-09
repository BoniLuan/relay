package delivery

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Secret keeps its bytes private and redacts all fmt formatting, including %#v.
// It is not persisted in this milestone. Export is an explicit provisioning step.
type Secret struct {
	key   [32]byte
	valid bool
}

func (Secret) Format(state fmt.State, verb rune) { io.WriteString(state, "[REDACTED]") }
func NewSecret() (Secret, error) {
	var s Secret
	if _, err := rand.Read(s.key[:]); err != nil {
		return Secret{}, err
	}
	s.valid = true
	return s, nil
}
func ParseSecret(encoded string) (Secret, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != 32 {
		return Secret{}, errors.New("invalid signing secret")
	}
	var s Secret
	copy(s.key[:], raw)
	s.valid = true
	return s, nil
}
func (s Secret) Export() string {
	if !s.valid {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(s.key[:])
}
func digest(secret Secret, timestamp, eventID string, body []byte) []byte {
	mac := hmac.New(sha256.New, secret.key[:])
	io.WriteString(mac, timestamp+"."+eventID+".")
	mac.Write(body)
	return mac.Sum(nil)
}
func signature(secret Secret, eventID string, body []byte, now time.Time) string {
	stamp := strconv.FormatInt(now.Unix(), 10)
	return "t=" + stamp + ",v1=" + hex.EncodeToString(digest(secret, stamp, eventID, body))
}

// Verify checks the exact body bytes and event ID, with a five-minute timestamp
// window in both directions. Receivers still need durable event-ID deduplication.
func Verify(secret Secret, header, eventID string, body []byte, now time.Time) bool {
	if !secret.valid || len(header) > 100 {
		return false
	}
	parts := strings.Split(header, ",")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") || !strings.HasPrefix(parts[1], "v1=") {
		return false
	}
	stamp := strings.TrimPrefix(parts[0], "t=")
	ts, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || strconv.FormatInt(ts, 10) != stamp || ts < now.Unix()-300 || ts > now.Unix()+300 {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(parts[1], "v1="))
	return err == nil && hmac.Equal(got, digest(secret, stamp, eventID, body))
}

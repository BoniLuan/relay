package delivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"time"
)

var (
	ErrDestination = errors.New("destination rejected by outbound policy")
	ErrNetwork     = errors.New("outbound connection failed")
	ErrResponse    = errors.New("outbound response exceeded limits or could not be read")
	ErrInput       = errors.New("invalid outbound event or signing secret")
)

const maxResponseBytes = 16 << 10

var eventIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// Sender has no exported policy overrides. Test dependencies stay package-private.
// Each Send makes one attempt; no redirects or implicit application retries.
type Sender struct {
	resolver  resolver
	dial      func(context.Context, string, string) (net.Conn, error)
	tlsConfig *tls.Config
	timeout   time.Duration
	now       func() time.Time
}

func NewSender() *Sender {
	d := &net.Dialer{Timeout: 2 * time.Second}
	return &Sender{resolver: net.DefaultResolver, dial: d.DialContext,
		tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12}, timeout: 5 * time.Second, now: time.Now}
}

// Outcome preserves the status for attempt history even when body reading fails.
// Success requires both a nil error and a 2xx status.
type Outcome struct{ StatusCode int }

func (s *Sender) Send(ctx context.Context, rawURL, eventID string, body []byte, secret Secret) (Outcome, error) {
	if !secret.valid || !eventIDPattern.MatchString(eventID) || len(body) == 0 || len(body) > 64<<10 {
		return Outcome{}, ErrInput
	}
	u, err := destinationURL(rawURL)
	if err != nil {
		return Outcome{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	host := u.Hostname()
	var addresses []netip.Addr
	if addr, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{addr}
	} else {
		// Absolute lookup avoids the machine's DNS search suffixes.
		name := host
		if name[len(name)-1] != '.' {
			name += "."
		}
		addresses, err = s.resolver.LookupNetIP(ctx, "ip", name)
		if err != nil {
			return Outcome{}, ErrNetwork
		}
	}
	if len(addresses) == 0 {
		return Outcome{}, ErrDestination
	}
	for _, addr := range addresses {
		if !publicAddress(addr) {
			return Outcome{}, ErrDestination
		}
	}
	// Pin a validated IP. The URL hostname remains intact for SNI and certificate
	// verification. A later attempt resolves again; it never reuses this connection.
	target := net.JoinHostPort(addresses[0].Unmap().String(), "443")
	transport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		TLSClientConfig: s.tlsConfig.Clone(), TLSHandshakeTimeout: 2 * time.Second,
		ResponseHeaderTimeout: 2 * time.Second, MaxResponseHeaderBytes: 8 << 10,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return s.dial(ctx, "tcp", target)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: s.timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	snapshot := bytes.Clone(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(snapshot))
	if err != nil {
		return Outcome{}, ErrInput
	}
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Relay/1")
	req.Header.Set("Relay-Event-ID", eventID)
	req.Header.Set("Relay-Signature", signature(secret, eventID, snapshot, s.now()))
	response, err := client.Do(req)
	if err != nil {
		return Outcome{}, ErrNetwork
	}
	defer response.Body.Close()
	outcome := Outcome{StatusCode: response.StatusCode}
	// Do not retain or log receiver content. Limit applies even to chunked bodies;
	// automatic decompression is disabled so compressed responses cannot expand it.
	n, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || ctx.Err() != nil || n > maxResponseBytes {
		return outcome, ErrResponse
	}
	return outcome, nil
}

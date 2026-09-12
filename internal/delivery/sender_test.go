package delivery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testEventID = "11111111-1111-4111-8111-111111111111"

type lookupFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f lookupFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}
func testSecret(t *testing.T) Secret {
	t.Helper()
	s, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Only test code maps an already-validated public IP to the loopback fixture.
// Production constructors expose neither this dialer nor a private-address flag.
func fixture(t *testing.T, handler http.Handler) (*Sender, *atomic.Int32) {
	t.Helper()
	fixtureCtx, cancelFixture := context.WithCancel(context.Background())
	server := httptest.NewUnstartedServer(handler)
	server.Config.BaseContext = func(net.Listener) context.Context { return fixtureCtx }
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(func() { cancelFixture(); server.Close() })
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	s := NewSender()
	s.tlsConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	s.resolver = lookupFunc(func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" || host != "example.com." {
			t.Errorf("lookup=%s/%s", network, host)
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	})
	count := new(atomic.Int32)
	s.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		count.Add(1)
		if address != "8.8.8.8:443" {
			return nil, errors.New("dial was not pinned to validated address")
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	return s, count
}

func TestSignedHTTPSAttempt(t *testing.T) {
	secret := testSecret(t)
	now := time.Unix(1800000000, 0)
	body := []byte(`{"amount":9007199254740993}`)
	received := make(chan bool, 1)
	s, calls := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		valid := r.Method == "POST" && r.Host == "example.com" && r.TLS.ServerName == "example.com" &&
			string(got) == string(body) && r.Header.Get("Relay-Event-ID") == testEventID &&
			Verify(secret, r.Header.Get("Relay-Signature"), testEventID, got, now)
		received <- valid
		w.WriteHeader(204)
	}))
	s.now = func() time.Time { return now }
	outcome, err := s.Send(context.Background(), "https://example.com/hook", testEventID, body, secret)
	if err != nil || outcome.StatusCode != 204 || calls.Load() != 1 || !<-received {
		t.Fatalf("attempt=%+v err=%v calls=%d", outcome, err, calls.Load())
	}
}
func TestMixedDNSAnswersNeverDial(t *testing.T) {
	for _, addresses := range [][]netip.Addr{
		nil, {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")},
		{netip.MustParseAddr("2606:4700:4700::1111"), netip.MustParseAddr("fc00::1")},
	} {
		s := NewSender()
		s.resolver = lookupFunc(func(context.Context, string, string) ([]netip.Addr, error) { return addresses, nil })
		s.dial = func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("unsafe DNS answer reached dialer")
			return nil, nil
		}
		if _, err := s.Send(context.Background(), "https://example.com", testEventID, []byte(`null`), testSecret(t)); !errors.Is(err, ErrDestination) {
			t.Fatalf("err=%v", err)
		}
	}
}
func TestDNSRebindingBetweenAttempts(t *testing.T) {
	s, calls := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	lookups := 0
	s.resolver = lookupFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		if lookups == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	secret := testSecret(t)
	if _, err := s.Send(context.Background(), "https://example.com", testEventID, []byte(`null`), secret); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "https://example.com", testEventID, []byte(`null`), secret); !errors.Is(err, ErrDestination) {
		t.Fatalf("rebinding accepted: %v", err)
	}
	if calls.Load() != 1 || lookups != 2 {
		t.Fatalf("calls=%d lookups=%d", calls.Load(), lookups)
	}
}
func TestRedirectsAndReceiverFailuresAreNotRetried(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308, 429, 500} {
		s, calls := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://127.0.0.1/private")
			w.WriteHeader(status)
		}))
		outcome, err := s.Send(context.Background(), "https://example.com", testEventID, []byte(`null`), testSecret(t))
		if err != nil || outcome.StatusCode != status || calls.Load() != 1 {
			t.Fatalf("status=%d outcome=%+v err=%v calls=%d", status, outcome, err, calls.Load())
		}
	}
}
func TestResponseLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    error
	}{
		{"body", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("x", maxResponseBytes+1))) }, ErrResponse},
		{"chunked body", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
		}, ErrResponse},
		{"headers", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Huge", strings.Repeat("x", 32<<10))
			w.WriteHeader(200)
		}, ErrNetwork},
		{"slow headers", func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); <-r.Context().Done() }, ErrNetwork},
		{"slow body", func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}, ErrResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fixture(t, tc.handler)
			s.timeout = 300 * time.Millisecond
			_, err := s.Send(context.Background(), "https://example.com", testEventID, []byte(`null`), testSecret(t))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
		})
	}
}
func TestTLSVerificationCannotBeBypassed(t *testing.T) {
	s, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS reached handler") }))
	s.tlsConfig = NewSender().tlsConfig
	if _, err := s.Send(context.Background(), "https://example.com", testEventID, []byte(`null`), testSecret(t)); !errors.Is(err, ErrNetwork) {
		t.Fatalf("untrusted TLS err=%v", err)
	}
}
func TestDNSDeadlineAndErrorRedaction(t *testing.T) {
	s := NewSender()
	s.timeout = 20 * time.Millisecond
	s.resolver = lookupFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, errors.New("private DNS detail")
	})
	_, err := s.Send(context.Background(), "https://example.com/?secret=private", testEventID, []byte(`null`), testSecret(t))
	if !errors.Is(err, ErrNetwork) || strings.Contains(err.Error(), "private") {
		t.Fatalf("err=%v", err)
	}
}
func TestEnvironmentProxyIgnored(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	s, calls := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	outcome, err := s.Send(context.Background(), "https://example.com", testEventID, []byte(`null`), testSecret(t))
	if err != nil || outcome.StatusCode != 204 || calls.Load() != 1 {
		t.Fatalf("proxy affected attempt: %+v %v", outcome, err)
	}
}

// NewFixtureSender is linked only into tests. External integration tests can
// exercise the real sender without adding production policy bypasses.
func NewFixtureSender(t *testing.T, handler http.Handler) (*Sender, *atomic.Int32) {
	return fixture(t, handler)
}

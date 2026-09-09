package delivery

import (
	"net/netip"
	"testing"
)

func TestAddressPolicy(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.0", "10.1.2.3", "100.100.100.200", "127.0.0.1", "169.254.169.254",
		"172.16.0.1", "172.31.255.255", "192.168.1.1", "192.0.0.9", "192.0.2.1",
		"192.31.196.1", "192.52.193.1", "192.88.99.1", "192.175.48.1",
		"198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::", "::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "fc00::1", "fe80::1",
		"fe80::1%eth0", "ff02::1", "64:ff9b::a00:1", "64:ff9b:1::1", "100::1",
		"2001::1", "2001:db8::1", "2002:7f00:1::1", "2620:4f:8000::1", "3fff::1", "5f00::1",
	} {
		t.Run(raw, func(t *testing.T) {
			if publicAddress(netip.MustParseAddr(raw)) {
				t.Fatal("unsafe address accepted")
			}
		})
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "172.15.255.255", "172.32.0.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !publicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("public address rejected: %s", raw)
		}
	}
	if publicAddress(netip.Addr{}) {
		t.Fatal("invalid address accepted")
	}
}
func TestDestinationPolicy(t *testing.T) {
	for _, raw := range []string{
		"http://example.com", "https://user:password@example.com", "https://example.com/#fragment",
		"https://example.com/#", "https://example.com:8443/", "https://example.com:/", "https://localhost",
		"https://127.0.0.1", "https://[::1]", "https://[fe80::1%25eth0]", "https://2130706433",
		"https://0177.0.0.1", "https://127.1", "https://bad_name.example", "https://-bad.example",
		"https://example..com", "https://éxample.com", "https:///example.com",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := destinationURL(raw); err == nil {
				t.Fatal("unsafe URL accepted")
			}
		})
	}
	for _, raw := range []string{"https://example.com/path?q=1", "https://example.com:443/", "https://example.com./", "https://8.8.8.8/", "https://[2606:4700:4700::1111]/", "https://xn--xample-9ua.com/"} {
		if _, err := destinationURL(raw); err != nil {
			t.Errorf("URL rejected: %s", raw)
		}
	}
}

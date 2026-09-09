// Package delivery provides tested outbound primitives. No API or worker calls
// them yet; durable secret management and queue processing are separate work.
package delivery

import (
	"net/netip"
	"net/url"
	"strings"
)

// Conservative policy: exclude all IANA special-purpose IPv4 blocks, multicast,
// and IPv6 outside ordinary global unicast. See docs/DELIVERY_SECURITY.md.
var deniedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"), netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"),
}
var globalIPv6 = netip.MustParsePrefix("2000::/3")

func publicAddress(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || (addr.Is6() && !globalIPv6.Contains(addr)) {
		return false
	}
	for _, prefix := range deniedNetworks {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func destinationURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.Contains(raw, "#") || u.Opaque != "" || (u.Port() != "" && u.Port() != "443") {
		return nil, ErrDestination
	}
	host := u.Hostname()
	if strings.Contains(host, "%") || strings.HasSuffix(u.Host, ":") {
		return nil, ErrDestination
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !publicAddress(addr) {
			return nil, ErrDestination
		}
	} else {
		// ASCII DNS names only; IDNs must be submitted as punycode. Reject ambiguous
		// numeric hosts and single-label names that could use local search domains.
		name := strings.TrimSuffix(host, ".")
		if len(name) > 253 || !strings.Contains(name, ".") {
			return nil, ErrDestination
		}
		hasLetter := false
		for _, label := range strings.Split(name, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, ErrDestination
			}
			for _, c := range label {
				switch {
				case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
					hasLetter = true
				case c >= '0' && c <= '9', c == '-':
				default:
					return nil, ErrDestination
				}
			}
		}
		if !hasLetter {
			return nil, ErrDestination
		}
	}
	return u, nil
}

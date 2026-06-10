package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// MaxForwardedForHops limits the amount of X-Forwarded-For entries inspected
// per request. The trusted chain is walked from the right, closest to the app.
const MaxForwardedForHops = 32

// ParsePrefixes parses trusted proxy CIDR strings once at application startup.
func ParsePrefixes(cidrs []string) ([]netip.Prefix, error) {
	if len(cidrs) == 0 {
		return nil, nil
	}

	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		value := strings.TrimSpace(cidr)
		if value == "" {
			return nil, fmt.Errorf("empty trusted proxy CIDR")
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("parse trusted proxy CIDR %q: %w", value, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}

	return prefixes, nil
}

// Trusted reports whether addr is contained in any trusted proxy prefix.
func Trusted(prefixes []netip.Prefix, addr netip.Addr) bool {
	if len(prefixes) == 0 || !addr.IsValid() {
		return false
	}

	addr = addr.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// RemoteAddrIP parses an http.Request RemoteAddr and returns an IP-only string
// for valid addresses. If RemoteAddr is unparseable, fallback is the trimmed
// RemoteAddr value and ok is false.
func RemoteAddrIP(remoteAddr string) (addr netip.Addr, fallback string, ok bool) {
	fallback = strings.TrimSpace(remoteAddr)
	if fallback == "" {
		return netip.Addr{}, "", false
	}

	if host, _, err := net.SplitHostPort(fallback); err == nil {
		if addr, ok := parseAddr(host); ok {
			return addr, addr.String(), true
		}
		return netip.Addr{}, fallback, false
	}

	if addr, ok := parseAddr(fallback); ok {
		return addr, addr.String(), true
	}

	return netip.Addr{}, fallback, false
}

// Scheme reports the original client scheme when the immediate peer is
// trusted. The RFC 7239 Forwarded header takes precedence over the legacy
// X-Forwarded-* headers.
func Scheme(r *http.Request, trustedProxies []netip.Prefix) string {
	if r == nil {
		return "http"
	}
	if r.TLS != nil {
		return "https"
	}

	peer, _, ok := RemoteAddrIP(r.RemoteAddr)
	if !ok || !Trusted(trustedProxies, peer) {
		return "http"
	}

	if proto := forwardedProto(r.Header.Values("Forwarded")); proto != "" {
		return proto
	}

	for _, header := range []string{"X-Forwarded-Proto", "X-Forwarded-Ssl", "Front-End-Https"} {
		value := strings.ToLower(strings.TrimSpace(r.Header.Get(header)))
		switch value {
		case "https", "on":
			return "https"
		case "http", "off":
			return "http"
		}
	}

	return "http"
}

// RealIP reports the original client IP when the immediate peer is trusted.
// The RFC 7239 Forwarded header (for= parameters) takes precedence over the
// legacy X-Forwarded-For and X-Real-IP headers; all chain sources are walked
// with the same right-to-left trusted-hop semantics.
func RealIP(r *http.Request, trustedProxies []netip.Prefix) string {
	if r == nil {
		return ""
	}

	peer, fallback, ok := RemoteAddrIP(r.RemoteAddr)
	if !ok || !Trusted(trustedProxies, peer) {
		return fallback
	}

	if ip, ok := chainRealIP(forwardedFor(r.Header.Values("Forwarded")), trustedProxies); ok {
		return ip.String()
	}

	if ip, ok := chainRealIP(splitForwardedFor(r.Header.Get("X-Forwarded-For")), trustedProxies); ok {
		return ip.String()
	}

	if ip, ok := parseAddr(r.Header.Get("X-Real-IP")); ok {
		return ip.String()
	}

	return fallback
}

func splitForwardedFor(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.Split(value, ",")
}

// chainRealIP walks a proxy chain (leftmost = client) from the right,
// skipping trusted hops, and returns the first untrusted address — the
// client as seen by the outermost trusted proxy. If every valid hop is
// trusted, the leftmost valid one is returned. At most
// MaxForwardedForHops entries are inspected.
func chainRealIP(chain []string, trustedProxies []netip.Prefix) (netip.Addr, bool) {
	if len(chain) == 0 {
		return netip.Addr{}, false
	}

	start := len(chain) - 1
	end := 0
	if len(chain) > MaxForwardedForHops {
		end = len(chain) - MaxForwardedForHops
	}

	var leftmostValid netip.Addr
	for i := start; i >= end; i-- {
		ip, ok := parseAddr(chain[i])
		if !ok {
			continue
		}
		leftmostValid = ip
		if !Trusted(trustedProxies, ip) {
			return ip, true
		}
	}

	if leftmostValid.IsValid() {
		return leftmostValid, true
	}
	return netip.Addr{}, false
}

// forwardedFor extracts the for= node identifiers from RFC 7239 Forwarded
// header values, preserving element order (leftmost = client). Obfuscated
// ("_hidden") and "unknown" identifiers are kept as raw strings — the chain
// walk skips them like any other non-IP hop.
func forwardedFor(values []string) []string {
	var nodes []string
	for _, value := range values {
		for elem := range strings.SplitSeq(value, ",") {
			if node, ok := forwardedParam(elem, "for"); ok {
				nodes = append(nodes, node)
			}
		}
	}
	return nodes
}

// forwardedProto returns "https" or "http" from the first RFC 7239 element
// carrying a recognized proto= parameter, or "" when absent.
func forwardedProto(values []string) string {
	for _, value := range values {
		for elem := range strings.SplitSeq(value, ",") {
			proto, ok := forwardedParam(elem, "proto")
			if !ok {
				continue
			}
			switch strings.ToLower(proto) {
			case "https":
				return "https"
			case "http":
				return "http"
			}
		}
	}
	return ""
}

// forwardedParam extracts one parameter from a single RFC 7239 element
// (semicolon-separated key=value pairs). The value is unquoted and, for
// node identifiers, stripped of an optional port.
func forwardedParam(elem, key string) (string, bool) {
	for param := range strings.SplitSeq(elem, ";") {
		k, v, ok := strings.Cut(param, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		// Node identifiers may carry a port: "[2001:db8::17]:4711",
		// "192.0.2.60:8080". parseAddr handles the portless forms.
		if host, _, err := net.SplitHostPort(v); err == nil {
			v = host
		}
		return v, v != ""
	}
	return "", false
}

func parseAddr(value string) (netip.Addr, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(strings.TrimSuffix(value, "]"), "[")
	if value == "" {
		return netip.Addr{}, false
	}

	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

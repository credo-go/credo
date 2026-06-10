package proxy

import (
	"crypto/tls"
	"net/http"
	"strings"
	"testing"
)

func TestParsePrefixes(t *testing.T) {
	prefixes, err := ParsePrefixes([]string{"10.0.0.0/8", " 127.0.0.1/32 "})
	if err != nil {
		t.Fatalf("ParsePrefixes() error: %v", err)
	}
	if len(prefixes) != 2 {
		t.Fatalf("len(prefixes) = %d, want 2", len(prefixes))
	}

	if _, err := ParsePrefixes([]string{"not-cidr"}); err == nil {
		t.Fatal("expected invalid CIDR error")
	}
	if _, err := ParsePrefixes([]string{""}); err == nil {
		t.Fatal("expected empty CIDR error")
	}
}

func TestRemoteAddrIP(t *testing.T) {
	tests := []struct {
		name         string
		remoteAddr   string
		wantFallback string
		wantOK       bool
	}{
		{name: "ipv4 host port", remoteAddr: "203.0.113.1:1234", wantFallback: "203.0.113.1", wantOK: true},
		{name: "ipv6 host port", remoteAddr: "[::1]:1234", wantFallback: "::1", wantOK: true},
		{name: "raw ipv6", remoteAddr: "::1", wantFallback: "::1", wantOK: true},
		{name: "invalid", remoteAddr: "client-name", wantFallback: "client-name", wantOK: false},
		{name: "empty", remoteAddr: "", wantFallback: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gotFallback, gotOK := RemoteAddrIP(tt.remoteAddr)
			if gotFallback != tt.wantFallback {
				t.Fatalf("fallback = %q, want %q", gotFallback, tt.wantFallback)
			}
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
		})
	}
}

func TestScheme(t *testing.T) {
	trusted, err := ParsePrefixes([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		remoteAddr string
		tls        bool
		headers    map[string]string
		trusted    bool
		want       string
	}{
		{name: "tls direct", remoteAddr: "203.0.113.1:1", tls: true, want: "https"},
		{name: "default deny", remoteAddr: "203.0.113.1:1", headers: map[string]string{"X-Forwarded-Proto": "https"}, want: "http"},
		{name: "trusted forwarded proto", remoteAddr: "10.0.0.1:1", headers: map[string]string{"X-Forwarded-Proto": "https"}, trusted: true, want: "https"},
		{name: "trusted forwarded ssl", remoteAddr: "10.0.0.1:1", headers: map[string]string{"X-Forwarded-Ssl": "on"}, trusted: true, want: "https"},
		{name: "invalid header fallback", remoteAddr: "10.0.0.1:1", headers: map[string]string{"X-Forwarded-Proto": "//evil.com"}, trusted: true, want: "http"},
		{name: "rfc7239 proto https", remoteAddr: "10.0.0.1:1", headers: map[string]string{"Forwarded": "for=1.1.1.1;proto=https"}, trusted: true, want: "https"},
		{name: "rfc7239 proto quoted", remoteAddr: "10.0.0.1:1", headers: map[string]string{"Forwarded": `proto="https"`}, trusted: true, want: "https"},
		{name: "rfc7239 precedence over xfp", remoteAddr: "10.0.0.1:1", headers: map[string]string{"Forwarded": "proto=http", "X-Forwarded-Proto": "https"}, trusted: true, want: "http"},
		{name: "rfc7239 unknown proto falls through", remoteAddr: "10.0.0.1:1", headers: map[string]string{"Forwarded": "proto=ftp", "X-Forwarded-Proto": "https"}, trusted: true, want: "https"},
		{name: "rfc7239 ignored when untrusted", remoteAddr: "203.0.113.1:1", headers: map[string]string{"Forwarded": "proto=https"}, want: "http"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, "/", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.RemoteAddr = tt.remoteAddr
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}

			got := Scheme(r, nil)
			if tt.trusted {
				got = Scheme(r, trusted)
			}
			if got != tt.want {
				t.Fatalf("Scheme() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRealIP(t *testing.T) {
	trusted, err := ParsePrefixes([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		trusted    bool
		want       string
	}{
		{name: "default deny", remoteAddr: "203.0.113.1:1", headers: map[string]string{"X-Forwarded-For": "1.1.1.1"}, want: "203.0.113.1"},
		{name: "trusted xff single", remoteAddr: "10.0.0.1:1", headers: map[string]string{"X-Forwarded-For": "1.1.1.1"}, trusted: true, want: "1.1.1.1"},
		{name: "trusted xff chain", remoteAddr: "10.0.0.1:1", headers: map[string]string{"X-Forwarded-For": "1.1.1.1, 10.0.0.2"}, trusted: true, want: "1.1.1.1"},
		{name: "all xff hops trusted uses leftmost valid", remoteAddr: "10.0.0.9:1", headers: map[string]string{"X-Forwarded-For": "10.0.0.5, 10.0.0.3, 10.0.0.1"}, trusted: true, want: "10.0.0.5"},
		{name: "hop limit clamps long xff", remoteAddr: "10.0.0.9:1", headers: map[string]string{"X-Forwarded-For": longForwardedForChain()}, trusted: true, want: "10.0.0.1"},
		{name: "ipv4 mapped ipv6 xff is unmapped", remoteAddr: "10.0.0.1:1", headers: map[string]string{"X-Forwarded-For": "::ffff:1.1.1.1"}, trusted: true, want: "1.1.1.1"},
		{name: "invalid xff entry skipped", remoteAddr: "10.0.0.1:1", headers: map[string]string{"X-Forwarded-For": "evil, 10.0.0.2"}, trusted: true, want: "10.0.0.2"},
		{name: "xri fallback", remoteAddr: "10.0.0.1:1", headers: map[string]string{"X-Real-IP": "198.51.100.20"}, trusted: true, want: "198.51.100.20"},
		{name: "empty xff and xri falls back to trusted peer", remoteAddr: "10.0.0.1:1", headers: map[string]string{"X-Forwarded-For": " ", "X-Real-IP": " "}, trusted: true, want: "10.0.0.1"},
		{name: "xri ignored when untrusted", remoteAddr: "203.0.113.1:1", headers: map[string]string{"X-Real-IP": "198.51.100.20"}, want: "203.0.113.1"},
		{name: "unparseable remote", remoteAddr: "client-name", headers: map[string]string{"X-Forwarded-For": "1.1.1.1"}, trusted: true, want: "client-name"},
		{name: "rfc7239 single", remoteAddr: "10.0.0.1:1", headers: map[string]string{"Forwarded": "for=1.1.1.1;proto=https"}, trusted: true, want: "1.1.1.1"},
		{name: "rfc7239 chain skips trusted hop", remoteAddr: "10.0.0.1:1", headers: map[string]string{"Forwarded": "for=1.1.1.1, for=10.0.0.2"}, trusted: true, want: "1.1.1.1"},
		{name: "rfc7239 quoted ipv6 with port", remoteAddr: "10.0.0.1:1", headers: map[string]string{"Forwarded": `for="[2001:db8::17]:4711"`}, trusted: true, want: "2001:db8::17"},
		{name: "rfc7239 ipv4 with port", remoteAddr: "10.0.0.1:1", headers: map[string]string{"Forwarded": `for="192.0.2.60:8080"`}, trusted: true, want: "192.0.2.60"},
		{name: "rfc7239 obfuscated hop skipped", remoteAddr: "10.0.0.1:1", headers: map[string]string{"Forwarded": "for=1.1.1.1, for=_internal"}, trusted: true, want: "1.1.1.1"},
		{name: "rfc7239 precedence over xff", remoteAddr: "10.0.0.1:1", headers: map[string]string{"Forwarded": "for=2.2.2.2", "X-Forwarded-For": "1.1.1.1"}, trusted: true, want: "2.2.2.2"},
		{name: "rfc7239 unparseable falls back to xff", remoteAddr: "10.0.0.1:1", headers: map[string]string{"Forwarded": "for=unknown", "X-Forwarded-For": "1.1.1.1"}, trusted: true, want: "1.1.1.1"},
		{name: "rfc7239 ignored when untrusted", remoteAddr: "203.0.113.1:1", headers: map[string]string{"Forwarded": "for=1.1.1.1"}, want: "203.0.113.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, "/", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.RemoteAddr = tt.remoteAddr
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}

			got := RealIP(r, nil)
			if tt.trusted {
				got = RealIP(r, trusted)
			}
			if got != tt.want {
				t.Fatalf("RealIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func longForwardedForChain() string {
	hops := make([]string, MaxForwardedForHops+1)
	hops[0] = "203.0.113.10"
	for i := 1; i < len(hops); i++ {
		hops[i] = "10.0.0.1"
	}
	return strings.Join(hops, ", ")
}

package origin

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Origin
	}{
		{
			name: "HTTPS case and default port",
			raw:  "HTTPS://APP.Example.COM:443",
			want: Origin{Scheme: "https", Host: "app.example.com", Port: 443},
		},
		{
			name: "HTTP default port",
			raw:  "http://app.example.com",
			want: Origin{Scheme: "http", Host: "app.example.com", Port: 80},
		},
		{
			name: "custom port",
			raw:  "https://app.example.com:8443",
			want: Origin{Scheme: "https", Host: "app.example.com", Port: 8443},
		},
		{
			name: "bracketed IPv6 canonicalized",
			raw:  "https://[2001:0db8:0:0::1]",
			want: Origin{Scheme: "https", Host: "2001:db8::1", Port: 443},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("origin = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseRejectsMalformedAndUnsafeInputs(t *testing.T) {
	invalid := []string{
		"", "null", " null", "ftp://example.com", "https:example.com",
		"https://", "https://user@example.com", "https://example.com/",
		"https://example.com/path", "https://example.com?query",
		"https://example.com#fragment", "https://example.com:",
		"https://example.com:0", "https://example.com:65536",
		"https://münich.example", "https://example_com",
		"https://-bad.example", "https://bad-.example",
		"https://*.example.com", // wildcard is a pattern, not an origin
	}
	for _, raw := range invalid {
		t.Run(raw, func(t *testing.T) {
			if _, err := Parse(raw); err == nil {
				t.Fatalf("Parse(%q) succeeded", raw)
			}
		})
	}
	if _, err := Parse("https://" + strings.Repeat("a", MaxLength)); err == nil {
		t.Fatal("oversized origin succeeded")
	}
}

func TestParsePatternWildcardGrammar(t *testing.T) {
	valid, err := ParsePattern("https://*.Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !valid.Wildcard || valid.Origin.Host != "example.com" {
		t.Errorf("wildcard = %+v", valid)
	}
	exact, err := ParsePattern("https://app.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if exact.Wildcard {
		t.Errorf("exact pattern reported wildcard: %+v", exact)
	}
	invalid := []string{
		"https://*example.com",
		"https://a.*.example.com",
		"https://**.example.com",
		"https://*.com",
		"https://*.127.0.0.1",
		"https://[*.example.com]",
	}
	for _, raw := range invalid {
		if _, err := ParsePattern(raw); err == nil {
			t.Errorf("ParsePattern(%q) succeeded", raw)
		}
	}
}

func TestPatternMatches(t *testing.T) {
	mustPattern := func(raw string) Pattern {
		t.Helper()
		p, err := ParsePattern(raw)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	mustOrigin := func(raw string) Origin {
		t.Helper()
		o, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	tests := []struct {
		pattern string
		origin  string
		want    bool
	}{
		{"https://app.example.com", "https://APP.example.com:443", true},
		{"https://app.example.com", "http://app.example.com", false},
		{"https://app.example.com", "https://app.example.com:8443", false},
		{"https://*.partner.example", "https://tenant.partner.example", true},
		{"https://*.partner.example", "https://partner.example", false},
		{"https://*.partner.example", "https://a.b.partner.example", false},
		{"https://*.partner.example", "http://tenant.partner.example", false},
		{"https://[2001:db8::1]:8443", "https://[2001:0db8::1]:8443", true},
	}
	for _, tc := range tests {
		t.Run(tc.pattern+" vs "+tc.origin, func(t *testing.T) {
			if got := mustPattern(tc.pattern).Matches(mustOrigin(tc.origin)); got != tc.want {
				t.Errorf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOriginString(t *testing.T) {
	for raw, want := range map[string]string{
		"HTTPS://APP.Example.COM:443":  "https://app.example.com",
		"http://app.example.com:80":    "http://app.example.com",
		"https://app.example.com:8443": "https://app.example.com:8443",
		"https://[2001:0db8::1]:8443":  "https://[2001:db8::1]:8443",
		"http://[::1]":                 "http://[::1]",
	} {
		o, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := o.String(); got != want {
			t.Errorf("Parse(%q).String() = %q, want %q", raw, got, want)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"https://example.com",
		"HTTP://EXAMPLE.COM:80",
		"https://[2001:db8::1]:8443",
		"null",
		"https://example.com/path",
		"https://münich.example",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		o, err := Parse(raw)
		if err != nil {
			return
		}
		if o.Scheme != "http" && o.Scheme != "https" {
			t.Fatalf("accepted scheme %q", o.Scheme)
		}
		if o.Host == "" || !isASCII(o.Host) || o.Port == 0 {
			t.Fatalf("accepted non-canonical origin: %+v", o)
		}
		if o.Host != strings.ToLower(o.Host) {
			t.Fatalf("accepted mixed-case host %q", o.Host)
		}
	})
}

package websocket

import (
	"strings"
	"testing"
)

func TestParseCanonicalOrigin(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want canonicalOrigin
	}{
		{
			name: "HTTPS case and default port",
			raw:  "HTTPS://APP.Example.COM:443",
			want: canonicalOrigin{scheme: "https", host: "app.example.com", port: 443},
		},
		{
			name: "HTTP default port",
			raw:  "http://app.example.com",
			want: canonicalOrigin{scheme: "http", host: "app.example.com", port: 80},
		},
		{
			name: "custom port",
			raw:  "https://app.example.com:8443",
			want: canonicalOrigin{scheme: "https", host: "app.example.com", port: 8443},
		},
		{
			name: "bracketed IPv6 canonicalized",
			raw:  "https://[2001:0db8:0:0::1]",
			want: canonicalOrigin{scheme: "https", host: "2001:db8::1", port: 443},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCanonicalOrigin(tc.raw, false)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("origin = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseOriginRejectsMalformedAndUnsafeInputs(t *testing.T) {
	invalid := []string{
		"", "null", " null", "ftp://example.com", "https:example.com",
		"https://", "https://user@example.com", "https://example.com/",
		"https://example.com/path", "https://example.com?query",
		"https://example.com#fragment", "https://example.com:",
		"https://example.com:0", "https://example.com:65536",
		"https://münich.example", "https://example_com",
		"https://-bad.example", "https://bad-.example",
	}
	for _, raw := range invalid {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseCanonicalOrigin(raw, false); err == nil {
				t.Fatalf("parseCanonicalOrigin(%q) succeeded", raw)
			}
		})
	}
	if _, err := parseCanonicalOrigin("https://"+strings.Repeat("a", maxOriginLength), false); err == nil {
		t.Fatal("oversized origin succeeded")
	}
}

func TestParseOriginPatternWildcardGrammar(t *testing.T) {
	valid, err := parseOriginPattern("https://*.Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !valid.wildcard || valid.origin.host != "example.com" {
		t.Errorf("wildcard = %+v", valid)
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
		if _, err := parseOriginPattern(raw); err == nil {
			t.Errorf("parseOriginPattern(%q) succeeded", raw)
		}
	}
}

func TestOriginPolicyAuthorization(t *testing.T) {
	policy, err := compileOriginPolicy([]string{
		"https://app.example.com",
		"https://*.partner.example",
		"https://[2001:db8::1]:8443",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		origins       []string
		requestScheme string
		requestHost   string
		wantErr       bool
	}{
		{name: "missing non-browser", requestScheme: "https", requestHost: "chat.example.com"},
		{
			name:          "same origin normalized",
			origins:       []string{"https://CHAT.example.com:443"},
			requestScheme: "https", requestHost: "chat.example.com",
		},
		{
			name:          "exact cross origin",
			origins:       []string{"https://app.example.com"},
			requestScheme: "https", requestHost: "chat.example.com",
		},
		{
			name:          "one-label wildcard",
			origins:       []string{"https://tenant.partner.example"},
			requestScheme: "https", requestHost: "chat.example.com",
		},
		{
			name:          "IPv6 exact",
			origins:       []string{"https://[2001:0db8::1]:8443"},
			requestScheme: "https", requestHost: "chat.example.com",
		},
		{
			name: "scheme mismatch", origins: []string{"http://chat.example.com"},
			requestScheme: "https", requestHost: "chat.example.com", wantErr: true,
		},
		{
			name: "wildcard apex", origins: []string{"https://partner.example"},
			requestScheme: "https", requestHost: "chat.example.com", wantErr: true,
		},
		{
			name: "wildcard multiple labels", origins: []string{"https://a.b.partner.example"},
			requestScheme: "https", requestHost: "chat.example.com", wantErr: true,
		},
		{
			name: "multiple headers", origins: []string{"https://app.example.com", "https://app.example.com"},
			requestScheme: "https", requestHost: "chat.example.com", wantErr: true,
		},
		{
			name: "null", origins: []string{"null"},
			requestScheme: "https", requestHost: "chat.example.com", wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := policy.authorize(tc.origins, tc.requestScheme, tc.requestHost)
			if (err != nil) != tc.wantErr {
				t.Errorf("authorize() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}

	insecure, err := compileOriginPolicy(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := insecure.authorize([]string{"null", "https://evil.example"}, "bad", "bad"); err != nil {
		t.Errorf("explicit insecure bypass rejected Origin: %v", err)
	}
}

func FuzzParseCanonicalOrigin(f *testing.F) {
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
		origin, err := parseCanonicalOrigin(raw, false)
		if err != nil {
			return
		}
		if origin.scheme != "http" && origin.scheme != "https" {
			t.Fatalf("accepted scheme %q", origin.scheme)
		}
		if origin.host == "" || !isASCII(origin.host) || origin.port == 0 {
			t.Fatalf("accepted non-canonical origin: %+v", origin)
		}
		if origin.host != strings.ToLower(origin.host) {
			t.Fatalf("accepted mixed-case host %q", origin.host)
		}
	})
}

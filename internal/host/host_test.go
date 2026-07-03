package host

import "testing"

func TestNormalizeRequest(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple", "example.com", "example.com"},
		{"port strip", "example.com:8080", "example.com"},
		{"lowercase", "Example.COM", "example.com"},
		{"trailing dot", "example.com.", "example.com"},
		{"fqdn with port", "Example.COM.:443", "example.com"},
		{"ipv6 with port strips brackets and port", "[2001:DB8::1]:8080", "2001:db8::1"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeRequest(tt.input); got != tt.want {
				t.Errorf("NormalizeRequest(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPatternHasPort(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    bool
	}{
		{"no port", "example.com", false},
		{"with port", "example.com:8080", true},
		{"regex colon", "{id:[0-9]+}.example.com", false},
		{"port after regex", "{id:[0-9]+}.example.com:443", true},
		{"nested braces", "{id:[a-z]{2,4}}.example.com", false},
		// IPv6 literals: colons inside brackets are part of the address, not a
		// port separator (regression: brackets were not tracked).
		{"ipv6 literal without port", "[::1]", false},
		{"ipv6 literal with port", "[2001:db8::1]:8080", true},
		{"loopback ipv6 with port", "[::1]:9090", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PatternHasPort(tt.pattern); got != tt.want {
				t.Errorf("PatternHasPort(%q) = %v, want %v", tt.pattern, got, tt.want)
			}
		})
	}
}

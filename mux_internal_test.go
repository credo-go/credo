package credo

import (
	"net/http"
	"testing"
)

func TestJoinPath(t *testing.T) {
	tests := []struct {
		prefix  string
		pattern string
		want    string
	}{
		{"", "/users", "/users"},
		{"/api", "", "/api"},
		{"/api", "/users", "/api/users"},
		{"/api/", "/users", "/api/users"},
		{"/api", "users", "/api/users"},
		{"/api/", "users", "/api/users"},
		{"/", "/users", "/users"},
		{"/", "/", "/"},
		{"", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.prefix+"+"+tt.pattern, func(t *testing.T) {
			got := joinPath(tt.prefix, tt.pattern)
			if got != tt.want {
				t.Errorf("joinPath(%q, %q) = %q, want %q", tt.prefix, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestRouteStore_UpsertDeduplicates(t *testing.T) {
	rs := &routeStore{}

	// Simulate auto-HEAD then explicit HEAD: same method+pattern, so the later
	// registration's handler must win and the entry must not duplicate.
	auto := &routeHandler{route: &Route{method: "HEAD", pattern: "/users"}}
	explicit := &routeHandler{route: &Route{method: "HEAD", pattern: "/users"}}
	rs.add("HEAD", "/users", auto)
	rs.add("HEAD", "/users", explicit)

	entries := rs.snapshot()

	count := 0
	var got *routeHandler
	for _, e := range entries {
		if e.method == "HEAD" && e.pattern == "/users" {
			count++
			got = e.rh
		}
	}
	if count != 1 {
		t.Errorf("HEAD /users entry count = %d, want 1", count)
	}
	if got != explicit {
		t.Error("upsert did not keep the later (explicit) routeHandler")
	}
}

func TestBuildServer_IPv6(t *testing.T) {
	cfg := serverConfig{
		Host: "::1",
		Port: 8080,
	}
	srv := buildServer(cfg, http.DefaultServeMux)
	if srv.Addr != "[::1]:8080" {
		t.Errorf("Addr = %q, want %q", srv.Addr, "[::1]:8080")
	}
}

func TestBuildServer_IPv4(t *testing.T) {
	cfg := serverConfig{
		Host: "127.0.0.1",
		Port: 3000,
	}
	srv := buildServer(cfg, http.DefaultServeMux)
	if srv.Addr != "127.0.0.1:3000" {
		t.Errorf("Addr = %q, want %q", srv.Addr, "127.0.0.1:3000")
	}
}

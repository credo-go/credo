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

	// Simulate auto-HEAD then explicit HEAD
	rs.add(RouteInfo{Method: "HEAD", Pattern: "/users"})
	rs.add(RouteInfo{Method: "HEAD", Pattern: "/users"})

	routes := rs.snapshot()

	headCount := 0
	for _, ri := range routes {
		if ri.Method == "HEAD" && ri.Pattern == "/users" {
			headCount++
		}
	}
	if headCount != 1 {
		t.Errorf("HEAD /users count = %d, want 1", headCount)
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

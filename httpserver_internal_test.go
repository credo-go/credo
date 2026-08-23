package credo

import (
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestBuildServer_MaxHeaderValueCount pins the config → server mapping. Zero
// is passed through untouched: net/http reads it as "apply my own default"
// (DefaultMaxHeaderValueCount), so the framework must not substitute a number
// of its own and freeze a stdlib default into Credo.
func TestBuildServer_MaxHeaderValueCount(t *testing.T) {
	tests := []struct {
		name string
		cfg  int
		want int
	}{
		{"unset stays zero", 0, 0},
		{"explicit limit", 12, 12},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := buildServer(serverConfig{MaxHeaderValueCount: tt.cfg}, http.DefaultServeMux, nil, nil)
			if srv.MaxHeaderValueCount != tt.want {
				t.Errorf("MaxHeaderValueCount = %d, want %d", srv.MaxHeaderValueCount, tt.want)
			}
		})
	}
}

// TestValidateServerConfig_MaxHeaderValueCount locks the fail-fast rule: a
// negative count is rejected rather than reaching net/http, where it would be
// silently indistinguishable from the default.
func TestValidateServerConfig_MaxHeaderValueCount(t *testing.T) {
	err := validateServerConfig(&serverConfig{MaxHeaderValueCount: -1})
	if err == nil {
		t.Fatal("validateServerConfig accepted a negative MaxHeaderValueCount")
	}
	if got := err.Error(); !strings.Contains(got, "MaxHeaderValueCount") {
		t.Errorf("error = %q, want it to name the field", got)
	}
	if err := validateServerConfig(&serverConfig{MaxHeaderValueCount: 0}); err != nil {
		t.Errorf("zero MaxHeaderValueCount rejected: %v", err)
	}
}

// TestBuildServer_ConfigureCallback proves the ownership contract of
// WithHTTPServer: the callback sees a fully-populated server, wins over every
// framework-set field it touches, and cannot take over the three the
// lifecycle owns.
func TestBuildServer_ConfigureCallback(t *testing.T) {
	cfg := serverConfig{
		Host:              "127.0.0.1",
		Port:              8080,
		ReadHeaderTimeout: 3 * time.Second,
		MaxHeaderBytes:    4096,
	}
	handler := http.DefaultServeMux
	otherHandler := http.NewServeMux()

	var seen *http.Server
	calls := 0
	srv := buildServer(cfg, handler, nil, func(s *http.Server) {
		calls++
		// Snapshot what the callback was handed, before mutating it.
		seen = &http.Server{
			Addr:              s.Addr,
			ReadHeaderTimeout: s.ReadHeaderTimeout,
			MaxHeaderBytes:    s.MaxHeaderBytes,
			ErrorLog:          s.ErrorLog,
		}
		// Framework-set fields the callback is allowed to override.
		s.MaxHeaderBytes = 1 << 20
		s.MaxHeaderValueCount = 7
		s.ConnState = func(net.Conn, http.ConnState) {}
		// Framework-owned fields, re-imposed on return.
		s.Handler = otherHandler
		s.Addr = "127.0.0.1:9999"
		s.TLSConfig = &tls.Config{}
	})

	if calls != 1 {
		t.Fatalf("callback called %d times, want exactly 1", calls)
	}
	if seen.Addr != "127.0.0.1:8080" {
		t.Errorf("callback saw Addr %q, want the configured address", seen.Addr)
	}
	if seen.ReadHeaderTimeout != 3*time.Second || seen.MaxHeaderBytes != 4096 {
		t.Errorf("callback saw unpopulated config fields: %+v", seen)
	}
	if seen.ErrorLog == nil {
		t.Error("callback saw a nil ErrorLog; it must run after the bridge so it can override it")
	}

	if srv.MaxHeaderBytes != 1<<20 || srv.MaxHeaderValueCount != 7 {
		t.Errorf("callback overrides lost: MaxHeaderBytes=%d MaxHeaderValueCount=%d",
			srv.MaxHeaderBytes, srv.MaxHeaderValueCount)
	}
	if srv.ConnState == nil {
		t.Error("ConnState set by the callback was dropped")
	}
	if srv.Handler != http.Handler(handler) {
		t.Error("Handler must be re-imposed after the callback")
	}
	if srv.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr = %q, want the configured address re-imposed", srv.Addr)
	}
}

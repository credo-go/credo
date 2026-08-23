package credo

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// TestNewServerErrorLog pins the bridge semantics: the stdlib message text is
// preserved verbatim, records land at Error level tagged component=net/http,
// the trailing newline log.Logger appends is dropped, and a nil logger falls
// back to the framework default instead of panicking.
func TestNewServerErrorLog(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	el := newServerErrorLog(logger)
	el.Printf("http: TLS handshake error from %s: %v", "127.0.0.1:5555", "bad record")

	got := buf.String()
	for _, want := range []string{
		"level=ERROR",
		"component=net/http",
		`msg="http: TLS handshake error from 127.0.0.1:5555: bad record"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("record missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("expected exactly one record line (the log.Logger newline must be trimmed):\n%q", got)
	}

	// A nil logger must not panic: framework internals may build a server
	// before an application logger is configured.
	if el := newServerErrorLog(nil); el == nil {
		t.Fatal("newServerErrorLog(nil) returned nil")
	}
}

// TestBuildServer_ErrorLogBridged locks the wiring: every server the framework
// builds carries the bridge, so net/http never falls back to the standard log
// package's stderr output.
func TestBuildServer_ErrorLogBridged(t *testing.T) {
	srv := buildServer(serverConfig{Host: "127.0.0.1", Port: 8080}, http.DefaultServeMux, nil, nil)
	if srv.ErrorLog == nil {
		t.Fatal("buildServer left ErrorLog nil; net/http would log to stderr")
	}
}

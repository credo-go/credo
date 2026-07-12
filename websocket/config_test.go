package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coderwebsocket "github.com/coder/websocket"

	"github.com/credo-go/credo"
)

func TestResolveConfigDefaultsAndCompressionMapping(t *testing.T) {
	zero, err := resolveConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if zero.readLimit != 32<<10 {
		t.Errorf("zero readLimit = %d, want %d", zero.readLimit, 32<<10)
	}
	if zero.compressionMode != coderwebsocket.CompressionDisabled || zero.compressionThreshold != 0 {
		t.Errorf("zero compression = (%d, %d), want disabled/0",
			zero.compressionMode, zero.compressionThreshold)
	}
	if zero.requireSubprotocol || len(zero.subprotocols) != 0 || len(zero.origins.allowed) != 0 {
		t.Errorf("zero config unexpectedly enabled policy: %+v", zero)
	}

	tests := []struct {
		name          string
		cfg           Config
		wantMode      coderwebsocket.CompressionMode
		wantThreshold int
	}{
		{
			name:          "disabled ignores positive threshold",
			cfg:           Config{CompressionThreshold: 999},
			wantMode:      coderwebsocket.CompressionDisabled,
			wantThreshold: 0,
		},
		{
			name:          "no context default",
			cfg:           Config{CompressionMode: CompressionNoContextTakeover},
			wantMode:      coderwebsocket.CompressionNoContextTakeover,
			wantThreshold: 512,
		},
		{
			name:          "context default",
			cfg:           Config{CompressionMode: CompressionContextTakeover},
			wantMode:      coderwebsocket.CompressionContextTakeover,
			wantThreshold: 128,
		},
		{
			name: "enabled exact threshold",
			cfg: Config{
				CompressionMode:      CompressionNoContextTakeover,
				CompressionThreshold: 42,
			},
			wantMode:      coderwebsocket.CompressionNoContextTakeover,
			wantThreshold: 42,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveConfig(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got.compressionMode != tc.wantMode || got.compressionThreshold != tc.wantThreshold {
				t.Errorf("compression = (%d, %d), want (%d, %d)",
					got.compressionMode, got.compressionThreshold, tc.wantMode, tc.wantThreshold)
			}
		})
	}
}

func TestResolveConfigValidationAndDefensiveOwnership(t *testing.T) {
	invalid := []struct {
		name string
		cfg  Config
	}{
		{name: "negative read limit", cfg: Config{ReadLimit: -1}},
		{name: "unknown compression", cfg: Config{CompressionMode: 99}},
		{name: "negative threshold", cfg: Config{CompressionThreshold: -1}},
		{name: "required empty protocols", cfg: Config{RequireSubprotocol: true}},
		{name: "empty protocol", cfg: Config{Subprotocols: []string{""}}},
		{name: "invalid protocol", cfg: Config{Subprotocols: []string{"bad token"}}},
		{name: "duplicate protocol", cfg: Config{Subprotocols: []string{"chat", "chat"}}},
		{name: "invalid origin", cfg: Config{AllowedOrigins: []string{"https://example.com/path"}}},
		{
			name: "insecure and allowlist",
			cfg: Config{
				AllowedOrigins:          []string{"https://example.com"},
				InsecureSkipOriginCheck: true,
			},
		},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveConfig(tc.cfg); err == nil {
				t.Fatal("resolveConfig() accepted invalid config")
			}
		})
	}

	origins := []string{"https://APP.example.com"}
	protocols := []string{"events.v1", "events.v2"}
	app := mustNewWebSocketApp(t)
	server := Use(app, Config{AllowedOrigins: origins, Subprotocols: protocols})
	origins[0] = "https://evil.example"
	protocols[0] = "mutated"
	if server.config.origins.allowed[0].origin.host != "app.example.com" {
		t.Errorf("resolved origin changed after caller mutation: %+v", server.config.origins.allowed)
	}
	if server.config.subprotocols[0] != "events.v1" {
		t.Errorf("resolved subprotocol changed after caller mutation: %q", server.config.subprotocols[0])
	}
	opts := server.config.acceptOptions()
	opts.Subprotocols[0] = "mutated again"
	if server.config.subprotocols[0] != "events.v1" {
		t.Fatal("acceptOptions exposed the frozen subprotocol slice")
	}
}

func TestUsePanicsForStartupMisuse(t *testing.T) {
	tests := []struct {
		name string
		fn   func()
	}{
		{name: "nil app", fn: func() { Use(nil) }},
		{
			name: "multiple configs",
			fn:   func() { Use(mustNewWebSocketApp(t), Config{}, Config{}) },
		},
		{
			name: "invalid config",
			fn:   func() { Use(mustNewWebSocketApp(t), Config{ReadLimit: -1}) },
		},
		{
			name: "frozen app",
			fn: func() {
				app := mustNewWebSocketApp(t)
				app.ServeHTTP(
					httptest.NewRecorder(),
					httptest.NewRequest(http.MethodGet, "/", nil),
				)
				Use(app)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("expected panic")
				}
			}()
			tc.fn()
		})
	}
}

func TestInvalidUseDoesNotPreventLaterValidRegistration(t *testing.T) {
	app := mustNewWebSocketApp(t)
	func() {
		defer func() { _ = recover() }()
		Use(app, Config{AllowedOrigins: []string{"not an origin"}})
	}()
	server := Use(app)
	if server == nil {
		t.Fatal("valid Use() returned nil after invalid config panic")
	}

	var drainRan bool
	app.OnDrain(func(context.Context) error {
		drainRan = true
		return nil
	})
	app.OnStart(func(context.Context) error { return errConfigStartupStop })
	if err := app.Run(); err == nil {
		t.Fatal("Run() should return the deliberate startup error")
	}
	if !drainRan {
		t.Fatal("valid lifecycle registration did not survive prior config panic")
	}
}

var errConfigStartupStop = &configStartupError{}

type configStartupError struct{}

func (*configStartupError) Error() string { return "stop after lifecycle registration test" }

func mustNewWebSocketApp(t *testing.T) *credo.App {
	t.Helper()
	app, err := credo.New(
		credo.WithAddr("127.0.0.1", 0),
		credo.WithoutAccessLog(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestConfigPanicMentionsInvalidField(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil || !strings.Contains(recovered.(string), "ReadLimit") {
			t.Fatalf("panic = %v, want ReadLimit diagnostic", recovered)
		}
	}()
	Use(mustNewWebSocketApp(t), Config{ReadLimit: -1})
}

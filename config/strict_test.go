package config

import (
	"strings"
	"testing"
	"time"
)

// strictAppConfig is the common target for strict-decoding tests.
type strictAppConfig struct {
	Name    string
	Port    int
	Debug   bool
	Timeout time.Duration
	Level   testLogLevel
}

// testLogLevel exercises the retained TextUnmarshaler hook under strict mode.
type testLogLevel string

func (l *testLogLevel) UnmarshalText(text []byte) error {
	*l = testLogLevel("level:" + string(text))
	return nil
}

// loadStrict builds a hermetic strict Config from a JSON document.
func loadStrict(t *testing.T, doc string) *Config {
	t.Helper()
	c, err := LoadBytes([]byte(doc), FormatJSON,
		WithoutProcessEnv(), WithoutDotenv(), WithStrictDecoding())
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return c
}

func TestStrictDecodingRejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		key  string
	}{
		{
			name: "top-level unknown key",
			doc:  `{"app":{"name":"x","porrt":8080}}`,
			key:  "porrt",
		},
		{
			name: "nested unknown key",
			doc:  `{"app":{"name":"x","nested":{"deep":"secret-value"}}}`,
			key:  "nested",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := loadStrict(t, tt.doc)
			var cfg strictAppConfig
			err := c.Unmarshal("app", &cfg)
			if err == nil {
				t.Fatal("Unmarshal: expected unknown-key error, got nil")
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q does not name the unknown key %q", err, tt.key)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Errorf("unknown-key error leaked a config value: %q", err)
			}
		})
	}
}

func TestStrictDecodingDisablesWeakCoercion(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{name: "string to int", doc: `{"app":{"port":"8080"}}`},
		{name: "string to bool", doc: `{"app":{"debug":"true"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Default mode: weak coercion accepts the string form.
			weak, err := LoadBytes([]byte(tt.doc), FormatJSON, WithoutProcessEnv(), WithoutDotenv())
			if err != nil {
				t.Fatalf("LoadBytes: %v", err)
			}
			var cfg strictAppConfig
			if err := weak.Unmarshal("app", &cfg); err != nil {
				t.Fatalf("default mode must coerce: %v", err)
			}

			// Strict mode: the same document fails to decode.
			c := loadStrict(t, tt.doc)
			cfg = strictAppConfig{}
			if err := c.Unmarshal("app", &cfg); err == nil {
				t.Error("strict mode: expected coercion error, got nil")
			}
		})
	}
}

// TestStrictDecodingRetainedCoercions pins the deliberate exceptions: duration
// strings, TextUnmarshaler fields, and native numeric kind conversions (JSON
// numbers arrive as float64 and must still decode into int and Duration).
func TestStrictDecodingRetainedCoercions(t *testing.T) {
	c := loadStrict(t, `{"app":{"name":"x","port":8080,"debug":true,"timeout":"5s","level":"info"}}`)
	var cfg strictAppConfig
	if err := c.Unmarshal("app", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080 (float64→int is native, not weak)", cfg.Port)
	}
	if cfg.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s (duration hook must stay active)", cfg.Timeout)
	}
	if cfg.Level != "level:info" {
		t.Errorf("Level = %q, want %q (TextUnmarshaler hook must stay active)", cfg.Level, "level:info")
	}
}

func TestStrictDecodingPrimitives(t *testing.T) {
	c := loadStrict(t, `{"app":{"port":8080}}`)
	var port int
	if err := c.Unmarshal("app.port", &port); err != nil || port != 8080 {
		t.Errorf("Unmarshal(app.port) = %d, %v; want 8080", port, err)
	}
	var got string
	if err := c.Unmarshal("app.port", &got); err == nil {
		t.Error("strict mode: int into *string must fail, got nil error")
	}
}

// TestStrictDecodingAppliesToStaged verifies that the staged candidate
// produced by Stage inherits the strict policy — the seam reload subscriber
// validation decodes through.
func TestStrictDecodingAppliesToStaged(t *testing.T) {
	c := loadStrict(t, `{"app":{"name":"x","unknown_key":1}}`)
	s, err := c.Stage()
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	var cfg strictAppConfig
	if err := s.Unmarshal("app", &cfg); err == nil {
		t.Error("staged Unmarshal: expected unknown-key error, got nil")
	}
}

// TestStrictDecodingFullTree documents the full-tree caveat: any section the
// target struct does not cover (such as the framework's "server" section)
// fails a strict full-tree decode.
func TestStrictDecodingFullTree(t *testing.T) {
	c := loadStrict(t, `{"app":{"name":"x"},"server":{"port":9090}}`)
	var cfg struct{ App strictAppConfig }
	if err := c.Unmarshal("", &cfg); err == nil {
		t.Error("full-tree decode without a Server field: expected error, got nil")
	}
	var full struct {
		App    strictAppConfig
		Server struct{ Port int }
	}
	if err := c.Unmarshal("", &full); err != nil {
		t.Errorf("full-tree decode covering all sections: %v", err)
	}
}

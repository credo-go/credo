package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestConfigFormattingIsRedacted verifies that every formatting path over a
// *Config — fmt verbs, GoString, and slog attribute logging — emits metadata
// only and never a configuration value or key name.
func TestConfigFormattingIsRedacted(t *testing.T) {
	c, err := LoadBytes([]byte(`{"db":{"password":"s3cr3t-value","host":"localhost"}}`),
		FormatJSON, WithoutProcessEnv(), WithoutDotenv())
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	outputs := map[string]string{
		"%v":  fmt.Sprintf("%v", c),
		"%s":  fmt.Sprintf("%s", c),
		"%+v": fmt.Sprintf("%+v", c),
		"%#v": fmt.Sprintf("%#v", c),
		// A dereferenced copy must be just as redacted as the pointer.
		"deref %v":  fmt.Sprintf("%v", *c),
		"deref %+v": fmt.Sprintf("%+v", *c),
		"deref %#v": fmt.Sprintf("%#v", *c),
	}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("loaded", "config", c)
	outputs["slog"] = logs.String()
	logs.Reset()
	slog.New(slog.NewTextHandler(&logs, nil)).Info("loaded", "config", *c)
	outputs["deref slog"] = logs.String()

	for verb, out := range outputs {
		if strings.Contains(out, "s3cr3t-value") || strings.Contains(out, "password") {
			t.Errorf("%s leaked config content: %q", verb, out)
		}
		if !strings.Contains(out, "redacted") {
			t.Errorf("%s missing redaction marker: %q", verb, out)
		}
		if !strings.Contains(out, "2 keys") {
			t.Errorf("%s missing key count metadata: %q", verb, out)
		}
	}
}

// TestConfigStringEdgeCases pins the nil and uninitialized forms.
func TestConfigStringEdgeCases(t *testing.T) {
	var c *Config
	if got := fmt.Sprintf("%v", c); got != "<nil>" {
		t.Errorf("nil %%v = %q", got)
	}
	if got := (&Config{}).String(); got != "config.Config(uninitialized)" {
		t.Errorf("uninitialized String() = %q", got)
	}
	if got := (Config{}).String(); got != "config.Config(uninitialized)" {
		t.Errorf("zero-value String() = %q", got)
	}
	if (&Config{}).Exists("any") {
		t.Error("uninitialized Exists() = true, want false")
	}
}

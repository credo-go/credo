package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// strictAppConfig is the common target for decoding-mode tests.
type strictAppConfig struct {
	Name    string
	Port    int
	Debug   bool
	Ratio   float64
	Timeout time.Duration
	Level   testLogLevel
	Tags    []string
}

// testLogLevel exercises the TextUnmarshaler hook, which runs in both modes.
type testLogLevel string

func (l *testLogLevel) UnmarshalText(text []byte) error {
	*l = testLogLevel("level:" + string(text))
	return nil
}

// loadHermetic builds a Config from a JSON document with no environment or
// .env source, in the default (strict) mode unless opts say otherwise.
func loadHermetic(t *testing.T, doc string, opts ...Option) *Config {
	t.Helper()
	opts = append([]Option{WithoutProcessEnv(), WithoutDotenv()}, opts...)
	c, err := LoadBytes([]byte(doc), FormatJSON, opts...)
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return c
}

// assertRemedy checks that a strict failure names the key path, the refused
// conversion or the unknown key, and the opt-in.
func assertRemedy(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a strict decoding error, got nil")
	}
	for _, p := range append(parts, "config.WithWeakDecoding()") {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not contain %q", err, p)
		}
	}
}

func TestStrictDecodingRejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		path string
	}{
		{
			name: "unknown key in a section",
			doc:  `{"app":{"name":"x","porrt":8080}}`,
			path: `"app.porrt"`,
		},
		{
			name: "unknown nested section",
			doc:  `{"app":{"name":"x","nested":{"deep":"secret-value"}}}`,
			path: `"app.nested"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := loadHermetic(t, tt.doc)
			var cfg strictAppConfig
			err := c.Unmarshal("app", &cfg)
			assertRemedy(t, err, "unknown key", tt.path)
			if strings.Contains(err.Error(), "secret-value") {
				t.Errorf("unknown-key error leaked a config value: %q", err)
			}
		})
	}
}

// TestStrictDecodingUnknownKeyThroughLoad covers the file path: a Config
// built by Load from a file decodes strictly by default.
func TestStrictDecodingUnknownKeyThroughLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("app:\n  name: x\n  db:\n    hots: localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(WithFiles(path), WithoutProcessEnv(), WithoutDotenv())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var cfg struct {
		Name string
		DB   struct{ Host string }
	}
	assertRemedy(t, c.Unmarshal("app", &cfg), "unknown key", `"app.db.hots"`)
}

// TestStrictDecodingStringOverrides pins that strings parse into the basic
// type of their target: an environment override of a typed field decodes.
func TestStrictDecodingStringOverrides(t *testing.T) {
	t.Setenv("CREDO_APP__PORT", "9090")
	t.Setenv("CREDO_APP__DEBUG", "true")
	t.Setenv("CREDO_APP__RATIO", "0.25")
	t.Setenv("CREDO_APP__TIMEOUT", "5s")
	t.Setenv("CREDO_APP__LEVEL", "info")
	c, err := LoadBytes([]byte(`{"app":{"name":"x","port":8080}}`), FormatJSON, WithoutDotenv())
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	var cfg strictAppConfig
	if err := c.Unmarshal("app", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := strictAppConfig{Name: "x", Port: 9090, Debug: true, Ratio: 0.25, Timeout: 5 * time.Second, Level: "level:info"}
	if cfg.Name != want.Name || cfg.Port != want.Port || cfg.Debug != want.Debug ||
		cfg.Ratio != want.Ratio || cfg.Timeout != want.Timeout || cfg.Level != want.Level {
		t.Errorf("Unmarshal = %+v, want %+v", cfg, want)
	}
}

// TestStrictDecodingRefusesCrossKind pins the conversions strict decoding
// refuses, each named with its key path and the remedy.
func TestStrictDecodingRefusesCrossKind(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		path string
		part string
	}{
		{name: "boolean into int", doc: `{"app":{"port":true}}`, path: `"app.port"`, part: "'int'"},
		{name: "number into string", doc: `{"app":{"name":42}}`, path: `"app.name"`, part: "'string'"},
		{name: "scalar into slice", doc: `{"app":{"tags":"a"}}`, path: `"app.tags"`, part: "array or slice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := loadHermetic(t, tt.doc)
			var cfg strictAppConfig
			assertRemedy(t, c.Unmarshal("app", &cfg), tt.path, tt.part, "converts no value across kinds")

			weak := loadHermetic(t, tt.doc, WithWeakDecoding())
			cfg = strictAppConfig{}
			if err := weak.Unmarshal("app", &cfg); err != nil {
				t.Errorf("WithWeakDecoding: %v", err)
			}
		})
	}
}

// TestStrictDecodingParseErrorHasNoRemedy pins that a string that does not
// parse is reported with its key path but without the opt-in, which would
// not decode it either.
func TestStrictDecodingParseErrorHasNoRemedy(t *testing.T) {
	c := loadHermetic(t, `{"app":{"port":"eighty"}}`)
	var cfg strictAppConfig
	err := c.Unmarshal("app", &cfg)
	if err == nil || !strings.Contains(err.Error(), `"app.port"`) {
		t.Fatalf("Unmarshal = %v, want an error naming app.port", err)
	}
	if strings.Contains(err.Error(), "WithWeakDecoding") {
		t.Errorf("parse error %q offers an opt-in that would not decode the value", err)
	}
}

// TestStrictDecodingReportsEveryKey checks that one decode reports every
// offending key, not only the first.
func TestStrictDecodingReportsEveryKey(t *testing.T) {
	c := loadHermetic(t, `{"app":{"port":true,"zeta":1,"alpha":2}}`)
	var cfg strictAppConfig
	err := c.Unmarshal("app", &cfg)
	assertRemedy(t, err, `"app.port"`, `"app.alpha"`, `"app.zeta"`)
	if a, z := strings.Index(err.Error(), "app.alpha"), strings.Index(err.Error(), "app.zeta"); a > z {
		t.Errorf("unknown keys are not reported in sorted order: %q", err)
	}
}

// TestWeakDecodingRestoresCoercion pins the opt-in: weak coercion and unknown
// keys ignored, as mapstructure's WeaklyTypedInput does.
func TestWeakDecodingRestoresCoercion(t *testing.T) {
	c := loadHermetic(t, `{"app":{"port":"8080","debug":1,"tags":"a","unknown":true}}`, WithWeakDecoding())
	var cfg strictAppConfig
	if err := c.Unmarshal("app", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Port != 8080 || !cfg.Debug || len(cfg.Tags) != 1 || cfg.Tags[0] != "a" {
		t.Errorf("Unmarshal = %+v, want port 8080, debug true, tags [a]", cfg)
	}
}

// TestStrictDecodingNativeNumbers pins that JSON numbers decode into integer
// and duration fields: a number into a number is no conversion across kinds.
func TestStrictDecodingNativeNumbers(t *testing.T) {
	c := loadHermetic(t, `{"app":{"name":"x","port":8080,"debug":true,"timeout":"5s","level":"info"}}`)
	var cfg strictAppConfig
	if err := c.Unmarshal("app", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Port != 8080 || cfg.Timeout != 5*time.Second || cfg.Level != "level:info" {
		t.Errorf("Unmarshal = %+v", cfg)
	}
}

func TestStrictDecodingPrimitives(t *testing.T) {
	c := loadHermetic(t, `{"app":{"port":8080}}`)
	var port int
	if err := c.Unmarshal("app.port", &port); err != nil || port != 8080 {
		t.Errorf("Unmarshal(app.port) = %d, %v; want 8080", port, err)
	}
	var got string
	assertRemedy(t, c.Unmarshal("app.port", &got), `"app.port"`)
}

// TestReloadCandidateDecodesInInstanceMode verifies that a reload decodes
// its candidate in the instance's mode: a live section whose new value has
// an unknown key aborts a strict reload and is replaced under weak decoding.
func TestReloadCandidateDecodesInInstanceMode(t *testing.T) {
	type app struct{ Name string }
	for _, weak := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(`{"app":{"name":"x"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		opts := []Option{WithFiles(path), WithoutProcessEnv(), WithoutDotenv()}
		if weak {
			opts = append(opts, WithWeakDecoding())
		}
		c, err := Load(opts...)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		live, err := Section[app]("app").Live(c)
		if err != nil {
			t.Fatalf("Live: %v", err)
		}
		if err = os.WriteFile(path, []byte(`{"app":{"name":"y","unknown_key":1}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = c.Reload(t.Context())
		if weak {
			if err != nil || live.Load().Name != "y" {
				t.Errorf("weak reload = %v, name %q; want the new value", err, live.Load().Name)
			}
			continue
		}
		assertRemedy(t, err, `"app.unknown_key"`)
		if live.Load().Name != "x" {
			t.Errorf("strict reload published %q despite the unknown key", live.Load().Name)
		}
	}
}

// TestStrictDecodingFullTree documents the full-tree caveat: any section the
// target struct does not cover (such as the framework's "server" section)
// fails a strict full-tree decode.
func TestStrictDecodingFullTree(t *testing.T) {
	c := loadHermetic(t, `{"app":{"name":"x"},"server":{"port":9090}}`)
	var cfg struct{ App strictAppConfig }
	assertRemedy(t, c.Unmarshal("", &cfg), `unknown key "server"`)
	var full struct {
		App    strictAppConfig
		Server struct{ Port int }
	}
	if err := c.Unmarshal("", &full); err != nil {
		t.Errorf("full-tree decode covering all sections: %v", err)
	}
}

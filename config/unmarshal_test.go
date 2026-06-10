package config_test

import (
	"testing"
	"time"

	"github.com/credo-go/credo/config"
)

// newTestConfig builds a Config from embedded JSON via the public
// [config.LoadBytes] API. The NOTSET_ prefix keeps process env vars out
// of the fixture.
func newTestConfig(t *testing.T) config.RawConfig {
	t.Helper()
	data := []byte(`{
		"server": {"port": 8080, "host": "localhost", "read_timeout": "30s"},
		"debug": true,
		"name": "myapp",
		"rate": 3.14,
		"str_num": "9090",
		"str_on": "true"
	}`)
	c, err := config.LoadBytes(data, config.FormatJSON, config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return c
}

func TestUnmarshalString(t *testing.T) {
	c := newTestConfig(t)
	var got string
	if err := c.Unmarshal("name", &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != "myapp" {
		t.Errorf("got %q, want %q", got, "myapp")
	}
}

func TestUnmarshalInt(t *testing.T) {
	c := newTestConfig(t)
	var got int
	if err := c.Unmarshal("server.port", &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != 8080 {
		t.Errorf("got %d, want 8080", got)
	}
}

func TestUnmarshalIntFromString(t *testing.T) {
	c := newTestConfig(t)
	var got int
	if err := c.Unmarshal("str_num", &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != 9090 {
		t.Errorf("got %d, want 9090", got)
	}
}

func TestUnmarshalBool(t *testing.T) {
	c := newTestConfig(t)
	var got bool
	if err := c.Unmarshal("debug", &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != true {
		t.Errorf("got %v, want true", got)
	}
}

func TestUnmarshalBoolFromString(t *testing.T) {
	c := newTestConfig(t)
	var got bool
	if err := c.Unmarshal("str_on", &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != true {
		t.Errorf("got %v, want true", got)
	}
}

func TestUnmarshalFloat64(t *testing.T) {
	c := newTestConfig(t)
	var got float64
	if err := c.Unmarshal("rate", &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != 3.14 {
		t.Errorf("got %v, want 3.14", got)
	}
}

func TestUnmarshalFloat64FromInt(t *testing.T) {
	c := newTestConfig(t)
	var got float64
	if err := c.Unmarshal("server.port", &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != 8080.0 {
		t.Errorf("got %v, want 8080.0", got)
	}
}

func TestUnmarshalStruct(t *testing.T) {
	c := newTestConfig(t)

	type ServerConfig struct {
		Port        int           `credo:"port"`
		Host        string        `credo:"host"`
		ReadTimeout time.Duration `credo:"read_timeout"`
	}

	var got ServerConfig
	if err := c.Unmarshal("server", &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Port != 8080 {
		t.Errorf("Port: got %d, want 8080", got.Port)
	}
	if got.Host != "localhost" {
		t.Errorf("Host: got %q, want localhost", got.Host)
	}
	if got.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout: got %v, want 30s", got.ReadTimeout)
	}
}

func TestUnmarshalMissing(t *testing.T) {
	c := newTestConfig(t)
	var got string
	if err := c.Unmarshal("nonexistent", &got); err == nil {
		t.Error("expected error for missing key")
	}
}

func TestUnmarshalNilConfig(t *testing.T) {
	var c *config.Config
	var got string
	if err := c.Unmarshal("key", &got); err == nil {
		t.Error("expected error for nil config")
	}
}

func TestUnmarshalExists(t *testing.T) {
	c := newTestConfig(t)
	if !c.Exists("server.port") {
		t.Error("server.port should exist")
	}
	if !c.Exists("server") {
		t.Error("server (intermediate map) should exist")
	}
	if c.Exists("nonexistent") {
		t.Error("nonexistent should not exist")
	}
	if c.Exists("server.missing") {
		t.Error("server.missing should not exist")
	}
}

func TestToSnakeCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Port", "port"},
		{"Host", "host"},
		{"MaxOpen", "max_open"},
		{"MaxIdle", "max_idle"},
		{"MaxLifetime", "max_lifetime"},
		{"ConnectTimeout", "connect_timeout"},
		{"ReadTimeout", "read_timeout"},
		{"WriteTimeout", "write_timeout"},
		{"SSLMode", "ssl_mode"},
		{"APIKey", "api_key"},
		{"HTMLParser", "html_parser"},
		{"ID", "id"},
		{"UserID", "user_id"},
		{"MaxHeaderBytes", "max_header_bytes"},
		{"ReadHeaderTimeout", "read_header_timeout"},
		{"XSSProtection", "xss_protection"},
		{"HSTSMaxAge", "hsts_max_age"},
		{"CSPReportOnly", "csp_report_only"},
		{"", ""},
		{"a", "a"},
		{"A", "a"},
		{"already_snake", "already_snake"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := config.ToSnakeCase(tt.input)
			if got != tt.want {
				t.Errorf("ToSnakeCase(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestUnmarshalStructWithoutTags(t *testing.T) {
	// Verify MapFieldName auto-converts PascalCase to snake_case
	// so structs work without explicit credo tags.
	data := []byte(`{
		"databases": {
			"default": {
				"driver": "postgres",
				"host": "localhost",
				"port": 5432,
				"max_open": 25,
				"max_idle": 5,
				"max_lifetime": "30m",
				"connect_timeout": "10s",
				"ssl_mode": "require"
			}
		}
	}`)
	c, err := config.LoadBytes(data, config.FormatJSON, config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	// No credo tags — relies entirely on MapFieldName.
	type DBConfig struct {
		Driver         string
		Host           string
		Port           int
		MaxOpen        int
		MaxIdle        int
		MaxLifetime    time.Duration
		ConnectTimeout time.Duration
		SSLMode        string
	}

	var got DBConfig
	if err := c.Unmarshal("databases.default", &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Driver != "postgres" {
		t.Errorf("Driver: got %q, want postgres", got.Driver)
	}
	if got.Host != "localhost" {
		t.Errorf("Host: got %q, want localhost", got.Host)
	}
	if got.Port != 5432 {
		t.Errorf("Port: got %d, want 5432", got.Port)
	}
	if got.MaxOpen != 25 {
		t.Errorf("MaxOpen: got %d, want 25", got.MaxOpen)
	}
	if got.MaxIdle != 5 {
		t.Errorf("MaxIdle: got %d, want 5", got.MaxIdle)
	}
	if got.MaxLifetime != 30*time.Minute {
		t.Errorf("MaxLifetime: got %v, want 30m", got.MaxLifetime)
	}
	if got.ConnectTimeout != 10*time.Second {
		t.Errorf("ConnectTimeout: got %v, want 10s", got.ConnectTimeout)
	}
	if got.SSLMode != "require" {
		t.Errorf("SSLMode: got %q, want require", got.SSLMode)
	}
}

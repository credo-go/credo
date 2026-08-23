package credo

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// TestApplyServerDefaults verifies the Slowloris-mitigating ReadHeaderTimeout
// default is applied only when the value is unset.
func TestApplyServerDefaults(t *testing.T) {
	t.Run("zero gets default", func(t *testing.T) {
		c := serverConfig{}
		applyServerDefaults(&c)
		if c.ReadHeaderTimeout != defaultReadHeaderTimeout {
			t.Errorf("ReadHeaderTimeout = %v, want %v", c.ReadHeaderTimeout, defaultReadHeaderTimeout)
		}
		if c.MaxBodyBytes != defaultMaxBodyBytes {
			t.Errorf("MaxBodyBytes = %d, want %d", c.MaxBodyBytes, defaultMaxBodyBytes)
		}
	})

	t.Run("negative MaxBodyBytes preserved (limit disabled)", func(t *testing.T) {
		c := serverConfig{MaxBodyBytes: -1}
		applyServerDefaults(&c)
		if c.MaxBodyBytes != -1 {
			t.Errorf("MaxBodyBytes = %d, want -1 preserved", c.MaxBodyBytes)
		}
	})

	t.Run("explicit value preserved", func(t *testing.T) {
		c := serverConfig{ReadHeaderTimeout: 3 * time.Second}
		applyServerDefaults(&c)
		if c.ReadHeaderTimeout != 3*time.Second {
			t.Errorf("ReadHeaderTimeout = %v, want 3s preserved", c.ReadHeaderTimeout)
		}
	})
}

// TestNew_AppliesReadHeaderTimeoutDefault verifies the default is wired through
// New() so the built server is never left without a header read timeout.
func TestNew_AppliesReadHeaderTimeoutDefault(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if app.serverCfg.ReadHeaderTimeout != defaultReadHeaderTimeout {
		t.Errorf("serverCfg.ReadHeaderTimeout = %v, want %v",
			app.serverCfg.ReadHeaderTimeout, defaultReadHeaderTimeout)
	}
}

// stubServerRC is a RawConfig whose "server" section decodes a fixed map of
// settings onto the destination struct via its "credo" field tags. It lets
// New() exercise the server-config path without any filesystem I/O.
type stubServerRC struct {
	fields map[string]any
}

func (s stubServerRC) Exists(key string) bool { return key == "server" }

func (s stubServerRC) Unmarshal(key string, dst any) error {
	if key != "server" {
		return fmt.Errorf("key %q not found", key)
	}
	rv := reflect.ValueOf(dst).Elem()
	rt := rv.Type()
	for i := range rt.NumField() {
		val, ok := s.fields[rt.Field(i).Tag.Get("credo")]
		if !ok {
			continue
		}
		rv.Field(i).Set(reflect.ValueOf(val).Convert(rv.Field(i).Type()))
	}
	return nil
}

// TestNew_ServerOptionsWinOverConfig locks in that an explicit With* server
// option is never silently overwritten by a conflicting "server" config
// section. Regression: New() re-applied only the trusted-proxy and TLS options
// after Unmarshal, so WithAddr, WithMaxBodyBytes, WithShutdownTimeout, and
// WithRedirectTrailingSlash were clobbered by config. The WithMaxBodyBytes case
// is the sharpest: config 0 → applyServerDefaults → 4 MiB, silently re-enabling
// a limit the caller disabled with -1.
func TestNew_ServerOptionsWinOverConfig(t *testing.T) {
	configTrue := true
	rc := stubServerRC{fields: map[string]any{
		"host":                    "0.0.0.0",
		"port":                    3000,
		"max_body_bytes":          int64(0),
		"shutdown_timeout":        1 * time.Second,
		"reload_timeout":          1 * time.Second,
		"redirect_trailing_slash": &configTrue,
	}}

	app, err := New(
		WithRawConfig(rc),
		WithAddr("127.0.0.1", 9090),
		WithMaxBodyBytes(-1),
		WithShutdownTimeout(5*time.Second),
		WithReloadTimeout(7*time.Second),
		WithRedirectTrailingSlash(false),
	)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if app.serverCfg.Host != "127.0.0.1" || app.serverCfg.Port != 9090 {
		t.Errorf("WithAddr lost to config: Host=%q Port=%d, want 127.0.0.1:9090",
			app.serverCfg.Host, app.serverCfg.Port)
	}
	if app.serverCfg.MaxBodyBytes != -1 {
		t.Errorf("WithMaxBodyBytes(-1) lost to config: MaxBodyBytes=%d, want -1 (limit disabled)",
			app.serverCfg.MaxBodyBytes)
	}
	if app.serverCfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("WithShutdownTimeout lost to config: ShutdownTimeout=%v, want 5s",
			app.serverCfg.ShutdownTimeout)
	}
	if app.serverCfg.ReloadTimeout != 7*time.Second || app.reloadTimeout() != 7*time.Second {
		t.Errorf("WithReloadTimeout lost to config: ReloadTimeout=%v, want 7s", app.serverCfg.ReloadTimeout)
	}
	if app.redirectTrailingSlash {
		t.Error("WithRedirectTrailingSlash(false) lost to config: redirectTrailingSlash=true, want false")
	}
}

// TestNew_ReloadTimeout covers the config key, the default, and rejection of a
// negative value, mirroring shutdown_timeout.
func TestNew_ReloadTimeout(t *testing.T) {
	app, err := New(WithRawConfig(stubServerRC{fields: map[string]any{"reload_timeout": 3 * time.Second}}))
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if app.reloadTimeout() != 3*time.Second {
		t.Errorf("server.reload_timeout: got %v, want 3s", app.reloadTimeout())
	}

	app, err = New(WithRawConfig(stubServerRC{fields: map[string]any{}}))
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if app.reloadTimeout() != defaultReloadTimeout {
		t.Errorf("default reload timeout: got %v, want %v", app.reloadTimeout(), defaultReloadTimeout)
	}

	if _, err := New(WithRawConfig(stubServerRC{fields: map[string]any{}}), WithReloadTimeout(-time.Second)); err == nil {
		t.Error("negative WithReloadTimeout must be rejected by New")
	}
}

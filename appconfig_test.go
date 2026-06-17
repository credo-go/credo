package credo_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

func TestNew_ZeroConfig(t *testing.T) {
	// Zero-config: New() without arguments uses defaults.
	app, err := credo.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if app == nil {
		t.Fatal("New() returned nil")
	}
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q, want %q", got, "building")
	}
}

func TestNew_WithRawConfig_BypassesAutoLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("server: ["), 0o600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	t.Chdir(dir)

	if _, err := credo.New(); err == nil {
		t.Fatal("New() should auto-load config.yaml and fail on invalid syntax")
	}

	rc := newServerConfigRC(map[string]any{})
	app, err := credo.New(credo.WithRawConfig(rc))
	if err != nil {
		t.Fatalf("New(WithRawConfig) should bypass auto-load, got: %v", err)
	}

	resolved, err := credo.Resolve[credo.RawConfig](app)
	if err != nil {
		t.Fatalf("Resolve[RawConfig]: %v", err)
	}
	if resolved != rc {
		t.Fatal("resolved RawConfig should be the explicit instance passed to WithRawConfig")
	}
}

func TestNew_WithAddr(t *testing.T) {
	app, err := credo.New(credo.WithAddr("127.0.0.1", 9090))
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if app == nil {
		t.Fatal("New() returned nil")
	}
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q, want %q", got, "building")
	}
}

func TestNew_InvalidConfig_NegativeReadTimeout(t *testing.T) {
	rc := newServerConfigRC(map[string]any{"read_timeout": -1 * time.Second})
	_, err := credo.New(credo.WithRawConfig(rc))
	if err == nil {
		t.Fatal("expected error for negative ReadTimeout")
	}
}

func TestNew_InvalidConfig_NegativeWriteTimeout(t *testing.T) {
	rc := newServerConfigRC(map[string]any{"write_timeout": -1 * time.Second})
	_, err := credo.New(credo.WithRawConfig(rc))
	if err == nil {
		t.Fatal("expected error for negative WriteTimeout")
	}
}

func TestNew_InvalidConfig_NegativeIdleTimeout(t *testing.T) {
	rc := newServerConfigRC(map[string]any{"idle_timeout": -1 * time.Second})
	_, err := credo.New(credo.WithRawConfig(rc))
	if err == nil {
		t.Fatal("expected error for negative IdleTimeout")
	}
}

func TestNew_InvalidConfig_NegativeReadHeaderTimeout(t *testing.T) {
	rc := newServerConfigRC(map[string]any{"read_header_timeout": -1 * time.Second})
	_, err := credo.New(credo.WithRawConfig(rc))
	if err == nil {
		t.Fatal("expected error for negative ReadHeaderTimeout")
	}
}

func TestNew_InvalidConfig_NegativePort(t *testing.T) {
	_, err := credo.New(credo.WithAddr("", -1))
	if err == nil {
		t.Fatal("expected error for negative port")
	}
}

func TestNew_InvalidConfig_PortTooHigh(t *testing.T) {
	_, err := credo.New(credo.WithAddr("", 70000))
	if err == nil {
		t.Fatal("expected error for port > 65535")
	}
}

func TestNew_InvalidConfig_NegativeMaxHeaderBytes(t *testing.T) {
	rc := newServerConfigRC(map[string]any{"max_header_bytes": -1})
	_, err := credo.New(credo.WithRawConfig(rc))
	if err == nil {
		t.Fatal("expected error for negative MaxHeaderBytes")
	}
}

func TestNew_ValidConfig_BoundaryPort(t *testing.T) {
	// Port 0 and 65535 should both be valid.
	for _, port := range []int{0, 1, 65535} {
		app, err := credo.New(credo.WithAddr("", port))
		if err != nil {
			t.Errorf("New() with port %d error: %v", port, err)
		}
		if app == nil {
			t.Errorf("New() with port %d returned nil", port)
		}
	}
}

func TestNew_InvalidTrustedProxiesOption(t *testing.T) {
	_, err := credo.New(credo.WithTrustedProxies("not-cidr"))
	if err == nil {
		t.Fatal("expected error for invalid trusted proxy CIDR")
	}
}

func TestNew_InvalidTrustedProxiesConfig(t *testing.T) {
	rc := newServerConfigRC(map[string]any{"trusted_proxies": []string{"not-cidr"}})
	_, err := credo.New(credo.WithRawConfig(rc))
	if err == nil {
		t.Fatal("expected error for invalid trusted proxy CIDR")
	}
}

func TestNew_TrustedProxiesConfig(t *testing.T) {
	rc := newServerConfigRC(map[string]any{"trusted_proxies": []string{"10.0.0.0/8"}})
	app := mustNew(t, credo.WithRawConfig(rc), credo.WithoutAccessLog())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Request().RealIP())
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.10")
	app.ServeHTTP(w, r)

	if got := w.Body.String(); got != "203.0.113.10" {
		t.Fatalf("RealIP() = %q, want 203.0.113.10", got)
	}
}

func TestNew_TrustedProxiesOptionOverridesConfig(t *testing.T) {
	rc := newServerConfigRC(map[string]any{"trusted_proxies": []string{"192.0.2.0/24"}})
	app := mustNew(t, credo.WithRawConfig(rc), credo.WithTrustedProxies("10.0.0.0/8"), credo.WithoutAccessLog())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Request().RealIP())
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.10")
	app.ServeHTTP(w, r)

	if got := w.Body.String(); got != "203.0.113.10" {
		t.Fatalf("RealIP() = %q, want 203.0.113.10", got)
	}
}

// serverConfigRC is a RawConfig that populates a struct from a map when
// the "server" key is requested. It matches struct field tags ("credo:...")
// against map keys via reflection.
type serverConfigRC struct {
	fields map[string]any
}

// newServerConfigRC creates a RawConfig that returns the given map of
// server settings when Unmarshal("server", &dst) is called.
func newServerConfigRC(fields map[string]any) *serverConfigRC {
	return &serverConfigRC{fields: fields}
}

func (s *serverConfigRC) Unmarshal(key string, dst any) error {
	if key != "server" {
		return fmt.Errorf("key %q not found", key)
	}
	// Use reflection to set fields on the destination struct based on "credo" tags.
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("dst must be a non-nil pointer")
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("dst must point to a struct")
	}
	rt := rv.Type()
	for i := range rt.NumField() {
		tag := rt.Field(i).Tag.Get("credo")
		if tag == "" {
			continue
		}
		if val, ok := s.fields[tag]; ok {
			fv := rv.Field(i)
			if fv.CanSet() {
				fv.Set(reflect.ValueOf(val).Convert(fv.Type()))
			}
		}
	}
	return nil
}

func (s *serverConfigRC) Exists(key string) bool {
	return key == "server"
}

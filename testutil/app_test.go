package testutil_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/testutil"
)

type greeter struct {
	msg string
}

func TestNewApp_Defaults(t *testing.T) {
	app := testutil.NewApp(t)

	// Hermetic: the injected RawConfig is empty, so nothing was auto-loaded
	// from the working directory.
	rc := app.MustResolve[credo.RawConfig]()
	if rc.Exists("server") {
		t.Error("expected hermetic config: the server key should not exist")
	}

	// The App is usable and the built-in middleware tier runs.
	app.GET("/ping", func(c *credo.Context) error {
		return c.Response().Text(http.StatusOK, "pong")
	})
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "pong" {
		t.Errorf("GET /ping = %d %q, want %d %q",
			rec.Code, rec.Body.String(), http.StatusOK, "pong")
	}
}

func TestWithOverride_ReplacesWiredDep(t *testing.T) {
	app := testutil.NewApp(t,
		// Wiring establishes the "real" binding...
		testutil.WithWiring(func(app *credo.App) {
			app.MustProvideValue[*greeter](&greeter{msg: "real"})
		}),
		// ...and the override replaces it (overrides run after wiring).
		testutil.WithOverride[*greeter](&greeter{msg: "fake"}),
	)

	got := app.MustResolve[*greeter]()
	if got.msg != "fake" {
		t.Errorf("greeter.msg = %q, want %q (override should win over wiring)", got.msg, "fake")
	}
}

func TestWithOverride_AddsWhenAbsent(t *testing.T) {
	// WithOverride works even when nothing wired T, because Replace adds the
	// binding when it is absent.
	app := testutil.NewApp(t, testutil.WithOverride[*greeter](&greeter{msg: "only"}))

	if got := app.MustResolve[*greeter](); got.msg != "only" {
		t.Errorf("greeter.msg = %q, want %q", got.msg, "only")
	}
}

func TestWithConfig_Injection(t *testing.T) {
	type appCfg struct {
		Name string `credo:"name"`
		Env  string `credo:"env"`
	}

	app := testutil.NewApp(t,
		// Two pairs under the same root exercise dotted-key nesting + merge.
		testutil.WithConfig("app.name", "credo-test"),
		testutil.WithConfig("app.env", "testing"),
	)

	rc := app.MustResolve[credo.RawConfig]()

	var cfg appCfg
	if err := rc.Unmarshal("app", &cfg); err != nil {
		t.Fatalf("Unmarshal(\"app\"): %v", err)
	}
	if cfg.Name != "credo-test" {
		t.Errorf("cfg.Name = %q, want %q", cfg.Name, "credo-test")
	}
	if cfg.Env != "testing" {
		t.Errorf("cfg.Env = %q, want %q", cfg.Env, "testing")
	}
}

func TestAssertHas_Pass(t *testing.T) {
	buf := testutil.NewLogBuffer()
	app := testutil.NewApp(t, testutil.WithLogBuffer(buf))

	app.GET("/ping", func(c *credo.Context) error {
		return c.Response().Text(http.StatusOK, "pong")
	})
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))

	// The built-in access log emits "request completed" at INFO for a 200,
	// with method and status attributes. Level matches case-insensitively and
	// status (an int) is compared after JSON normalization.
	buf.AssertHas(t, testutil.LogEntry{
		Level:   "INFO",
		Message: "request completed",
		Attrs: map[string]any{
			"method": http.MethodGet,
			"status": 200,
		},
	})
}

func TestAssertNotHas_Pass(t *testing.T) {
	buf := testutil.NewLogBuffer()
	app := testutil.NewApp(t, testutil.WithLogBuffer(buf))

	app.GET("/ping", func(c *credo.Context) error {
		return c.Response().Text(http.StatusOK, "pong")
	})
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))

	// A 200 response never produces an ERROR-level access log entry.
	buf.AssertNotHas(t, testutil.LogEntry{Level: "ERROR", Message: "request completed"})
}

func TestAssertEmpty_Pass(t *testing.T) {
	buf := testutil.NewLogBuffer()
	buf.AssertEmpty(t)

	buf.Handler().Handle(t.Context(), newRecord(t))
	buf.Reset()
	buf.AssertEmpty(t)
}

// failProbe records whether Errorf was called, letting the failure paths of
// the assert helpers be tested without failing the real test.
type failProbe struct {
	testing.TB
	failed bool
}

func (p *failProbe) Helper() {}

func (p *failProbe) Errorf(string, ...any) { p.failed = true }

func TestAssertHelpers_FailurePaths(t *testing.T) {
	buf := testutil.NewLogBuffer()

	probe := &failProbe{TB: t}
	buf.AssertHas(probe, testutil.LogEntry{Message: "never logged"})
	if !probe.failed {
		t.Error("AssertHas should fail on an empty buffer")
	}

	buf.Handler().Handle(t.Context(), newRecord(t))

	probe = &failProbe{TB: t}
	buf.AssertNotHas(probe, testutil.LogEntry{Message: "hello"})
	if !probe.failed {
		t.Error("AssertNotHas should fail when a matching record exists")
	}

	probe = &failProbe{TB: t}
	buf.AssertEmpty(probe)
	if !probe.failed {
		t.Error("AssertEmpty should fail when records were captured")
	}
}

func newRecord(t *testing.T) slog.Record {
	t.Helper()
	return slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
}

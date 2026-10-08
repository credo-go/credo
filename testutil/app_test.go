package testutil_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	finalize(t, app)
	rc := app.MustResolve[credo.RawConfig]()
	if rc.Exists("server") {
		t.Error("expected hermetic config: the server key should not exist")
	}

	// The App is usable and the framework request executor runs.
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
			app.ProvideValue[*greeter](&greeter{msg: "real"})
		}),
		// ...and the override replaces it (overrides run after wiring).
		testutil.WithOverride[*greeter](&greeter{msg: "fake"}),
	)

	finalize(t, app)
	got := app.MustResolve[*greeter]()
	if got.msg != "fake" {
		t.Errorf("greeter.msg = %q, want %q (override should win over wiring)", got.msg, "fake")
	}
}

func TestWithOverride_PanicsWithoutBinding(t *testing.T) {
	// An override that no longer matches the wiring fails instead of adding a
	// binding nothing resolves.
	defer func() {
		r := recover()
		msg, _ := r.(string)
		if !strings.Contains(msg, "credo.Override() replaces an earlier binding") {
			t.Fatalf("panic = %v, want the Override misuse", r)
		}
	}()
	testutil.NewApp(t, testutil.WithOverride[*greeter](&greeter{msg: "only"}))
	t.Fatal("WithOverride without a binding did not panic")
}

func TestWithWiring_AddsBinding(t *testing.T) {
	app := testutil.NewApp(t, testutil.WithWiring(func(app *credo.App) {
		app.ProvideValue[*greeter](&greeter{msg: "only"})
	}))

	finalize(t, app)
	if got := app.MustResolve[*greeter](); got.msg != "only" {
		t.Errorf("greeter.msg = %q, want %q", got.msg, "only")
	}
}

// emptyRawConfig is a hermetic RawConfig for an App built without NewApp.
type emptyRawConfig struct{}

func (emptyRawConfig) Unmarshal(string, any) error { return nil }
func (emptyRawConfig) Exists(string) bool          { return false }

// startedComponent records its start, and its stop with whether the test
// server had been closed by then.
type startedComponent struct {
	mu        sync.Mutex
	events    []string
	srvClosed *atomic.Bool
}

func (c *startedComponent) Start(context.Context) error {
	c.add("start")
	return nil
}

func (c *startedComponent) Shutdown(context.Context) error {
	if c.srvClosed.Load() {
		c.add("stop after server closed")
	} else {
		c.add("stop before server closed")
	}
	return nil
}

func (c *startedComponent) add(e string) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *startedComponent) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

func TestStart_ServesThroughHTTPTestAndStopsAfterTheServer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		newApp      func(t *testing.T) *credo.App
		serverFirst bool
	}{
		{name: "NewApp, server after Start", newApp: func(t *testing.T) *credo.App { return testutil.NewApp(t) }},
		{name: "NewApp, server before Start", newApp: func(t *testing.T) *credo.App { return testutil.NewApp(t) },
			serverFirst: true},
		{name: "credo.New, server after Start", newApp: func(t *testing.T) *credo.App {
			app, err := credo.New(credo.WithRawConfig(emptyRawConfig{}), credo.WithLogger(slog.New(slog.DiscardHandler)))
			if err != nil {
				t.Fatal(err)
			}
			return app
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := &atomic.Bool{}
			comp := &startedComponent{srvClosed: closed}
			t.Run("inner", func(t *testing.T) {
				app := tc.newApp(t)
				app.ProvideValue[*startedComponent](comp)
				app.GET("/ping", func(c *credo.Context) error {
					return c.Response().Text(http.StatusOK, "pong")
				})
				var srv *httptest.Server
				open := func() {
					srv = httptest.NewServer(app)
					t.Cleanup(func() { srv.Close(); closed.Store(true) })
				}
				if tc.serverFirst {
					open()
				}
				testutil.Start(t, app)
				if !tc.serverFirst {
					open()
				}
				resp, err := http.Get(srv.URL + "/ping")
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("GET /ping = %d", resp.StatusCode)
				}
			})
			want := []string{"start", "stop after server closed"}
			if got := comp.list(); !slices.Equal(got, want) {
				t.Fatalf("events = %q, want %q", got, want)
			}
		})
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

	finalize(t, app)
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

// TestWithConfig_IsHermetic: a test App is built from the values the test
// gives it and nothing else. A .env file in the working directory and CREDO_*
// variables in the environment — a developer's shell, a CI job — must not reach
// an App configured through WithConfig.
func TestWithConfig_IsHermetic(t *testing.T) {
	newApp := func(t *testing.T) *credo.App {
		t.Helper()
		return testutil.NewApp(t,
			testutil.WithConfig("app.name", "credo-test"),
			testutil.WithConfig("app.env", "testing"),
		)
	}
	check := func(t *testing.T, app *credo.App) {
		t.Helper()
		for key, want := range map[string]string{"app.name": "credo-test", "app.env": "testing"} {
			got, err := app.GetConfig[string](key)
			if err != nil {
				t.Fatalf("GetConfig(%q): %v", key, err)
			}
			if got != want {
				t.Errorf("%s = %q, want the WithConfig value %q", key, got, want)
			}
		}
		for _, key := range []string{"app.extra", "app.secret"} {
			if app.ConfigExists(key) {
				got, _ := app.GetConfig[string](key)
				t.Errorf("%s = %q came from outside the test", key, got)
			}
		}
	}

	t.Run("a .env file in the working directory", func(t *testing.T) {
		dir := t.TempDir()
		dotenv := "APP__NAME=from-dotenv\nAPP__EXTRA=from-dotenv\n"
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotenv), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		check(t, newApp(t))
	})

	t.Run("CREDO_ variables in the process environment", func(t *testing.T) {
		t.Setenv("CREDO_APP__ENV", "from-process-env")
		t.Setenv("CREDO_APP__SECRET", "from-process-env")
		check(t, newApp(t))
	})

	t.Run("CREDO_ENV_FILE naming a file that is not there", func(t *testing.T) {
		t.Setenv("CREDO_ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
		check(t, newApp(t))
	})
}

func TestAssertHas_Pass(t *testing.T) {
	buf := testutil.NewLogBuffer()
	app := testutil.NewApp(t, testutil.WithLogBuffer(buf))
	app.UseAccessLog()

	app.GET("/ping", func(c *credo.Context) error {
		return c.Response().Text(http.StatusOK, "pong")
	})
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))

	// The access log feature emits "request completed" at INFO for a 200,
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
	app.UseAccessLog()

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

func finalize(t *testing.T, app *credo.App) {
	t.Helper()
	if err := app.Finalize(); err != nil {
		t.Fatalf("Finalize() = %v", err)
	}
}

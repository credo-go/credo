package credo_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/credo-go/credo"
)

// switchable is a component whose Ready returns the error it is given.
type switchable struct {
	mu  sync.Mutex
	err error
}

func (s *switchable) set(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *switchable) Ready(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (*switchable) Shutdown(context.Context) error { return nil }

// cacheComponent and gateComponent are two switchable components of their
// own types.
type cacheComponent struct{ switchable }
type gateComponent struct{ switchable }

// readyBody is one /ready response: its code, top-level status and entries.
type readyBody struct {
	code   int
	Status string                       `json:"status"`
	Checks map[string]map[string]string `json:"checks"`
}

func getReady(t *testing.T, app *credo.App) readyBody {
	t.Helper()
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	var body readyBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/ready body %q: %v", rec.Body, err)
	}
	body.code = rec.Code
	return body
}

// TestOptionalReadiness_Component: a failing optional entry is shown down
// and logged while /ready answers 200 degraded, and up returns once it
// recovers; a failing gating entry beside it answers 503 down. The option
// applies to Provide, ProvideValue and Manage.
func TestOptionalReadiness_Component(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func(app *credo.App, c *cacheComponent)
	}{
		{"ProvideValue", func(app *credo.App, c *cacheComponent) { app.ProvideValue(c, credo.OptionalReadiness()) }},
		{"Provide", func(app *credo.App, c *cacheComponent) {
			app.Provide(func() *cacheComponent { return c }, credo.OptionalReadiness())
		}},
		{"Manage", func(app *credo.App, c *cacheComponent) { app.Manage(c, credo.OptionalReadiness()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, logs := newTestLogger(t)
			app := mustNew(t, credo.WithLogger(logger))
			cache, gate := &cacheComponent{}, &gateComponent{}
			tc.register(app, cache)
			app.ProvideValue(gate)
			app.UseHealth()
			startServing(t, app)

			cache.set(errors.New("cache unreachable"))
			body := getReady(t, app)
			if body.code != http.StatusOK || body.Status != "degraded" ||
				body.Checks["*credo_test.cacheComponent"]["status"] != "down" {
				t.Fatalf("/ready = %+v, want 200 degraded with the cache down", body)
			}
			if !strings.Contains(logs.String(), `"check":"*credo_test.cacheComponent"`) ||
				!strings.Contains(logs.String(), "cache unreachable") {
				t.Errorf("the optional failure was not logged: %s", logs)
			}

			gate.set(errors.New("gate down"))
			if body := getReady(t, app); body.code != http.StatusServiceUnavailable || body.Status != "down" {
				t.Errorf("/ready with a gating entry down = %+v, want 503 down", body)
			}

			cache.set(nil)
			gate.set(nil)
			if body := getReady(t, app); body.code != http.StatusOK || body.Status != "up" {
				t.Errorf("/ready after recovery = %+v, want 200 up", body)
			}
		})
	}
}

// readyOnlyValue shows Ready and no Shutdown.
type readyOnlyValue struct {
	mu  sync.Mutex
	err error
}

func (v *readyOnlyValue) set(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.err = err
}

func (v *readyOnlyValue) Ready(context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.err
}

// shutdownOnly shows Shutdown and no Ready.
type shutdownOnly struct{}

func (*shutdownOnly) Shutdown(context.Context) error { return nil }

// TestOptionalReadiness_PanicsWithoutAnEntry: the option panics on a
// registration that plans no readiness entry — a type without Ready, or one
// that is neither a component nor borrowed — and applies to a borrowed value
// whose type shows Ready and no Shutdown.
func TestOptionalReadiness_PanicsWithoutAnEntry(t *testing.T) {
	t.Run("no Ready", func(t *testing.T) {
		app := mustNew(t)
		const want = "credo: App.ProvideValue[*credo_test.shutdownOnly]: credo.OptionalReadiness() on " +
			"*credo_test.shutdownOnly, which plans no readiness entry: its type shows no Ready method; give the " +
			"option to a registration whose type shows Ready"
		if got := mustPanicMessage(t, func() { app.ProvideValue(&shutdownOnly{}, credo.OptionalReadiness()) }); got != want {
			t.Fatalf("panic = %q\nwant    %q", got, want)
		}
	})
	t.Run("neither a component nor borrowed", func(t *testing.T) {
		app := mustNew(t)
		const want = "credo: App.ProvideValue[*credo_test.readyOnlyValue]: credo.OptionalReadiness() on " +
			"*credo_test.readyOnlyValue, which plans no readiness entry: it is neither a component nor borrowed; " +
			"give the option to a registration whose type shows Ready and Shutdown, or to a borrowed one"
		got := mustPanicMessage(t, func() {
			app.ProvideValue(&readyOnlyValue{}, credo.OptionalReadiness())
		})
		if got != want {
			t.Fatalf("panic = %q\nwant    %q", got, want)
		}
	})
	t.Run("borrowed, Ready without Shutdown", func(t *testing.T) {
		app := mustNew(t)
		v := &readyOnlyValue{}
		app.ProvideValue(v, credo.Borrowed(), credo.OptionalReadiness())
		app.UseHealth()
		startServing(t, app)
		v.set(errors.New("down"))
		if body := getReady(t, app); body.code != http.StatusOK || body.Status != "degraded" {
			t.Errorf("/ready = %+v, want 200 degraded", body)
		}
	})
}

// TestAddReadinessCheck_OptionalReadiness: a failing check added with the
// option is shown down and logged while /ready answers 200 degraded; the
// same check without it answers 503; another option panics, naming it.
func TestAddReadinessCheck_OptionalReadiness(t *testing.T) {
	failing := func(context.Context) error { return errors.New("api unreachable") }
	for _, optional := range []bool{true, false} {
		logger, logs := newTestLogger(t)
		app := mustNew(t, credo.WithLogger(logger))
		app.UseHealth()
		var opts []credo.RegistrationOption
		if optional {
			opts = append(opts, credo.OptionalReadiness())
		}
		app.AddReadinessCheck("payments-api", failing, opts...)
		startServing(t, app)
		body := getReady(t, app)
		wantCode, wantStatus := http.StatusServiceUnavailable, "down"
		if optional {
			wantCode, wantStatus = http.StatusOK, "degraded"
		}
		if body.code != wantCode || body.Status != wantStatus || body.Checks["payments-api"]["status"] != "down" {
			t.Errorf("optional=%v: /ready = %+v, want %d %s", optional, body, wantCode, wantStatus)
		}
		if !strings.Contains(logs.String(), "api unreachable") {
			t.Errorf("optional=%v: the failure was not logged: %s", optional, logs)
		}
	}
	app := mustNew(t)
	app.UseHealth()
	const want = "credo: App.AddReadinessCheck does not accept credo.Ingress(); it accepts credo.OptionalReadiness()"
	if got := mustPanicMessage(t, func() { app.AddReadinessCheck("x", failing, credo.Ingress()) }); got != want {
		t.Fatalf("panic = %q\nwant    %q", got, want)
	}
}

// slowReady is a component whose Ready takes the time it is given, or
// waits for its deadline.
type slowReady struct{ d time.Duration }

func (s *slowReady) Ready(ctx context.Context) error {
	if s.d == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	time.Sleep(s.d)
	return nil
}

func (*slowReady) Shutdown(context.Context) error { return nil }

type slowReadyB struct{ slowReady }

// TestReadiness_EveryEntryCarriesLatency: every readiness entry — a
// component and a named check — carries its latency; a check that outlives
// its deadline reports the time until the deadline, and the failure log
// carries it.
func TestReadiness_EveryEntryCarriesLatency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, logs := newTestLogger(t)
		app := mustNew(t, credo.WithLogger(logger))
		app.ProvideValue(&slowReady{d: 300 * time.Millisecond})
		app.ProvideValue(&slowReadyB{}, credo.ComponentName("stuck"))
		app.UseHealth(credo.HealthConfig{CheckTimeout: time.Second})
		app.AddReadinessCheck("named", func(context.Context) error {
			time.Sleep(200 * time.Millisecond)
			return nil
		})
		startServing(t, app)
		body := getReady(t, app)
		for name, want := range map[string]string{"*credo_test.slowReady": "300ms", "named": "200ms", "stuck": "1s"} {
			if got := body.Checks[name]["latency"]; got != want {
				t.Errorf("%s: latency = %q, want %q (body %+v)", name, got, want, body)
			}
		}
		if !strings.Contains(logs.String(), `"check":"stuck"`) || !strings.Contains(logs.String(), `"latency":1000000000`) {
			t.Errorf("the failure log should carry the latency: %s", logs)
		}
	})
}

// failingStart fails its Start.
type failingStart struct{}

func (*failingStart) Start(context.Context) error    { return errors.New("billing refused") }
func (*failingStart) Shutdown(context.Context) error { return nil }

// TestComponentName_OnBindings: credo.ComponentName on ProvideValue names the
// readiness entry and the component in a LifecycleError.
func TestComponentName_OnBindings(t *testing.T) {
	t.Run("readiness entry", func(t *testing.T) {
		app := mustNew(t)
		app.ProvideValue(&cacheComponent{}, credo.ComponentName("cache"))
		app.UseHealth()
		startServing(t, app)
		if body := getReady(t, app); body.Checks["cache"]["status"] != "up" || len(body.Checks) != 1 {
			t.Errorf("/ready = %+v, want one entry named cache", body)
		}
	})
	t.Run("lifecycle error", func(t *testing.T) {
		app := mustNew(t)
		app.Provide(func() *failingStart { return &failingStart{} }, credo.ComponentName("billing"))
		err := app.Start(t.Context())
		le, ok := errors.AsType[*credo.LifecycleError](err)
		if !ok || len(le.Entries) == 0 || le.Entries[0].Name != "billing" {
			t.Fatalf("Start() = %v, want a lifecycle error naming billing", err)
		}
	})
}

// TestComponentName_Misuse: a duplicate name panics, and so do a name on a
// binding that is not a component and an invalid name, naming the option;
// a duplicate binding whose registration carries a name says that the name
// is not a DI key.
func TestComponentName_Misuse(t *testing.T) {
	t.Run("duplicate name", func(t *testing.T) {
		app := mustNew(t)
		app.ProvideValue(&cacheComponent{}, credo.ComponentName("shared"))
		const want = "credo: App.ProvideValue[*credo_test.gateComponent]: a component named \"shared\" is " +
			"already registered, as *credo_test.cacheComponent; give each component its own name with " +
			"credo.ComponentName"
		if got := mustPanicMessage(t, func() {
			app.ProvideValue(&gateComponent{}, credo.ComponentName("shared"))
		}); got != want {
			t.Fatalf("panic = %q\nwant    %q", got, want)
		}
	})
	t.Run("duplicate name on a constructor", func(t *testing.T) {
		app := mustNew(t)
		app.ProvideValue(&cacheComponent{}, credo.ComponentName("shared"))
		app.Provide(func() *gateComponent { return &gateComponent{} }, credo.ComponentName("shared"))
		err := app.Start(t.Context())
		if want := "a component named \"shared\" is already registered, as *credo_test.cacheComponent"; err == nil ||
			!strings.Contains(err.Error(), want) {
			t.Fatalf("Start() = %v, want it to contain %q", err, want)
		}
	})
	t.Run("not a component", func(t *testing.T) {
		app := mustNew(t)
		const want = "credo: App.ProvideValue[*credo_test.bindEnglish]: credo.ComponentName(\"greeter\") on " +
			"*credo_test.bindEnglish, which is not a component: it shows no Shutdown method and has no " +
			"credo.CloseOnShutdown(), and the name is planned before the value exists; give " +
			"*credo_test.bindEnglish a Shutdown method, or provide the concrete type that has one"
		if got := mustPanicMessage(t, func() {
			app.ProvideValue(&bindEnglish{}, credo.ComponentName("greeter"))
		}); got != want {
			t.Fatalf("panic = %q\nwant    %q", got, want)
		}
	})
	t.Run("invalid name", func(t *testing.T) {
		app := mustNew(t)
		got := mustPanicMessage(t, func() {
			app.ProvideValue(&cacheComponent{}, credo.ComponentName("credo.cache"))
		})
		if !strings.HasPrefix(got, `credo: App.ProvideValue: credo.ComponentName("credo.cache"): `) ||
			!strings.Contains(got, "reserved") {
			t.Fatalf("panic = %q, want the option named and the health-name rule", got)
		}
	})
	t.Run("duplicate binding with a name", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			first, again []credo.RegistrationOption
		}{
			{"on the second", nil, []credo.RegistrationOption{credo.ComponentName("replica")}},
			{"on the first", []credo.RegistrationOption{credo.ComponentName("replica")}, nil},
		} {
			app := mustNew(t)
			app.ProvideValue(&cacheComponent{}, tc.first...)
			const want = "credo: App.ProvideValue[*credo_test.cacheComponent]: *credo_test.cacheComponent is " +
				"already registered; credo.ComponentName(\"replica\") names a component in logs, /ready and " +
				"lifecycle errors and is not a DI key; bind a second instance under a wrapper type of its own"
			if got := mustPanicMessage(t, func() { app.ProvideValue(&cacheComponent{}, tc.again...) }); got != want {
				t.Errorf("%s: panic = %q\nwant    %q", tc.name, got, want)
			}
		}
	})
	t.Run("duplicate binding without a name", func(t *testing.T) {
		app := mustNew(t)
		app.ProvideValue(&cacheComponent{})
		const want = "credo: App.ProvideValue[*credo_test.cacheComponent]: *credo_test.cacheComponent is " +
			"already registered; bind a second instance under a wrapper type of its own"
		if got := mustPanicMessage(t, func() { app.ProvideValue(&cacheComponent{}) }); got != want {
			t.Fatalf("panic = %q\nwant    %q", got, want)
		}
	})
}

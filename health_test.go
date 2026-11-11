package credo_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/store"
)

func TestUseHealth_DefaultConfig(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()

	// /health should be registered.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("/health status = %d, want %d", w.Code, http.StatusOK)
	}

	// /ready should be registered.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/ready", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("/ready status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestUseHealth_CustomPaths(t *testing.T) {
	app := mustNew(t)
	app.UseHealth(credo.HealthConfig{
		LivenessPath:  "/livez",
		ReadinessPath: "/readyz",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/livez", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("/livez status = %d, want %d", w.Code, http.StatusOK)
	}

	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/readyz", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("/readyz status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestUseHealth_Disabled(t *testing.T) {
	app := mustNew(t)
	app.UseHealth(credo.HealthConfig{Disabled: true})

	for _, path := range []string{"/health", "/ready"} {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want %d (disabled)", path, w.Code, http.StatusNotFound)
		}
	}
}

// A disabled UseHealth is still the App's UseHealth call, so an application
// that maps a configuration key to Disabled keeps its check registrations
// unconditional: they are accepted, and with no route mounted they never run.
func TestUseHealth_Disabled_AcceptsChecksAndServesNone(t *testing.T) {
	app := mustNew(t)
	app.UseHealth(credo.HealthConfig{Disabled: true})

	var runs atomic.Int32
	check := func(context.Context) error {
		runs.Add(1)
		return errors.New("down")
	}
	app.AddLivenessCheck("disk", check)
	app.AddReadinessCheck("database", check)
	app.AddReadinessCheck("cache", check, credo.OptionalReadiness())

	for _, method := range []string{"GET", "HEAD"} {
		for _, path := range []string{"/health", "/ready"} {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(method, path, nil))
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s status = %d, want %d (disabled)", method, path, w.Code, http.StatusNotFound)
			}
		}
	}
	if got := runs.Load(); got != 0 {
		t.Errorf("checks ran %d times, want 0: a disabled UseHealth serves no check", got)
	}
}

// The checks registered beside a disabled UseHealth are validated like any
// others, so a mistake in one shows in every deployment and not only in the
// one that turns health on.
func TestUseHealth_Disabled_ChecksAreStillValidated(t *testing.T) {
	check := func(context.Context) error { return nil }
	tests := []struct {
		name     string
		register func(*credo.App)
		want     string
	}{
		{"nil liveness check", func(a *credo.App) { a.AddLivenessCheck("disk", nil) },
			`credo: AddLivenessCheck: the check function of "disk" is nil`},
		{"nil readiness check", func(a *credo.App) { a.AddReadinessCheck("database", nil) },
			`credo: AddReadinessCheck: the check function of "database" is nil`},
		{"duplicate readiness name", func(a *credo.App) {
			a.AddReadinessCheck("database", check)
			a.AddReadinessCheck("database", check)
		}, "duplicate readiness check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.UseHealth(credo.HealthConfig{Disabled: true})
			defer func() {
				if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), tt.want) {
					t.Errorf("panic = %v, want it to contain %q", r, tt.want)
				}
			}()
			tt.register(app)
		})
	}
}

func TestUseHealth_LivenessOnly(t *testing.T) {
	app := mustNew(t)
	app.UseHealth(credo.HealthConfig{DisableReadiness: true})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("/health status = %d, want %d", w.Code, http.StatusOK)
	}

	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/ready", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("/ready status = %d, want %d (disabled)", w.Code, http.StatusNotFound)
	}
}

func TestUseHealth_ReadinessOnly(t *testing.T) {
	app := mustNew(t)
	app.UseHealth(credo.HealthConfig{DisableLiveness: true})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("/health status = %d, want %d (disabled)", w.Code, http.StatusNotFound)
	}

	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/ready", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("/ready status = %d, want %d", w.Code, http.StatusOK)
	}
}

// --- Access-log silencing (HealthConfig.LogRequests) ---

func TestUseHealth_DefaultSilencesAccessLog(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger))
	buf.Reset() // drop the record of the automatic configuration load
	app.UseHealth()

	for _, path := range []string{"/health", "/ready"} {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, w.Code)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("expected no access log for health probes by default, got: %s", buf.String())
	}
}

func TestUseHealth_DefaultSilencesHeadProbes(t *testing.T) {
	logger, buf := newTestLogger(t)

	// SetMeta on the GET route propagates to its auto-generated HEAD twin, so
	// HEAD probes must be silenced too.
	app := mustNew(t, credo.WithLogger(logger))
	buf.Reset() // drop the record of the automatic configuration load
	app.UseHealth()

	for _, path := range []string{"/health", "/ready"} {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("HEAD", path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("HEAD %s status = %d, want 200", path, w.Code)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("expected no access log for HEAD health probes, got: %s", buf.String())
	}
}

func TestUseHealth_LogRequestsEnablesAccessLog(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger))
	app.UseAccessLog()
	app.UseHealth(credo.HealthConfig{LogRequests: true})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	if buf.Len() == 0 {
		t.Error("expected access log for GET health probe when LogRequests is true")
	}

	buf.Reset()
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("HEAD", "/health", nil))
	if buf.Len() == 0 {
		t.Error("expected access log for HEAD health probe when LogRequests is true (twin propagation)")
	}
}

func TestUseHealth_GroupDefaultSilencesAccessLog(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger))
	buf.Reset() // drop the record of the automatic configuration load
	g := app.Group("/sys")
	app.UseHealth(credo.HealthConfig{Group: g})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/sys/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /sys/health status = %d, want 200", w.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no access log for group-registered health probe by default, got: %s", buf.String())
	}
}

func TestUseHealth_LogRequestsOverridesSilencedGroup(t *testing.T) {
	logger, buf := newTestLogger(t)

	// Health routes under a group that silenced access logging, but
	// LogRequests:true sets the meta at the route level, which overrides the
	// group's false (LookupMeta reads the route before its parents).
	app := mustNew(t, credo.WithLogger(logger))
	app.UseAccessLog()
	g := app.Group("/sys")
	g.SetMeta(credo.MetaAccessLog, false)
	app.UseHealth(credo.HealthConfig{Group: g, LogRequests: true})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/sys/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /sys/health status = %d, want 200", w.Code)
	}
	if buf.Len() == 0 {
		t.Error("expected access log: LogRequests:true must override a silenced parent group")
	}
}

func TestLiveness_NoChecks_Up(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["status"] != "up" {
		t.Errorf("status = %q, want %q", body["status"], "up")
	}
}

func TestLiveness_AllPass(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()
	app.AddLivenessCheck("ok", func(context.Context) error {
		return nil
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestLiveness_OneFails(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()
	app.AddLivenessCheck("bad", func(context.Context) error {
		return errors.New("disk full")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["status"] != "down" {
		t.Errorf("status = %q, want %q", body["status"], "down")
	}
}

func TestReadiness_NoChecks_Up(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ready", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["status"] != "up" {
		t.Errorf("status = %v, want %q", body["status"], "up")
	}
}

func TestReadiness_CheckFails(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()
	app.AddReadinessCheck("db", func(context.Context) error {
		return errors.New("connection refused")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ready", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestReadiness_ErrorsMaskedByDefault(t *testing.T) {
	logger, logs := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	app.UseHealth()
	app.AddReadinessCheck("db", func(context.Context) error {
		return errors.New("dial tcp 10.0.1.5:5432: connection refused")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ready", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	body := w.Body.String()
	if strings.Contains(body, "connection refused") || strings.Contains(body, "10.0.1.5") {
		t.Errorf("readiness body leaks check error detail: %s", body)
	}

	var parsed map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	checks := parsed["checks"].(map[string]any)
	db := checks["db"].(map[string]any)
	if db["status"] != "down" {
		t.Errorf("db status = %v, want down", db["status"])
	}
	if !strings.Contains(logs.String(), "connection refused") || !strings.Contains(logs.String(), `"check":"db"`) {
		t.Errorf("masked named-check cause must remain in operator log: %s", logs.String())
	}
}

func TestReadiness_ExposeErrorsOptIn(t *testing.T) {
	app := mustNew(t)
	app.UseHealth(credo.HealthConfig{ExposeErrors: true})
	app.AddReadinessCheck("db", func(context.Context) error {
		return errors.New("connection refused")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ready", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(w.Body.String(), "connection refused") {
		t.Errorf("ExposeErrors: readiness body should include the error, got: %s", w.Body.String())
	}
}

func TestReadiness_StoreIntegration(t *testing.T) {
	// A bound store's entry is its Ready, under its credo.ComponentName name.
	app := mustNew(t)
	registerReadinessStore(app, "postgres", store.Health{Status: store.StatusUp, Latency: 2 * time.Millisecond})
	app.UseHealth()
	startServing(t, app)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ready", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatal("expected checks map in response")
	}
	pg, ok := checks["postgres"].(map[string]any)
	if !ok {
		t.Fatal("expected postgres entry in checks")
	}
	if pg["status"] != "up" {
		t.Errorf("postgres status = %v, want %q", pg["status"], "up")
	}
}

func TestReadiness_StoreFailureLoggedAndMasked(t *testing.T) {
	const secret = "dial tcp 10.0.1.5:5432: connection refused"
	tests := []struct {
		name          string
		exposeErrors  bool
		wantBodyCause bool
	}{
		{name: "masked by default"},
		{name: "explicitly exposed", exposeErrors: true, wantBodyCause: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := newTestLogger(t)
			app := mustNew(t, credo.WithLogger(logger))
			registerReadinessStore(app, "postgres", store.Health{
				Status:  store.StatusDown,
				Latency: 2 * time.Millisecond,
				Cause:   errors.New(secret),
			})
			app.UseHealth(credo.HealthConfig{ExposeErrors: tt.exposeErrors})
			startServing(t, app)

			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
			}
			if got := strings.Contains(w.Body.String(), secret); got != tt.wantBodyCause {
				t.Errorf("body contains cause = %v, want %v; body: %s", got, tt.wantBodyCause, w.Body.String())
			}
			if !strings.Contains(logs.String(), secret) || !strings.Contains(logs.String(), `"check":"postgres"`) ||
				!strings.Contains(logs.String(), `"latency":`) {
				t.Errorf("operator log must contain the typed store cause, the check and its latency; log: %s",
					logs.String())
			}
		})
	}
}

func TestReadiness_CustomStoreNameCollisionDoesNotOverwrite(t *testing.T) {
	app := mustNew(t)
	registerReadinessStore(app, "database", store.Health{Status: store.StatusUp})
	app.UseHealth()
	app.AddReadinessCheck("database", func(context.Context) error { return nil })
	startServing(t, app)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}

	var body struct {
		Checks map[string]struct {
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := body.Checks["database"].Status; got != "up" {
		t.Fatalf("named result was overwritten: status = %q, want up", got)
	}
	foundConflict := false
	for name, result := range body.Checks {
		if strings.HasPrefix(name, "credo.contributed_name_conflict.") && result.Status == "down" {
			foundConflict = true
		}
	}
	if !foundConflict {
		t.Fatalf("checks = %#v, want explicit down collision entry", body.Checks)
	}
}

func TestAddReadinessCheck_DuplicateNamePanics(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()
	check := func(context.Context) error { return nil }
	app.AddReadinessCheck("database", check)

	defer func() {
		if recovered := recover(); recovered == nil || !strings.Contains(fmt.Sprint(recovered), "duplicate readiness check") {
			t.Fatalf("panic = %v, want duplicate readiness check", recovered)
		}
	}()
	app.AddReadinessCheck("database", check)
}

// Checks are collected whenever they are registered: the endpoints serve a
// check added before UseHealth exactly as they serve one added after it.
func TestHealthChecks_RegisteredInAnyOrder(t *testing.T) {
	tests := []struct {
		name     string
		register func(app *credo.App, mount func(), liveness, readiness func(context.Context) error)
	}{
		{"checks before UseHealth", func(app *credo.App, mount func(), liveness, readiness func(context.Context) error) {
			app.AddLivenessCheck("disk", liveness)
			app.AddReadinessCheck("database", readiness)
			mount()
		}},
		{"checks after UseHealth", func(app *credo.App, mount func(), liveness, readiness func(context.Context) error) {
			mount()
			app.AddLivenessCheck("disk", liveness)
			app.AddReadinessCheck("database", readiness)
		}},
		{"UseHealth between the checks", func(app *credo.App, mount func(), liveness, readiness func(context.Context) error) {
			app.AddLivenessCheck("disk", liveness)
			mount()
			app.AddReadinessCheck("database", readiness)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			var livenessRuns, readinessRuns atomic.Int32
			tt.register(app,
				func() { app.UseHealth() },
				func(context.Context) error { livenessRuns.Add(1); return errors.New("disk full") },
				func(context.Context) error { readinessRuns.Add(1); return errors.New("unreachable") },
			)

			for _, path := range []string{"/health", "/ready"} {
				w := httptest.NewRecorder()
				app.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
				if w.Code != http.StatusServiceUnavailable {
					t.Errorf("%s status = %d, want %d: the failing check must be served", path, w.Code, http.StatusServiceUnavailable)
				}
			}
			if l, r := livenessRuns.Load(), readinessRuns.Load(); l != 1 || r != 1 {
				t.Errorf("runs = (liveness %d, readiness %d), want (1, 1)", l, r)
			}
		})
	}
}

// A check registered before UseHealth is run under the timeout UseHealth
// configures, not under the default the engine was built with.
func TestHealthChecks_RegisteredBeforeUseHealth_UseItsTimeout(t *testing.T) {
	app := mustNew(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	app.AddReadinessCheck("slow", func(ctx context.Context) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return ctx.Err()
	})
	app.UseHealth(credo.HealthConfig{CheckTimeout: 20 * time.Millisecond})

	began := time.Now()
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/ready", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/ready status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Errorf("/ready took %v, want it bounded by the configured 20ms CheckTimeout, not the 5s default", elapsed)
	}
}

// An App that registers checks and never calls UseHealth starts, and its
// start logs one warning that names how many checks no endpoint runs and
// the call that mounts them.
func TestHealthChecks_WithoutUseHealth_StartWarnsOnce(t *testing.T) {
	check := func(context.Context) error { return nil }
	tests := []struct {
		name      string
		register  func(*credo.App)
		wantWarn  bool
		liveness  float64
		readiness float64
	}{
		{"checks and no UseHealth", func(a *credo.App) {
			a.AddLivenessCheck("disk", check)
			a.AddReadinessCheck("database", check)
			a.AddReadinessCheck("cache", check, credo.OptionalReadiness())
		}, true, 1, 2},
		{"no checks and no UseHealth", func(*credo.App) {}, false, 0, 0},
		{"checks and UseHealth", func(a *credo.App) {
			a.AddReadinessCheck("database", check)
			a.UseHealth()
		}, false, 0, 0},
		{"checks and a disabled UseHealth", func(a *credo.App) {
			a.AddReadinessCheck("database", check)
			a.UseHealth(credo.HealthConfig{Disabled: true})
		}, false, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, buf := newTestLogger(t)
			app := mustNew(t, credo.WithLogger(logger))
			tt.register(app)
			startServing(t, app)

			var warnings []map[string]any
			for _, entry := range parseJSONLines(t, buf.Bytes()) {
				if msg, _ := entry["msg"].(string); strings.Contains(msg, "UseHealth was never called") {
					warnings = append(warnings, entry)
				}
			}
			if !tt.wantWarn {
				if len(warnings) != 0 {
					t.Fatalf("warnings = %v, want none", warnings)
				}
				return
			}
			if len(warnings) != 1 {
				t.Fatalf("got %d warnings, want exactly 1: %v", len(warnings), warnings)
			}
			w := warnings[0]
			if w["level"] != "WARN" {
				t.Errorf("level = %v, want WARN", w["level"])
			}
			msg, _ := w["msg"].(string)
			for _, want := range []string{
				"health checks are registered but UseHealth was never called", // what failed
				"call app.UseHealth()", // the remedy
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("msg = %q, want it to contain %q", msg, want)
				}
			}
			if w["checks"] != tt.liveness+tt.readiness || w["liveness"] != tt.liveness || w["readiness"] != tt.readiness {
				t.Errorf("counts = (checks %v, liveness %v, readiness %v), want (%v, %v, %v)",
					w["checks"], w["liveness"], w["readiness"], tt.liveness+tt.readiness, tt.liveness, tt.readiness)
			}

			// The checks are accepted and no endpoint serves them.
			for _, path := range []string{"/health", "/ready"} {
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s status = %d, want %d", path, rec.Code, http.StatusNotFound)
				}
			}
		})
	}
}

// UseHealth is called once per App. A call with Disabled set takes the
// registration like any other, so the second call is refused whichever of the
// two is disabled.
func TestUseHealth_DoubleCall_Panics(t *testing.T) {
	enabled := credo.HealthConfig{}
	disabled := credo.HealthConfig{Disabled: true}
	tests := []struct {
		name          string
		first, second credo.HealthConfig
	}{
		{"enabled then enabled", enabled, enabled},
		{"enabled then disabled", enabled, disabled},
		{"disabled then enabled", disabled, enabled},
		{"disabled then disabled", disabled, disabled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.UseHealth(tt.first)
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("expected panic on second UseHealth call")
				}
				const want = "credo: App.UseHealth called twice; call it once with one HealthConfig, " +
					"and register checks with AddLivenessCheck and AddReadinessCheck, before or after it"
				if msg, ok := r.(string); !ok || msg != want {
					t.Errorf("panic = %v, want %q", r, want)
				}
			}()
			app.UseHealth(tt.second)
		})
	}
}

// A nil check panics at the call, before or after UseHealth, naming the
// check and what to pass.
func TestAddCheck_NilCheck_Panics(t *testing.T) {
	tests := []struct {
		name string
		add  func(*credo.App)
		want string
	}{
		{"liveness", func(a *credo.App) { a.AddLivenessCheck("disk", nil) },
			`credo: AddLivenessCheck: the check function of "disk" is nil; pass a func(context.Context) error`},
		{"readiness", func(a *credo.App) { a.AddReadinessCheck("database", nil) },
			`credo: AddReadinessCheck: the check function of "database" is nil; pass a func(context.Context) error`},
	}
	for _, tt := range tests {
		for _, mounted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mounted=%v", tt.name, mounted), func(t *testing.T) {
				app := mustNew(t)
				if mounted {
					app.UseHealth()
				}
				if got := mustPanicMessage(t, func() { tt.add(app) }); got != tt.want {
					t.Errorf("panic = %q, want %q", got, tt.want)
				}
			})
		}
	}
}

func TestUseHealth_NegativeTimeout_DefaultApplied(t *testing.T) {
	app := mustNew(t)
	app.UseHealth(credo.HealthConfig{CheckTimeout: -1 * time.Second})

	// Should work fine with default timeout, not fail all checks.
	app.AddLivenessCheck("ok", func(context.Context) error {
		return nil
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (negative timeout should default to 5s)", w.Code, http.StatusOK)
	}
}

func TestUseHealth_AfterFrozen_Panics(t *testing.T) {
	app := mustNew(t)
	// Trigger compile by calling ServeHTTP.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/noop", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic from UseHealth after compile")
		}
	}()
	app.UseHealth()
}

// pingChecker is a value with a Check method, the shape a check had as an
// interface; it is registered as its method value.
type pingChecker struct{ err error }

func (p pingChecker) Check(context.Context) error { return p.err }

func TestAddCheck_MethodValue(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()
	app.AddLivenessCheck("disk", pingChecker{}.Check)
	app.AddReadinessCheck("database", pingChecker{err: errors.New("unreachable")}.Check)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != http.StatusOK {
		t.Errorf("/health status = %d, want %d", w.Code, http.StatusOK)
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/ready", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("/ready status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestUseHealth_WithGroup(t *testing.T) {
	app := mustNew(t)
	ops := app.Group("/-")
	app.UseHealth(credo.HealthConfig{
		Group:         ops,
		LivenessPath:  "/healthz",
		ReadinessPath: "/ready",
	})

	// /-/healthz should work
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/-/healthz", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("/-/healthz status = %d, want 200", w.Code)
	}

	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "up" {
		t.Errorf("status = %q, want %q", body["status"], "up")
	}

	// /health (root) should NOT work
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/health", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("/health status = %d, want 404", w.Code)
	}
}

func TestUseHealth_WithGroup_Middleware(t *testing.T) {
	app := mustNew(t)

	// Middleware that sets a custom header.
	tagMW := func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("X-Ops", "true")
			return next(ctx)
		}
	}

	ops := app.Group("/-").Middleware(tagMW)
	app.UseHealth(credo.HealthConfig{
		Group:        ops,
		LivenessPath: "/healthz",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/-/healthz", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Header().Get("X-Ops") != "true" {
		t.Error("group middleware did not execute on health route")
	}
}

// A group of another App is refused whether or not the config disables
// health, so the mistake shows in every deployment and not only in the one
// that turns health on. The panic names the field and both remedies.
func TestUseHealth_WithGroup_WrongApp_Panics(t *testing.T) {
	const want = "credo: UseHealth: HealthConfig.Group belongs to a different App; " +
		"pass a group created on this App, or leave Group nil to mount the endpoints on the app root"
	tests := []struct {
		name string
		cfg  credo.HealthConfig
	}{
		{"enabled", credo.HealthConfig{}},
		{"disabled", credo.HealthConfig{Disabled: true}},
		{"both endpoints off", credo.HealthConfig{DisableLiveness: true, DisableReadiness: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, other := mustNew(t), mustNew(t)
			cfg := tt.cfg
			cfg.Group = other.Group("/-")
			if got := mustPanicMessage(t, func() { app.UseHealth(cfg) }); got != want {
				t.Errorf("panic = %q, want %q", got, want)
			}
			// The refused call did not take the registration.
			app.UseHealth()
		})
	}
}

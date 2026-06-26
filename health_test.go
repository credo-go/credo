package credo_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
	internalhealth "github.com/credo-go/credo/internal/health"
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
	app.UseHealth(credo.HealthConfig{Enabled: new(false)})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("/health status = %d, want %d (disabled)", w.Code, http.StatusNotFound)
	}
}

func TestUseHealth_LivenessOnly(t *testing.T) {
	app := mustNew(t)
	app.UseHealth(credo.HealthConfig{Readiness: new(false)})

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
	app.UseHealth(credo.HealthConfig{Liveness: new(false)})

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

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
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
	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
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

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
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

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
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
	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
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
	app.AddLivenessCheck("ok", credo.HealthCheckFunc(func(context.Context) error {
		return nil
	}))

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
	app.AddLivenessCheck("bad", credo.HealthCheckFunc(func(context.Context) error {
		return errors.New("disk full")
	}))

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
	app.AddReadinessCheck("db", credo.HealthCheckFunc(func(context.Context) error {
		return errors.New("connection refused")
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ready", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestReadiness_ErrorsMaskedByDefault(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()
	app.AddReadinessCheck("db", credo.HealthCheckFunc(func(context.Context) error {
		return errors.New("dial tcp 10.0.1.5:5432: connection refused")
	}))

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
}

func TestReadiness_ExposeErrorsOptIn(t *testing.T) {
	app := mustNew(t)
	app.UseHealth(credo.HealthConfig{ExposeErrors: true})
	app.AddReadinessCheck("db", credo.HealthCheckFunc(func(context.Context) error {
		return errors.New("connection refused")
	}))

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
	// Store health arrives through the module-internal DI seam, the same
	// way store.Register provides it.
	app := mustNew(t)
	err := app.ProvideValue[internalhealth.StoreFunc](func(context.Context) []internalhealth.StoreResult {
		return []internalhealth.StoreResult{
			{Name: "postgres", Status: "up", Latency: 2 * time.Millisecond},
		}
	})
	if err != nil {
		t.Fatalf("ProvideValue: %v", err)
	}
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

func TestAddLivenessCheck_NoUseHealth_Panics(t *testing.T) {
	app := mustNew(t)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		msg, ok := r.(string)
		if !ok || msg != "credo: UseHealth() must be called before AddLivenessCheck" {
			t.Errorf("panic = %v, want specific message", r)
		}
	}()
	app.AddLivenessCheck("x", credo.HealthCheckFunc(func(context.Context) error { return nil }))
}

func TestAddReadinessCheck_NoUseHealth_Panics(t *testing.T) {
	app := mustNew(t)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		msg, ok := r.(string)
		if !ok || msg != "credo: UseHealth() must be called before AddReadinessCheck" {
			t.Errorf("panic = %v, want specific message", r)
		}
	}()
	app.AddReadinessCheck("x", credo.HealthCheckFunc(func(context.Context) error { return nil }))
}

func TestUseHealth_DoubleCall_Panics(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on second UseHealth call")
		}
		msg, ok := r.(string)
		if !ok || msg != "credo: UseHealth already called" {
			t.Errorf("panic = %v, want %q", r, "credo: UseHealth already called")
		}
	}()
	app.UseHealth()
}

func TestAddLivenessCheck_NilChecker_Panics(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for nil checker")
		}
		msg, ok := r.(string)
		if !ok || msg != "credo: AddLivenessCheck: checker must not be nil" {
			t.Errorf("panic = %v, want specific message", r)
		}
	}()
	app.AddLivenessCheck("bad", nil)
}

func TestAddReadinessCheck_NilChecker_Panics(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for nil checker")
		}
		msg, ok := r.(string)
		if !ok || msg != "credo: AddReadinessCheck: checker must not be nil" {
			t.Errorf("panic = %v, want specific message", r)
		}
	}()
	app.AddReadinessCheck("bad", nil)
}

func TestUseHealth_NegativeTimeout_DefaultApplied(t *testing.T) {
	app := mustNew(t)
	app.UseHealth(credo.HealthConfig{CheckTimeout: -1 * time.Second})

	// Should work fine with default timeout, not fail all checks.
	app.AddLivenessCheck("ok", credo.HealthCheckFunc(func(context.Context) error {
		return nil
	}))

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

func TestHealthCheckFunc_Adapter(t *testing.T) {
	var called bool
	f := credo.HealthCheckFunc(func(context.Context) error {
		called = true
		return nil
	})

	var checker credo.HealthChecker = f
	if err := checker.Check(t.Context()); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
	if !called {
		t.Error("function was not called")
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

func TestUseHealth_WithGroup_WrongApp_Panics(t *testing.T) {
	app1 := mustNew(t)
	app2 := mustNew(t)

	group := app2.Group("/-")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for Group from different App")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "different App") {
			t.Errorf("panic = %v, want message about different App", r)
		}
	}()

	app1.UseHealth(credo.HealthConfig{Group: group})
}

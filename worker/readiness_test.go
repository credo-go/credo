package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

func awaitCondition(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWithReadiness_Validation(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want string
	}{
		{"empty policy", []Option{WithReadiness(ReadinessPolicy{})}, "at least one condition"},
		{"negative age", []Option{WithSchedule("* * * * *"), WithReadiness(ReadinessPolicy{MaxSuccessAge: -time.Second})}, "must be >= 0"},
		{"first success on continuous", []Option{WithReadiness(ReadinessPolicy{RequireFirstSuccess: true})}, "for scheduled workers"},
		{"max age on continuous", []Option{WithReadiness(ReadinessPolicy{MaxSuccessAge: time.Minute})}, "for scheduled workers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := validateOptions(tt.opts)
			requireErrContaining(t, err, tt.want)
		})
	}

	if _, _, err := validateOptions([]Option{WithReadiness(ReadinessPolicy{FailWhenFailed: true})}); err != nil {
		t.Fatalf("FailWhenFailed on a continuous worker should be valid, got %v", err)
	}
}

func TestReadiness_FirstSuccessBarrier(t *testing.T) {
	p := newTestPool()
	w := Func("recover", func(context.Context) error { return nil })
	o, schedule, err := validateOptions([]Option{
		WithSchedule("@every 1h"), WithStartImmediately(),
		WithReadiness(ReadinessPolicy{RequireFirstSuccess: true}),
	})
	if err != nil {
		t.Fatalf("validateOptions: %v", err)
	}
	def := buildDefinition("recover", w, o, schedule, DefaultRestartDelay)
	if err := p.addDefinition(def); err != nil {
		t.Fatalf("addDefinition: %v", err)
	}

	checks := p.readinessChecks()
	if len(checks) != 1 || checks[0].Name != "worker:recover" || checks[0].Probe == nil {
		t.Fatalf("readinessChecks() = %+v, want one worker:recover probe", checks)
	}
	if err := p.evaluateReadiness(def); err == nil || !strings.Contains(err.Error(), "has not started") {
		t.Fatalf("before Start: err = %v, want not-started", err)
	}
	if res := checks[0].Probe.Run(t.Context(), time.Second); res.Status != "down" {
		t.Fatalf("probe before Start = %+v, want down", res)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitCondition(t, "first success", func() bool { return p.evaluateReadiness(def) == nil })
	if info := p.Workers()[0]; info.LastSuccess.IsZero() {
		t.Fatalf("Info.LastSuccess is zero after a successful run: %+v", info)
	}
	shutdownPool(t, p)
}

func TestReadiness_FailWhenFailed(t *testing.T) {
	p := newTestPool()
	w := Func("critical", func(context.Context) error { return errors.New("boom") })
	o, schedule, err := validateOptions([]Option{
		WithMaxRestarts(1), WithRestartDelay(time.Millisecond),
		WithReadiness(ReadinessPolicy{FailWhenFailed: true}),
	})
	if err != nil {
		t.Fatalf("validateOptions: %v", err)
	}
	def := buildDefinition("critical", w, o, schedule, DefaultRestartDelay)
	if err := p.addDefinition(def); err != nil {
		t.Fatalf("addDefinition: %v", err)
	}
	if err := p.evaluateReadiness(def); err != nil {
		t.Fatalf("before Start: err = %v, want ready (FailWhenFailed only)", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitCondition(t, "permanent failure", func() bool {
		err := p.evaluateReadiness(def)
		return err != nil && strings.Contains(err.Error(), "failed permanently")
	})
	shutdownPool(t, p)
}

func TestReadiness_MaxSuccessAge(t *testing.T) {
	p := newTestPool()
	w := Func("sync", func(context.Context) error { return nil })
	o, schedule, err := validateOptions([]Option{
		WithSchedule("@every 1h"), WithStartImmediately(),
		WithReadiness(ReadinessPolicy{MaxSuccessAge: time.Hour}),
	})
	if err != nil {
		t.Fatalf("validateOptions: %v", err)
	}
	def := buildDefinition("sync", w, o, schedule, DefaultRestartDelay)
	if err := p.addDefinition(def); err != nil {
		t.Fatalf("addDefinition: %v", err)
	}
	if err := p.evaluateReadiness(def); err != nil {
		t.Fatalf("before first success: err = %v, want ready (age not applied yet)", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitCondition(t, "first success", func() bool { return !p.Workers()[0].LastSuccess.IsZero() })
	if err := p.evaluateReadiness(def); err != nil {
		t.Fatalf("fresh success: err = %v, want ready", err)
	}

	p.mu.Lock()
	r := p.runners[0]
	p.mu.Unlock()
	r.update(func(r *runner) { r.lastSuccess = time.Now().Add(-2 * time.Hour) })
	if err := p.evaluateReadiness(def); err == nil || !strings.Contains(err.Error(), "last succeeded") {
		t.Fatalf("stale success: err = %v, want age violation", err)
	}
	shutdownPool(t, p)
}

func TestReadiness_AppIntegration_OrderIndependent(t *testing.T) {
	app := newTestApp(t)

	// Register before UseHealth: the seam is resolved lazily, so order is free.
	err := Register(app, Func("warmup", func(context.Context) error { return nil }),
		WithSchedule("@every 1h"), WithStartImmediately(),
		WithReadiness(ReadinessPolicy{RequireFirstSuccess: true}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	app.UseHealth()

	ready := func() (int, string) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
		return w.Code, w.Body.String()
	}
	if code, body := ready(); code != http.StatusServiceUnavailable || !strings.Contains(body, "worker:warmup") {
		t.Fatalf("before Start: /ready = %d %s, want 503 with worker:warmup", code, body)
	}

	pool, err := app.Resolve[*Pool]()
	if err != nil {
		t.Fatalf("Resolve[*Pool]: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitCondition(t, "/ready to turn 200", func() bool {
		code, _ := ready()
		return code == http.StatusOK
	})
	shutdownPool(t, pool)
}

func TestRegister_ReadinessNameCollisionFailsClosed(t *testing.T) {
	app := newTestApp(t)
	app.UseHealth()
	app.AddReadinessCheck("worker:dup", credo.HealthCheckFunc(func(context.Context) error { return nil }))
	if err := Register(app, Func("dup", func(context.Context) error { return nil }),
		WithReadiness(ReadinessPolicy{FailWhenFailed: true})); err != nil {
		t.Fatalf("Register: %v", err)
	}

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "contributed_name_conflict") {
		t.Fatalf("/ready = %d %s, want 503 with a contributed_name_conflict entry", w.Code, w.Body.String())
	}
}

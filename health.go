package credo

import (
	"context"
	"net/http"
	"time"

	internalhealth "github.com/credo-go/credo/internal/health"
)

// HealthConfig configures the health check endpoints.
type HealthConfig struct {
	// Enabled controls whether health endpoints are registered.
	// nil (default) = true.
	Enabled *bool

	// Liveness controls whether the liveness endpoint is registered.
	// nil (default) = true.
	Liveness *bool

	// Readiness controls whether the readiness endpoint is registered.
	// nil (default) = true.
	Readiness *bool

	// LivenessPath is the path for the liveness endpoint. Default: "/health".
	LivenessPath string

	// ReadinessPath is the path for the readiness endpoint. Default: "/ready".
	ReadinessPath string

	// CheckTimeout is the per-check timeout. Default: 5s.
	CheckTimeout time.Duration

	// ExposeErrors includes check error strings in the readiness response
	// body. Default false: failing checks report only "down" and the error
	// is written to the application log instead — error strings often
	// carry internal details (hostnames, connection targets) that should
	// not reach unauthenticated probe endpoints.
	ExposeErrors bool

	// Group registers health routes on a specific route group instead of the
	// app root. Routes inherit the group's prefix and middleware chain.
	// nil (default) = routes are registered on the app root.
	Group *Group
}

// HealthChecker checks the health of a component.
type HealthChecker interface {
	Check(ctx context.Context) error // nil = healthy
}

// HealthCheckFunc adapts a plain function to HealthChecker.
type HealthCheckFunc func(ctx context.Context) error

// Check implements HealthChecker.
func (f HealthCheckFunc) Check(ctx context.Context) error { return f(ctx) }

// UseHealth initializes health check endpoints on the application.
// With no arguments, it registers both /health (liveness) and /ready (readiness).
//
// UseHealth performs no I/O — it only registers in-process state — so misuse
// panics like every other registration API (contrast [App.UseI18n], which
// reads locale files and therefore returns an error). Panics if called more
// than once, if called after compile, or if cfg.Group belongs to a
// different App.
func (app *App) UseHealth(cfgs ...HealthConfig) {
	app.checkFrozen("UseHealth")
	if app.healthEngine != nil {
		panic("credo: UseHealth already called")
	}

	var cfg HealthConfig
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}

	// Apply defaults.
	if cfg.Enabled != nil && !*cfg.Enabled {
		return
	}
	if cfg.LivenessPath == "" {
		cfg.LivenessPath = "/health"
	}
	if cfg.ReadinessPath == "" {
		cfg.ReadinessPath = "/ready"
	}
	if cfg.CheckTimeout <= 0 {
		cfg.CheckTimeout = 5 * time.Second
	}

	app.healthEngine = newHealthEngine(cfg.CheckTimeout)
	app.healthExposeErrors = cfg.ExposeErrors

	// Validate Group belongs to this app.
	if cfg.Group != nil && cfg.Group.app != app {
		panic("credo: UseHealth: Group belongs to a different App")
	}

	// Register endpoints on the specified group or app root.
	registerGET := func(path string, h Handler) *Route {
		if cfg.Group != nil {
			return cfg.Group.GET(path, h)
		}
		return app.GET(path, h)
	}

	livenessEnabled := cfg.Liveness == nil || *cfg.Liveness
	readinessEnabled := cfg.Readiness == nil || *cfg.Readiness

	if livenessEnabled {
		registerGET(cfg.LivenessPath, app.livenessHandler).Name("credo.health")
	}
	if readinessEnabled {
		registerGET(cfg.ReadinessPath, app.readinessHandler).Name("credo.ready")
	}
}

// AddLivenessCheck registers a named liveness check.
// Panics if [App.UseHealth] has not been called first or if checker is nil.
func (app *App) AddLivenessCheck(name string, checker HealthChecker) {
	if app.healthEngine == nil {
		panic("credo: UseHealth() must be called before AddLivenessCheck")
	}
	if checker == nil {
		panic("credo: AddLivenessCheck: checker must not be nil")
	}
	app.healthEngine.addLiveness(name, checker.Check)
}

// AddReadinessCheck registers a named readiness check.
// Panics if [App.UseHealth] has not been called first or if checker is nil.
func (app *App) AddReadinessCheck(name string, checker HealthChecker) {
	if app.healthEngine == nil {
		panic("credo: UseHealth() must be called before AddReadinessCheck")
	}
	if checker == nil {
		panic("credo: AddReadinessCheck: checker must not be nil")
	}
	app.healthEngine.addReadiness(name, checker.Check)
}

// storeHealthFunc returns the store-health collector contributed by the
// store integration (provided into the DI container under the
// module-internal [internalhealth.StoreFunc] type), or nil when no stores
// are registered. Resolved lazily on each readiness check so the relative
// order of store.Register and UseHealth does not matter.
func (app *App) storeHealthFunc() internalhealth.StoreFunc {
	fn, err := Resolve[internalhealth.StoreFunc](app)
	if err != nil {
		return nil
	}
	return fn
}

// livenessHandler returns 200/503 with a JSON status body.
func (app *App) livenessHandler(ctx *Context) error {
	status, checks := app.healthEngine.checkLiveness(ctx.Request().Context())
	code := http.StatusOK
	if status != "up" {
		code = http.StatusServiceUnavailable
		app.logFailedChecks("liveness", checks)
	}
	return ctx.Response().JSON(code, map[string]string{"status": status})
}

// readinessHandler returns 200/503 with a JSON body including check details.
// Check error strings are masked unless HealthConfig.ExposeErrors is set;
// failures are logged instead.
func (app *App) readinessHandler(ctx *Context) error {
	status, checks, stores := app.healthEngine.checkReadiness(ctx.Request().Context(), app.storeHealthFunc())
	code := http.StatusOK
	if status != "up" {
		code = http.StatusServiceUnavailable
		app.logFailedChecks("readiness", checks)
	}

	checksMap := make(map[string]any, len(checks)+len(stores))
	for _, c := range checks {
		entry := map[string]string{"status": c.Status}
		if c.Error != "" && app.healthExposeErrors {
			entry["error"] = c.Error
		}
		checksMap[c.Name] = entry
	}
	for _, s := range stores {
		checksMap[s.Name] = map[string]string{
			"status":  s.Status,
			"latency": s.Latency.String(),
		}
	}

	body := map[string]any{"status": status}
	if len(checksMap) > 0 {
		body["checks"] = checksMap
	}
	return ctx.Response().JSON(code, body)
}

// logFailedChecks logs failing health checks so operators keep the error
// detail even when it is masked from the HTTP response.
func (app *App) logFailedChecks(kind string, checks []healthCheckResult) {
	for _, c := range checks {
		if c.Status != "up" {
			app.logger.Warn("credo: health check failed",
				"kind", kind, "check", c.Name, "error", c.Error)
		}
	}
}

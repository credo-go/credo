package credo

import (
	"context"
	"fmt"
	"net/http"
	"time"

	internalhealth "github.com/credo-go/credo/internal/health"
)

// HealthConfig configures the health check endpoints. Its zero value mounts
// both endpoints at their default paths; each switch turns a default off.
type HealthConfig struct {
	// Disabled mounts no endpoint. The call still counts as the App's
	// UseHealth, so an application can map a configuration key to Disabled
	// and keep its UseHealth call and its check registrations unconditional:
	// the checks are accepted and never run.
	Disabled bool

	// DisableLiveness leaves the liveness endpoint unmounted. Liveness checks
	// are still accepted and never run.
	DisableLiveness bool

	// DisableReadiness leaves the readiness endpoint unmounted. Readiness
	// checks are still accepted and never run.
	DisableReadiness bool

	// LivenessPath is the path for the liveness endpoint. Default: "/health".
	LivenessPath string

	// ReadinessPath is the path for the readiness endpoint. Default: "/ready".
	ReadinessPath string

	// CheckTimeout is the enforced per-check timeout. Default: 5s. A callback
	// that ignores cancellation cannot delay the response beyond this budget;
	// overlapping requests share its still-running execution until it exits.
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

	// LogRequests includes health and readiness probe requests in the access
	// log. Default false: probe traffic (such as Kubernetes liveness and
	// readiness checks) is silenced via the MetaAccessLog route meta to keep
	// logs clean. The endpoints remain registered and responsive either way.
	//
	// The meta is set at the route level, so LogRequests:true re-enables
	// logging even when the health routes sit under a group that silenced
	// access logging (a route-level value overrides its group's).
	LogRequests bool
}

// defaultHealthCheckTimeout is the per-check deadline when
// HealthConfig.CheckTimeout is not set.
const defaultHealthCheckTimeout = 5 * time.Second

// UseHealth mounts the health check endpoints on the application.
// With no arguments, it mounts both /health (liveness) and /ready (readiness).
// The endpoints run the checks added with [App.AddLivenessCheck] and
// [App.AddReadinessCheck], whether a check was added before this call or
// after it.
//
// UseHealth performs no I/O — it only registers in-process state — and misuse
// panics like every other registration API. Panics if called more than once,
// a call with Disabled set included; if called after compile; or if cfg.Group
// belongs to a different App, whether or not Disabled is set.
func (app *App) UseHealth(cfgs ...HealthConfig) {
	app.checkFrozen("App.UseHealth")
	if app.healthMounted {
		panic("credo: UseHealth already called")
	}

	var cfg HealthConfig
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}

	// The Group is checked before the Disabled switch: a group of another App
	// is a mistake in every deployment, not only in the one that turns health
	// on.
	if cfg.Group != nil && cfg.Group.registrar != routeRegistrar(app) {
		panic("credo: UseHealth: HealthConfig.Group belongs to a different App; " +
			"pass a group created on this App, or leave Group nil to mount the endpoints on the app root")
	}

	// Apply defaults.
	if cfg.LivenessPath == "" {
		cfg.LivenessPath = "/health"
	}
	if cfg.ReadinessPath == "" {
		cfg.ReadinessPath = "/ready"
	}
	if cfg.CheckTimeout <= 0 {
		cfg.CheckTimeout = defaultHealthCheckTimeout
	}

	app.healthMounted = true
	app.healthEngine.SetTimeout(cfg.CheckTimeout)
	app.healthExposeErrors = cfg.ExposeErrors

	if cfg.Disabled {
		return
	}

	// Register endpoints on the specified group or app root.
	registerGET := func(path string, h Handler) *Route {
		if cfg.Group != nil {
			return cfg.Group.GET(path, h)
		}
		return app.GET(path, h)
	}

	// Silence probe access logging by default; LogRequests re-enables it. The
	// meta is always set (not only when false) so an explicit true overrides a
	// silenced parent group — GetMeta reads the route before its parents.
	// SetMeta propagates to the auto-generated HEAD twin, so HEAD probes follow
	// the same rule as GET.
	if !cfg.DisableLiveness {
		registerGET(cfg.LivenessPath, app.livenessHandler).
			Name("credo.health").
			SetMeta(MetaAccessLog, cfg.LogRequests)
	}
	if !cfg.DisableReadiness {
		registerGET(cfg.ReadinessPath, app.readinessHandler).
			Name("credo.ready").
			SetMeta(MetaAccessLog, cfg.LogRequests)
	}
	app.healthProbes = !cfg.DisableLiveness || !cfg.DisableReadiness
}

// AddLivenessCheck registers a named liveness check: a function that returns
// nil while the process is healthy. A value with a Check method is passed as
// its method value, v.Check.
//
// A check is collected whenever it is registered, before [App.UseHealth] or
// after it; UseHealth mounts the endpoint that runs it. A check whose
// endpoint is not mounted is accepted and never run, and an App that starts
// without having called UseHealth logs one warning naming how many checks
// no endpoint runs.
//
// Panics if check is nil, or name is empty, padded with whitespace, contains
// control characters, uses the reserved "credo." prefix, or duplicates
// another liveness check.
func (app *App) AddLivenessCheck(name string, check func(context.Context) error) {
	if check == nil {
		panic(fmt.Sprintf("credo: AddLivenessCheck: the check function of %q is nil; pass a func(context.Context) error", name))
	}
	app.healthEngine.AddLiveness(name, check)
}

// AddReadinessCheck registers a named readiness check: a function that
// returns nil while what it watches is available. A value with a Check method
// is passed as its method value, v.Check. [OptionalReadiness], its one
// option, reports the check in /ready without letting it gate readiness: a
// check of a cache or of a third-party API.
//
// A check is collected whenever it is registered, before [App.UseHealth] or
// after it; UseHealth mounts the endpoint that runs it. A check whose
// endpoint is not mounted is accepted and never run, and an App that starts
// without having called UseHealth logs one warning naming how many checks
// no endpoint runs.
//
// Panics if check is nil, an option other than [OptionalReadiness] is given,
// or name is empty, padded with whitespace, contains control characters, uses
// the reserved "credo." prefix, or duplicates another readiness check.
func (app *App) AddReadinessCheck(name string, check func(context.Context) error, opts ...RegistrationOption) {
	o := registrationOptions("App.AddReadinessCheck", opts, optOptionalReadiness)
	if check == nil {
		panic(fmt.Sprintf("credo: AddReadinessCheck: the check function of %q is nil; pass a func(context.Context) error", name))
	}
	app.healthEngine.AddReadiness(name, check, o.OptionalReadiness)
}

// warnUnmountedHealthChecks logs one warning when checks are registered on
// an App that never called UseHealth: no endpoint runs them. A UseHealth call
// with Disabled set is a decision and is not warned about. The start calls it
// once the App has entered running.
func (app *App) warnUnmountedHealthChecks() {
	if app.healthMounted {
		return
	}
	liveness, readiness := app.healthEngine.Counts()
	if liveness+readiness == 0 {
		return
	}
	app.logger.Warn("credo: health checks are registered but UseHealth was never called, so no endpoint runs them; "+
		"call app.UseHealth() to mount /health and /ready",
		"checks", liveness+readiness, "liveness", liveness, "readiness", readiness)
}

// contributedReadinessFunc returns the readiness contributions: one per
// resource whose owner answers Ready — a worker with a readiness condition
// among them — built once by the start walk, so a readiness request resolves
// nothing. It returns nil before the App has started or when there are none.
func (app *App) contributedReadinessFunc() internalhealth.ReadinessFunc {
	if !app.lifecycle.started.Load() || len(app.lifecycle.readiness) == 0 {
		return nil
	}
	components := app.lifecycle.readiness
	return func() []internalhealth.ReadinessCheck { return components }
}

// livenessHandler returns 200/503 with a JSON status body.
func (app *App) livenessHandler(ctx *Context) error {
	status, checks := app.healthEngine.CheckLiveness(ctx.Request().Context())
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
	// During graceful shutdown, report unready immediately so load balancers
	// stop routing to this instance. Liveness stays up — the process is alive,
	// it is just no longer accepting new work.
	if app.lifecycle.draining.Load() {
		return ctx.Response().JSON(http.StatusServiceUnavailable, map[string]string{"status": "shutting_down"})
	}

	status, checks := app.healthEngine.CheckReadiness(ctx.Request().Context(), app.contributedReadinessFunc())
	code := http.StatusOK
	if status == "down" {
		code = http.StatusServiceUnavailable
	}
	// A failing entry is logged whenever an execution finds it failing, an
	// optional one while readiness answers degraded included.
	app.logFailedChecks("readiness", checks)

	checksMap := make(map[string]any, len(checks))
	for _, c := range checks {
		entry := map[string]string{"status": c.Status, "latency": c.Latency.String()}
		if c.Error != "" && app.healthExposeErrors {
			entry["error"] = c.Error
		}
		checksMap[c.Name] = entry
	}

	body := map[string]any{"status": status}
	if len(checksMap) > 0 {
		body["checks"] = checksMap
	}
	return ctx.Response().JSON(code, body)
}

// logFailedChecks logs failing health checks so operators keep the error
// detail even when it is masked from the HTTP response.
func (app *App) logFailedChecks(kind string, checks []internalhealth.CheckResult) {
	for _, c := range checks {
		if c.Status != "up" {
			app.logger.Warn("credo: health check failed",
				"kind", kind, "check", c.Name, "error", c.Error, "latency", c.Latency)
		}
	}
}

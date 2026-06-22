package credo

import (
	"net/http"
	"time"

	internalobserve "github.com/credo-go/credo/internal/observe"
)

// MetaAccessLog is the route-meta key that toggles built-in access logging for
// a route — or, via LookupMeta parent-chain inheritance, for a whole group.
// Set it to false to silence the access-log line for matched requests:
//
//	app.Group("/internal").SetMeta(credo.MetaAccessLog, false)
//	app.GET("/metrics", h).SetMeta(credo.MetaAccessLog, false)
//
// A route-level value overrides a group-level one (LookupMeta reads the route
// before its parents), so a noisy group can be silenced while one route inside
// it stays logged. Only a bool false silences; any non-bool value is ignored
// and the request is logged (fail-open). Keys under the "credo." namespace are
// reserved by the framework.
//
// This key is honoured by both the built-in access logger and
// [middleware.AccessLog]. Health probes use it internally; see
// [HealthConfig.LogRequests].
const MetaAccessLog = "credo.accesslog"

// accessLogSilenced reports whether the matched route (or an ancestor group)
// set MetaAccessLog to false. A non-bool meta value fails open (not silenced),
// and an unmatched request (404/405, ctx.route == nil) is never silenced.
func accessLogSilenced(ctx *Context) bool {
	if ctx.route == nil {
		return false
	}
	v, ok := ctx.route.LookupMeta(MetaAccessLog)
	if !ok {
		return false
	}
	enabled, ok := v.(bool) // non-bool value → ok=false → not silenced (fail-open)
	return ok && !enabled
}

// builtinAccessLog logs each HTTP request with structured attributes. It is
// applied in compile() between builtinRequestID (outer) and builtinRecover
// (inner). Disabled via [WithoutAccessLog].
//
// Chain order: builtinRequestID → builtinAccessLog → builtinRecover →
// builtinErrorHandler → globalMW → dispatch.
//
// Because builtinRecover and builtinErrorHandler are inner frames, they
// write the final response (including error/panic responses) before this
// layer's defer fires. The defer therefore observes the committed response
// state — correct status, bytes, and duration — for all paths:
//
//   - Normal: handler writes response, returns nil.
//   - Error:  builtinErrorHandler catches the error, writes via handleError.
//   - Panic:  builtinRecover catches the panic, writes via handleError.
//
// The panicked flag is a safety net for the case where builtinRecover is
// disabled ([WithoutRecover]) and a handler panics. In that scenario the
// defer fires during stack unwinding before the process crashes.
//
// A request can be excluded from the access log two ways: the
// [WithAccessLogSkipper] predicate (consulted before routing, so only
// request-level data is reliable) and the [MetaAccessLog] route meta
// (consulted in the defer, once the matched route is known).
//
// Log level varies by status: 2xx/3xx → Info, 4xx → Warn, 5xx → Error.
// The request_id attribute is implicit in ctx.Logger() (set by builtinRequestID).
func (app *App) builtinAccessLog(next Handler) Handler {
	skip := app.accessLogSkipper
	return func(ctx *Context) error {
		if skip != nil && skip(ctx) {
			return next(ctx)
		}

		start := time.Now()
		panicked := true

		var err error
		defer func() {
			// Per-route or per-group silencing via MetaAccessLog. Checked
			// first so silenced routes skip the duration/status work entirely.
			if accessLogSilenced(ctx) {
				return
			}

			duration := time.Since(start)
			status := ctx.Response().Status()

			if panicked {
				// Safety net: only reachable when builtinRecover is disabled
				// (WithoutRecover) and a handler panics. The defer fires
				// during stack unwinding; the response is uncommitted.
				if status == 0 {
					status = http.StatusInternalServerError
				}
			} else {
				status = internalobserve.Status(status, err)
			}

			req := ctx.Request()
			r := req.Request

			requestID := ""
			if !ctx.HasRequestLogger() {
				requestID = ctx.RequestID()
			}
			internalobserve.EmitAccessLog(
				r.Context(),
				ctx.Logger(),
				r.Method,
				r.URL.Path,
				status,
				ctx.Response().Size(),
				duration,
				req.RealIP(),
				r.UserAgent(),
				ctx.OriginalPath(),
				requestID,
			)
		}()

		err = next(ctx)
		panicked = false
		return err
	}
}

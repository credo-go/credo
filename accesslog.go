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
const MetaAccessLog = internalobserve.MetaAccessLogKey

// AccessLogEntry is an immutable value snapshot captured at an access-log
// producer's observation boundary. The built-in producer runs outside recovery
// and error rendering, so Status, Bytes, and Duration are final when the inner
// pipeline completes. If recovery is disabled and a panic escapes, Status uses
// the built-in 500 fallback while Bytes reflects only what was written before
// the panic. The configurable [middleware.AccessLog] runs at its middleware
// position; on an error path Status is its best pre-render classification,
// Bytes is the amount written so far, and Duration excludes later rendering.
//
// Route is the matched route's registered pattern (for example
// "/v1/jobs/{job_id}"), empty when no route matched (404/405). It is emitted as
// the "route" attribute, so a deployment that must not persist concrete path
// values can drop "path"/"path_original" in its slog handler (for example via
// slog.HandlerOptions.ReplaceAttr) and keep the low-cardinality route instead.
// RouteName is available to [AccessLogResultFilter] but is not added to the
// emitted structured-log attributes. RemoteAddr is the result of
// [Request.RealIP]. RequestID is populated independently of whether the target
// logger already carries the request_id attribute.
type AccessLogEntry struct {
	Method       string
	Path         string
	OriginalPath string
	Route        string
	Status       int
	Bytes        int64
	Duration     time.Duration
	RemoteAddr   string
	UserAgent    string
	RequestID    string
	RouteName    string
}

// AccessLogResultFilter decides whether an observed access-log entry is
// submitted to its logger. Returning true emits the entry; false skips it.
// The Context is pooled and must not be retained. The same filter may be called
// concurrently for multiple requests and must be concurrency-safe.
//
// For the built-in access logger this callback runs outside built-in recovery.
// For [middleware.AccessLog] it runs inside built-in recovery unless that layer
// was disabled or replaced.
type AccessLogResultFilter func(ctx *Context, entry AccessLogEntry) bool

// accessLogSilenced reports whether the matched route (or an ancestor group)
// set MetaAccessLog to false. The decode (bool false silences, anything else
// fails open) is shared with middleware.AccessLog through internal/observe;
// an unmatched request (404/405, ctx.route == nil) is never silenced.
func accessLogSilenced(ctx *Context) bool {
	if ctx.route == nil {
		return false
	}
	return internalobserve.SilencedByMeta(ctx.route.LookupMeta(MetaAccessLog))
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
// Log level varies by status: 1xx/2xx/3xx → Info, 4xx → Warn, 5xx → Error.
// The request_id attribute is implicit in ctx.Logger() (set by builtinRequestID).
func (app *App) builtinAccessLog(next Handler) Handler {
	skip := app.accessLogSkipper
	minLevel := app.accessLogMinLevel
	filter := app.accessLogFilter
	configuredLogger := app.accessLogLogger
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
			if internalobserve.BelowMinLevel(status, minLevel) {
				return
			}

			req := ctx.Request()
			r := req.Request
			entry := AccessLogEntry{
				Method:       r.Method,
				Path:         r.URL.Path,
				OriginalPath: ctx.OriginalPath(),
				Status:       status,
				Bytes:        ctx.Response().Size(),
				Duration:     time.Since(start),
				RemoteAddr:   req.RealIP(),
				UserAgent:    r.UserAgent(),
				RequestID:    ctx.RequestID(),
			}
			if route := ctx.Route(); route != nil {
				entry.Route = route.GetPattern()
				entry.RouteName = route.GetName()
			}
			if filter != nil && !filter(ctx, entry) {
				return
			}

			logger := configuredLogger
			explicitRequestID := ""
			switch {
			case configuredLogger != nil:
				// A configured logger never carries request-scoped
				// enrichment; attach the ID explicitly.
				explicitRequestID = entry.RequestID
			case ctx.logger != nil:
				// A materialized request logger already carries request_id
				// (derivation contract on Context.SetLogger).
				logger = ctx.logger
			default:
				// No materialized request logger: log through the base
				// logger with an explicit request_id, so the deferred
				// enrichment stays unpaid for handlers that never log.
				logger = ctx.baseLogger()
				explicitRequestID = entry.RequestID
			}
			internalobserve.EmitAccessLog(r.Context(), logger,
				internalobserve.AccessLogRecord(entry), explicitRequestID)
		}()

		err = next(ctx)
		panicked = false
		return err
	}
}

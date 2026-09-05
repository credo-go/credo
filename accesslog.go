// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Copyright (c) 2024 LabStack.
// Derived from github.com/go-chi/chi/middleware (MIT) and
// github.com/labstack/echo/middleware (MIT).

package credo

import (
	"errors"
	"fmt"
	"log/slog"
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
// Health probes use it internally; see [HealthConfig.LogRequests].
const MetaAccessLog = internalobserve.MetaAccessLogKey

// AccessLogEntry is an immutable value snapshot captured at the access-log
// observation boundary, after the final response: error rendering, panic
// recovery and response finalization (the compressor's trailer included) have
// completed, so Status, Bytes and Duration are final. Bytes counts the bytes
// the underlying transport writer accepted — compressed output when response
// compression applied. Duration is measured from executor entry to response
// finalization; the result filter and the log emission itself are excluded.
// If recovery is disabled and a panic escapes, Status uses the 500 fallback
// while Bytes reflects only what was written before the panic.
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
// The filter runs after the final response. A panic inside it is logged as a
// framework diagnostic and the entry is skipped; the response is unaffected.
// With [WithoutRecover] the panic propagates after request cleanup.
type AccessLogResultFilter func(ctx *Context, entry AccessLogEntry) bool

// accessLogSilenced reports whether the matched route (or an ancestor group)
// set MetaAccessLog to false. The decode (bool false silences, anything else
// fails open) lives in internal/observe; an unmatched request (404/405,
// ctx.route == nil) is never silenced.
func accessLogSilenced(ctx *Context) bool {
	if ctx.route == nil {
		return false
	}
	return internalobserve.SilencedByMeta(ctx.route.LookupMeta(MetaAccessLog))
}

// AccessLogConfig configures the access-log feature installed by
// [App.UseAccessLog]. The zero value selects every default.
type AccessLogConfig struct {
	// Logger is a dedicated sink for access records. nil logs through the
	// request-scoped logger, which already carries request_id when
	// [App.UseRequestID] is installed and the handler materialized it; a
	// dedicated logger never inherits [Context.AddLogAttrs]/[Context.SetLogger]
	// enrichment, so the framework attaches request_id explicitly.
	Logger *slog.Logger

	// MinLevel is the minimum status-derived level a record must reach to be
	// submitted: 1xx/2xx/3xx are Info, 4xx Warn, 5xx+ Error. nil selects
	// Info. The Leveler may be consulted concurrently and must be
	// concurrency-safe; [slog.LevelVar] supports runtime changes. A typed-nil
	// Leveler is rejected at registration.
	MinLevel slog.Leveler

	// Skipper excludes a request before routing; only request-level data is
	// reliable inside it (method, path, headers). For route-based silencing
	// use [MetaAccessLog]; for post-response decisions use MinLevel or
	// ResultFilter.
	Skipper func(ctx *Context) bool

	// ResultFilter is a post-response predicate: it runs only after
	// MetaAccessLog silencing and the MinLevel check, sees the final entry,
	// and emits it on true. It cannot restore an entry MinLevel rejected.
	ResultFilter AccessLogResultFilter
}

// accessLogFeature is the normalized access-log configuration.
type accessLogFeature struct {
	logger   *slog.Logger
	minLevel slog.Leveler
	skipper  func(*Context) bool
	filter   AccessLogResultFilter
}

// UseAccessLog installs access logging: one "request completed" record per
// request with method, path, route, status, bytes, duration, remote_addr
// (from [Request.RealIP]), user_agent and request_id. The record is observed
// after the final response — error rendering, panic recovery and response
// finalization included — so status and bytes are what the client received.
// Order: Skipper → request → [MetaAccessLog] → status/level → MinLevel →
// snapshot → ResultFilter → emit.
//
// The feature is off by default and independent of [App.UseRequestID]: the
// request_id attribute is empty when only access logging is installed.
// UseAccessLog accepts zero configs for the defaults or one config; it panics
// for more than one config, for a typed-nil MinLevel, when called twice, or
// after the App was prepared or shut down.
func (app *App) UseAccessLog(cfgs ...AccessLogConfig) {
	cfg := oneConfig("App.UseAccessLog", cfgs)
	if internalobserve.IsTypedNilLeveler(cfg.MinLevel) {
		panic("credo: App.UseAccessLog: typed-nil slog.Leveler")
	}
	f := &accessLogFeature{
		logger:   cfg.Logger,
		minLevel: cfg.MinLevel,
		skipper:  cfg.Skipper,
		filter:   cfg.ResultFilter,
	}
	if f.minLevel == nil {
		f.minLevel = slog.LevelInfo
	}
	app.installFeature("App.UseAccessLog", func() {
		if app.accessLog != nil {
			panic("credo: App.UseAccessLog called twice")
		}
		app.accessLog = f
	})
}

// observeAccess is the executor's access-log observation stage. It runs after
// response finalization for every request the feature selected. outBytes is
// the byte count taken at the output boundary when the response passed
// through a finalized writer (compression); counted reports that it applies,
// otherwise the plain Response size is the transport count.
func (app *App) observeAccess(c *Context, outBytes int64, counted bool) {
	f := app.accessLog

	// Per-route or per-group silencing via MetaAccessLog. Checked first so
	// silenced routes skip the duration/status work entirely.
	if accessLogSilenced(c) {
		return
	}

	status := c.response.Status()
	if status == 0 {
		if c.exec.panicked {
			// Only reachable when recovery is disabled and a panic escaped:
			// the response is uncommitted and the client sees the server's
			// abort.
			status = http.StatusInternalServerError
		} else {
			status = http.StatusOK
		}
	}
	if internalobserve.BelowMinLevel(status, f.minLevel) {
		return
	}

	req := c.request
	r := req.Request

	// The target logger depends only on the configuration and the request
	// logger, never on the entry, so it is selected first.
	logger := f.logger
	explicitRequestID := true
	switch {
	case f.logger != nil:
		// A configured logger never carries request-scoped enrichment;
		// attach the ID explicitly.
	case c.logger != nil:
		// A materialized request logger already carries request_id
		// (derivation contract on Context.SetLogger).
		logger = c.logger
		explicitRequestID = false
	default:
		// No materialized request logger: log through the base logger with
		// an explicit request_id, so the deferred enrichment stays unpaid
		// for handlers that never log.
		logger = c.baseLogger()
	}

	// Without a result filter nobody observes the entry but the logger, so a
	// level the logger will not record skips the entry construction (client
	// address, user agent, route, request ID). With a filter installed the
	// entry is always built: the filter observes every response.
	if f.filter == nil && !logger.Enabled(r.Context(), internalobserve.Level(status)) {
		return
	}

	bytes := c.response.Size()
	if counted {
		bytes = outBytes
	}
	entry := AccessLogEntry{
		Method:       r.Method,
		Path:         r.URL.Path,
		OriginalPath: c.OriginalPath(),
		Status:       status,
		Bytes:        bytes,
		Duration:     time.Since(c.exec.start),
		RemoteAddr:   req.RealIP(),
		UserAgent:    r.UserAgent(),
		RequestID:    c.RequestID(),
	}
	if route := c.Route(); route != nil {
		entry.Route = route.GetPattern()
		entry.RouteName = route.GetName()
	}
	if f.filter != nil && !app.runAccessFilter(c, f.filter, entry) {
		return
	}

	requestID := ""
	if explicitRequestID {
		requestID = entry.RequestID
	}
	internalobserve.EmitAccessLog(r.Context(), logger,
		internalobserve.AccessLogRecord(entry), requestID)
}

// runAccessFilter invokes the result filter. A panic inside it cannot change
// the response any more, so with recovery enabled it is logged as a framework
// diagnostic and the entry is skipped; http.ErrAbortHandler and, with recovery
// disabled, every panic propagate.
func (app *App) runAccessFilter(c *Context, filter AccessLogResultFilter, entry AccessLogEntry) (emit bool) {
	if app.recover != nil {
		defer func() {
			if rvr := recover(); rvr != nil {
				if err, ok := rvr.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rvr)
				}
				r := c.request.Request
				c.Logger().LogAttrs(r.Context(), slog.LevelError, "credo: access-log result filter panicked",
					slog.String("panic", fmt.Sprint(rvr)),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", entry.Status),
				)
				emit = false
			}
		}()
	}
	return filter(c, entry)
}

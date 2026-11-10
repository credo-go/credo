// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Copyright (c) 2024 LabStack.
// Derived from github.com/go-chi/chi/middleware (MIT) and
// github.com/labstack/echo/middleware (MIT).

package credo

import (
	"log/slog"

	internalobserve "github.com/credo-go/credo/internal/observe"
)

// MetaAccessLog is the route-meta key that toggles built-in access logging for
// a route — or, inherited through the group parent chain, for a whole group.
// Set it to false to silence the access-log line for matched requests:
//
//	app.Group("/internal").SetMeta(credo.MetaAccessLog, false)
//	app.GET("/metrics", h).SetMeta(credo.MetaAccessLog, false)
//
// A route-level value overrides a group-level one ([Route.GetMeta] reads the
// route before its parents), so a noisy group can be silenced while one route
// inside it stays logged, with true.
//
// Health probes use it internally; see [HealthConfig.LogRequests].
var MetaAccessLog = NewKey[bool]("credo.accesslog")

// accessLogSilenced reports whether the matched route (or an ancestor group)
// set MetaAccessLog to false; an unmatched request (404/405, ctx.route ==
// nil) is never silenced.
func accessLogSilenced(ctx *Context) bool {
	if ctx.route == nil {
		return false
	}
	enabled, ok := ctx.route.info.GetMeta(MetaAccessLog)
	return ok && !enabled
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
	// MetaAccessLog silencing and the MinLevel check, sees the final
	// [Exchange], and emits the record on true. It cannot restore a record
	// MinLevel rejected. It may be called concurrently for multiple requests
	// and must be concurrency-safe. A panic inside it is logged as a
	// framework diagnostic and the record is skipped; with [WithoutRecover]
	// the panic propagates.
	ResultFilter func(ex *Exchange) bool

	// Attrs returns application attributes for a record that is a candidate
	// for emission: after MetaAccessLog, MinLevel and ResultFilter, and only
	// when the logger is enabled for the record's level. They are emitted
	// inside one "attrs" group, so an application attribute never shadows a
	// framework attribute; "attrs" is a reserved key on the access record.
	// The returned slice passes to the framework and must not be reused. A
	// panic inside Attrs is logged as a framework diagnostic and the record
	// is emitted without the group; with [WithoutRecover] the panic
	// propagates. Attrs may be called concurrently and must be
	// concurrency-safe.
	Attrs func(ex *Exchange) []slog.Attr
}

// accessLogFeature is the normalized access-log configuration and the
// access log's [Observer].
type accessLogFeature struct {
	app      *App
	logger   *slog.Logger
	minLevel slog.Leveler
	skipper  func(*Context) bool
	filter   func(*Exchange) bool
	attrs    func(*Exchange) []slog.Attr
}

// UseAccessLog installs access logging: one "request completed" record per
// request with method, path, route, status, bytes, duration, remote_addr
// (from [Request.RealIP]), user_agent and request_id, and the application's
// attributes from Attrs in an "attrs" group. The access log is an [Observer]:
// it records the final response — error rendering, panic recovery and
// response finalization included — so status and bytes are what the client
// received, and it runs among the observers in the order UseAccessLog was
// called relative to [App.AddObserver]. Order: Skipper → request →
// [MetaAccessLog] → status/level → MinLevel → ResultFilter → logger level →
// Attrs → emit.
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
		app:      app,
		logger:   cfg.Logger,
		minLevel: cfg.MinLevel,
		skipper:  cfg.Skipper,
		filter:   cfg.ResultFilter,
		attrs:    cfg.Attrs,
	}
	if f.minLevel == nil {
		f.minLevel = slog.LevelInfo
	}
	app.installFeature("App.UseAccessLog", func() {
		if app.accessLog != nil {
			panic("credo: App.UseAccessLog called twice")
		}
		app.accessLog = f
		app.observers = append(app.observers, f)
	})
}

// Observe records one access-log line for an exchange the feature selected.
func (f *accessLogFeature) Observe(ex *Exchange) {
	c := ex.c
	if !c.exec.logging {
		return
	}
	// Per-route or per-group silencing via MetaAccessLog. Checked first so
	// silenced routes skip the level work entirely.
	if accessLogSilenced(c) {
		return
	}
	status := ex.status
	if internalobserve.BelowMinLevel(status, f.minLevel) {
		return
	}

	r := c.request.Request

	// The target logger depends only on the configuration and the request
	// logger, never on the exchange, so it is selected first.
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

	// Without a result filter nobody sees the record but the logger, so a
	// level the logger will not record skips the record construction (client
	// address, user agent, route, request ID). A filter sees every response
	// MinLevel admits, whether or not the logger records it.
	level := internalobserve.Level(status)
	enabled := logger.Enabled(r.Context(), level)
	if f.filter != nil && !f.runFilter(ex) {
		return
	}
	if !enabled {
		return
	}

	rec := internalobserve.AccessLogRecord{
		Method:       r.Method,
		Path:         r.URL.Path,
		OriginalPath: c.OriginalPath(),
		Route:        routePattern(c.route),
		Status:       status,
		Bytes:        ex.bytes,
		Duration:     ex.duration,
		RemoteAddr:   c.request.RealIP(),
		UserAgent:    r.UserAgent(),
	}
	if explicitRequestID {
		rec.RequestID = c.RequestID()
	}
	var attrs []slog.Attr
	if f.attrs != nil {
		attrs = f.runAttrs(ex)
	}
	internalobserve.EmitAccessLog(r.Context(), logger, level, rec, attrs)
}

// runFilter invokes the result filter. A panic inside it cannot change the
// response any more, so with recovery enabled it is logged as a framework
// diagnostic and the record is skipped; http.ErrAbortHandler and, with
// recovery disabled, every panic propagate.
func (f *accessLogFeature) runFilter(ex *Exchange) (emit bool) {
	if f.app.recover != nil {
		defer func() {
			if rvr := recover(); rvr != nil {
				f.app.observerPanicked(ex, "credo: access-log result filter panicked", rvr)
				emit = false
			}
		}()
	}
	return f.filter(ex)
}

// runAttrs invokes Attrs. With recovery enabled a panic inside it is logged
// as a framework diagnostic and the record is emitted without the group;
// http.ErrAbortHandler and, with recovery disabled, every panic propagate.
func (f *accessLogFeature) runAttrs(ex *Exchange) (attrs []slog.Attr) {
	if f.app.recover != nil {
		defer func() {
			if rvr := recover(); rvr != nil {
				f.app.observerPanicked(ex, "credo: access-log attrs panicked", rvr)
				attrs = nil
			}
		}()
	}
	return f.attrs(ex)
}

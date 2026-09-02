// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Copyright (c) 2024 LabStack.
// Derived from github.com/go-chi/chi/middleware (MIT) and
// github.com/labstack/echo/middleware (MIT).

package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/credo-go/credo"
	internalobserve "github.com/credo-go/credo/internal/observe"
	"github.com/credo-go/credo/validation"
)

// AccessLogConfig defines configuration for the AccessLog middleware.
type AccessLogConfig struct {
	// Logger is used to log request information.
	// Default: ctx.Logger() (the request-scoped logger from the app).
	Logger *slog.Logger

	// MinLevel is the minimum status-derived level submitted to the logger.
	// nil defaults to slog.LevelInfo. The Leveler is read once per eligible
	// request and must be concurrency-safe. A typed-nil Leveler panics during
	// middleware construction.
	MinLevel slog.Leveler

	// Skipper defines a function to skip logging for certain requests.
	// When Skipper returns true, the request is not logged.
	// Useful for health check endpoints or static assets.
	// Default: DefaultSkipper (all requests are logged).
	Skipper Skipper

	// ResultFilter runs after route-meta silencing and MinLevel. true emits the
	// observed entry; false skips it. It cannot restore an entry rejected by
	// MinLevel. The callback may run concurrently and must not retain ctx.
	ResultFilter credo.AccessLogResultFilter
}

// DefaultAccessLogConfig returns the default AccessLog middleware config.
// Each call returns a fresh value, so callers cannot mutate the
// package-wide defaults.
func DefaultAccessLogConfig() AccessLogConfig {
	return AccessLogConfig{
		MinLevel: slog.LevelInfo,
		Skipper:  DefaultSkipper,
	}
}

// AccessLog returns middleware that logs each HTTP request using slog with
// structured attributes: method, path, status, bytes, duration,
// remote_addr (from Request.RealIP), user_agent, request_id (if RequestID middleware is active),
// and path_original when the final served path differs from the client path.
//
// Requests can be excluded two ways: the [AccessLogConfig.Skipper] predicate
// (consulted before the handler runs) and the [credo.MetaAccessLog] route meta
// set to false (consulted after the handler, once the route is known), which
// also silences a whole group via LookupMeta inheritance.
//
// MinLevel and ResultFilter only narrow the set of records; ResultFilter cannot
// restore a record rejected by MinLevel. A custom Logger is not derived from
// ctx.Logger(), so request-scoped attributes are not inherited; request_id is
// still added explicitly. On returned-error paths this middleware observes the
// response before the centralized error renderer, unlike the built-in logger.
//
// The log level varies by response status code:
//   - 1xx, 2xx, 3xx: slog.LevelInfo
//   - 4xx:      slog.LevelWarn
//   - 5xx:      slog.LevelError
func AccessLog(cfg ...AccessLogConfig) credo.Middleware {
	config := resolveConfig(cfg, DefaultAccessLogConfig(), normalizeAccessLogConfig)

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if config.Skipper(ctx) {
				return next(ctx)
			}

			start := time.Now()

			err := next(ctx)

			// Honour per-route/group silencing (MetaAccessLog) once the
			// matched route is known. The pre-dispatch Skipper above covers
			// request-level skips; the key and its decode are shared with
			// the built-in access logger through internal/observe.
			if r := ctx.Route(); r != nil {
				if internalobserve.SilencedByMeta(r.LookupMeta(credo.MetaAccessLog)) {
					return err
				}
			}

			// Use the Response's tracked status and size.
			status := accessLogStatus(ctx.Response().Status(), err)
			if internalobserve.BelowMinLevel(status, config.MinLevel) {
				return err
			}

			req := ctx.Request()
			r := req.Request
			entry := credo.AccessLogEntry{
				Method:       r.Method,
				Path:         r.URL.Path,
				OriginalPath: ctx.OriginalPath(),
				Status:       status,
				Bytes:        ctx.Response().Size(),
				Duration:     time.Since(start),
				RemoteAddr:   req.RealIP(),
				UserAgent:    r.UserAgent(),
				RequestID:    GetRequestID(ctx),
			}
			if route := ctx.Route(); route != nil {
				entry.RouteName = route.GetName()
			}
			if config.ResultFilter != nil && !config.ResultFilter(ctx, entry) {
				return err
			}

			logger := config.Logger
			if logger == nil {
				logger = ctx.Logger()
			}

			// Add request_id explicitly only when the logger does not
			// already carry it: a custom Logger never does; ctx.Logger()
			// does whenever a request-scoped logger was set (built-in
			// request ID tier, RequestID middleware, or SetLogger).
			explicitRequestID := ""
			if config.Logger != nil || !ctx.HasRequestLogger() {
				explicitRequestID = entry.RequestID
			}

			internalobserve.EmitAccessLog(r.Context(), logger,
				internalobserve.AccessLogRecord(entry), explicitRequestID)

			return err
		}
	}
}

// accessLogStatus mirrors the root error handler's status precedence while
// the route middleware is still inside that handler and the error response
// has therefore not been committed yet. The shared observe classifier owns
// semantic-fault and legacy HTTPStatus handling; only root-specific error
// types are selected here.
func accessLogStatus(status int, err error) int {
	if status != 0 || err == nil {
		return internalobserve.Status(status, err)
	}
	if _, ok := errors.AsType[validation.Errors](err); ok {
		return http.StatusUnprocessableEntity
	}
	if httpErr, ok := errors.AsType[*credo.HTTPError](err); ok {
		return httpErr.Status
	}
	return internalobserve.Status(status, err)
}

func normalizeAccessLogConfig(config AccessLogConfig) AccessLogConfig {
	if internalobserve.IsTypedNilLeveler(config.MinLevel) {
		panic("middleware: AccessLog MinLevel is a typed-nil slog.Leveler")
	}
	if config.MinLevel == nil {
		config.MinLevel = slog.LevelInfo
	}
	if config.Skipper == nil {
		config.Skipper = DefaultSkipper
	}
	return config
}

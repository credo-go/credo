// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Copyright (c) 2024 LabStack.
// Derived from github.com/go-chi/chi/middleware (MIT) and
// github.com/labstack/echo/middleware (MIT).

package middleware

import (
	"log/slog"
	"time"

	"github.com/credo-go/credo"
	internalobserve "github.com/credo-go/credo/internal/observe"
)

// AccessLogConfig defines configuration for the AccessLog middleware.
type AccessLogConfig struct {
	// Logger is used to log request information.
	// Default: ctx.Logger() (the request-scoped logger from the app).
	Logger *slog.Logger

	// Skipper defines a function to skip logging for certain requests.
	// When Skipper returns true, the request is not logged.
	// Useful for health check endpoints or static assets.
	// Default: DefaultSkipper (all requests are logged).
	Skipper Skipper
}

// DefaultAccessLogConfig returns the default AccessLog middleware config.
// Each call returns a fresh value, so callers cannot mutate the
// package-wide defaults.
func DefaultAccessLogConfig() AccessLogConfig {
	return AccessLogConfig{
		Skipper: DefaultSkipper,
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
// The log level varies by response status code:
//   - 2xx, 3xx: slog.LevelInfo
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
			// request-level skips; this mirrors the built-in access logger so
			// both share the same route-meta opt-out. The key is the single
			// source of truth; the bool decode is duplicated here only because
			// internal/observe cannot import the root credo package.
			if r := ctx.Route(); r != nil {
				if v, ok := r.LookupMeta(credo.MetaAccessLog); ok {
					if enabled, ok := v.(bool); ok && !enabled {
						return err
					}
				}
			}

			duration := time.Since(start)

			// Use the Response's tracked status and size.
			status := internalobserve.Status(ctx.Response().Status(), err)

			req := ctx.Request()
			r := req.Request

			logger := config.Logger
			if logger == nil {
				logger = ctx.Logger()
			}

			// Add request_id explicitly only when the logger does not
			// already carry it: a custom Logger never does; ctx.Logger()
			// does whenever a request-scoped logger was set (built-in
			// request ID tier, RequestID middleware, or SetLogger).
			requestID := ""
			if config.Logger != nil || !ctx.HasRequestLogger() {
				requestID = GetRequestID(ctx)
			}

			internalobserve.EmitAccessLog(
				r.Context(),
				logger,
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

			return err
		}
	}
}

func normalizeAccessLogConfig(config AccessLogConfig) AccessLogConfig {
	if config.Skipper == nil {
		config.Skipper = DefaultSkipper
	}
	return config
}

// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi/middleware (MIT License).

package middleware

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/internal/httpheader"
	internalobserve "github.com/credo-go/credo/internal/observe"
)

// RecoverConfig defines configuration for the Recover middleware.
type RecoverConfig struct {
	// Logger is used to log panic information.
	// Default: ctx.Logger() (the request-scoped logger from the app).
	Logger *slog.Logger

	// DisableStackTrace disables stack trace logging on panic.
	// Default: false (stack traces are enabled).
	DisableStackTrace bool

	// StackSize is the maximum number of bytes for the stack trace.
	// Default: 8192.
	StackSize int
}

// DefaultRecoverConfig returns the default Recover middleware config.
// Each call returns a fresh value, so callers cannot mutate the
// package-wide defaults.
func DefaultRecoverConfig() RecoverConfig {
	return RecoverConfig{
		StackSize: 8192,
	}
}

// Recover returns middleware that recovers from panics, logs the panic
// using slog, and returns an HTTP 500 Internal Server Error response.
//
// Note: Credo includes built-in panic recovery by default (see
// [credo.WithoutRecover]). This middleware is useful when you need
// per-group/per-route recovery with custom configuration (e.g., custom
// logger, stack size control, or disabled stack traces).
//
// The http.ErrAbortHandler sentinel is re-panicked to allow the HTTP
// server to abort the connection as intended.
func Recover(cfg ...RecoverConfig) credo.Middleware {
	c := resolveConfig(cfg, DefaultRecoverConfig(), normalizeRecoverConfig)

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) (retErr error) {
			defer func() {
				if rvr := recover(); rvr != nil {
					// Re-panic http.ErrAbortHandler — this sentinel
					// tells the HTTP server to abort the connection.
					if err, ok := rvr.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(rvr)
					}

					r := ctx.Request().Request

					stack := ""
					if !c.DisableStackTrace {
						stack = internalobserve.StackTrace(c.StackSize)
					}

					// Add request_id explicitly only when the logger does
					// not already carry it (same rule as AccessLog).
					requestID := ""
					if c.Logger != nil || !ctx.HasRequestLogger() {
						requestID = GetRequestID(ctx)
					}
					attrs := internalobserve.PanicAttrs(rvr, r.Method, r.URL.Path, requestID, stack)

					logger := c.Logger
					if logger == nil {
						logger = ctx.Logger()
					}
					logger.LogAttrs(r.Context(), slog.LevelError,
						"panic recovered", attrs...)

					// Skip error propagation for upgraded connections where
					// the response writer may already be hijacked.
					if httpheader.HasToken(r.Header, "Connection", "upgrade") {
						retErr = nil
						return
					}

					retErr = credo.ErrInternalServerError.WithInternal(internalobserve.PanicError(rvr))
				}
			}()

			return next(ctx)
		}
	}
}

func normalizeRecoverConfig(config RecoverConfig) RecoverConfig {
	if config.StackSize <= 0 {
		config.StackSize = DefaultRecoverConfig().StackSize
	}
	return config
}

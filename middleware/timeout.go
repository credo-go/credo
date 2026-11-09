// Copyright (c) 2024 LabStack.
// Originally derived from github.com/labstack/echo/middleware/context_timeout.go (MIT License).

package middleware

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/credo-go/credo"
)

// TimeoutConfig defines configuration for Timeout middleware.
type TimeoutConfig struct {
	// Skipper defines a function to skip middleware.
	Skipper Skipper

	// Timeout defines request context timeout.
	// Zero or negative values disable timeout behavior.
	Timeout time.Duration

	// ErrorHandler maps downstream errors after timeout wrapping.
	ErrorHandler func(ctx *credo.Context, err error) error
}

// DefaultTimeoutConfig returns the default Timeout middleware config.
// Each call returns a fresh value, so callers cannot mutate the
// package-wide defaults.
func DefaultTimeoutConfig() TimeoutConfig {
	return TimeoutConfig{
		Skipper: DefaultSkipper,
	}
}

// Timeout returns request timeout middleware.
//
// It runs the rest of the chain in a scope whose context carries the
// configured deadline ([credo.Context.Within]): handlers and downstream calls
// observe it through c.Context(), and it ends when the chain returns. A value
// set inside the scope, such as an authenticated user, outlives it. When the
// chain returns context.DeadlineExceeded, the default ErrorHandler maps it to
// 503 request_timeout.
//
// Enforcement is cooperative. Unlike http.TimeoutHandler, this middleware
// does not buffer the response or cut off writes once the deadline passes —
// a handler that ignores its context can keep writing to the client. Pass
// c.Context() to blocking work (database queries, outbound requests) so
// it actually aborts on deadline.
func Timeout(cfg ...TimeoutConfig) credo.Middleware {
	config := resolveConfig(cfg, DefaultTimeoutConfig(), normalizeTimeoutConfig)

	return func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			if config.Skipper(c) || config.Timeout <= 0 {
				return next(c)
			}
			timeoutCtx, cancel := context.WithTimeout(c.Context(), config.Timeout)
			defer cancel()
			if err := c.Within(timeoutCtx, next); err != nil {
				return config.ErrorHandler(c, err)
			}
			return nil
		}
	}
}

func normalizeTimeoutConfig(config TimeoutConfig) TimeoutConfig {
	if config.Skipper == nil {
		config.Skipper = DefaultSkipper
	}
	if config.ErrorHandler == nil {
		config.ErrorHandler = defaultTimeoutErrorHandler
	}
	return config
}

func defaultTimeoutErrorHandler(_ *credo.Context, err error) error {
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		return credo.NewHTTPError(http.StatusServiceUnavailable, "request_timeout").WithInternal(err)
	}
	return err
}

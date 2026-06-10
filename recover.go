package credo

import (
	"log/slog"
	"net/http"

	"github.com/credo-go/credo/internal/httpheader"
	internalobserve "github.com/credo-go/credo/internal/observe"
)

// builtinStackSize bounds the stack trace captured during built-in panic
// recovery, matching middleware.Recover's default. It prevents unbounded log
// growth from deep stacks during a panic storm.
const builtinStackSize = 8192

// builtinRecover wraps a handler with panic recovery. It sits inside
// builtinAccessLog in compile(), catching panics from the error handler,
// global middleware, and dispatch. Disabled via [WithoutRecover].
//
// On panic, builtinRecover writes the error response directly via
// [App.handleError] and returns nil. Because it is an inner frame relative
// to builtinAccessLog, the access log's defer fires after the panic response
// is committed — giving correct bytes, status, and duration on the panic path.
//
// Unlike middleware.Recover, this uses the request-scoped logger (ctx.Logger)
// and has no user-configurable options — it always logs the stack trace.
// When a request-scoped logger is set (built-in request ID tier or
// middleware.RequestID), request_id is implicit in ctx.Logger(); otherwise
// it is read from the context store and added as an explicit attribute.
func builtinRecover(next Handler) Handler {
	return func(ctx *Context) error {
		defer func() {
			if rvr := recover(); rvr != nil {
				if rvr == http.ErrAbortHandler {
					panic(rvr)
				}

				r := ctx.Request().Request

				requestID := ""
				if !ctx.HasRequestLogger() {
					requestID = ctx.RequestID()
				}
				attrs := internalobserve.PanicAttrs(
					rvr,
					r.Method,
					r.URL.Path,
					requestID,
					internalobserve.StackTrace(builtinStackSize),
				)

				ctx.Logger().LogAttrs(r.Context(), slog.LevelError,
					"panic recovered", attrs...,
				)

				// Skip the error response for upgraded connections — the
				// writer may already be hijacked (WebSocket), and writing
				// to it would panic or corrupt the stream. Mirrors
				// middleware.Recover.
				if httpheader.HasToken(r.Header, "Connection", "upgrade") {
					return
				}

				// Write the error response directly instead of returning
				// an error, so that builtinAccessLog's defer can observe
				// the committed response (status, bytes).
				ctx.app.handleError(
					ErrInternalServerError.WithInternal(internalobserve.PanicError(rvr)),
					ctx,
				)
			}
		}()

		return next(ctx)
	}
}

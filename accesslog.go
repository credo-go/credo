package credo

import (
	"net/http"
	"time"

	internalobserve "github.com/credo-go/credo/internal/observe"
)

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
// Log level varies by status: 2xx/3xx → Info, 4xx → Warn, 5xx → Error.
// The request_id attribute is implicit in ctx.Logger() (set by builtinRequestID).
func builtinAccessLog(next Handler) Handler {
	return func(ctx *Context) error {
		start := time.Now()
		panicked := true

		var err error
		defer func() {
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
			attrs, attrCount := internalobserve.AccessLogAttrs(
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

			ctx.Logger().LogAttrs(r.Context(), internalobserve.Level(status), "request completed", attrs[:attrCount]...)
		}()

		err = next(ctx)
		panicked = false
		return err
	}
}

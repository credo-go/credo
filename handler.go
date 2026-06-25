package credo

import "net/http"

// Handler is the Credo handler signature. Handlers return an error for
// centralized error handling via the App's error handling pipeline.
type Handler func(ctx *Context) error

// ErrorInfo carries the original error and the framework-classified
// [ProblemDetails] to the [ErrorRenderer].
type ErrorInfo struct {
	// Err is the original error returned by the handler.
	// Use [errors.As] / [errors.Is] for type-specific behavior
	// (e.g., Sentry reporting, extracting metadata for custom headers).
	Err error

	// MessageKey is the i18n message key used to resolve [ProblemDetails.Title].
	// This is the raw key before resolution (e.g., "http.not_found",
	// "user.email_exists"). Useful for client-side i18n, telemetry
	// grouping, or custom error code mapping.
	MessageKey string

	// Problem is the framework-classified RFC 7807 Problem Details.
	// The renderer may use it as-is, modify it, or ignore it entirely
	// and write a custom response format.
	Problem *ProblemDetails
}

// ErrorRenderer formats an error response given a classified [ErrorInfo].
// The framework handles error classification, logging, and committed-response
// guards internally. ErrorRenderer is called for all HTTP methods including
// HEAD, allowing it to set response headers (e.g., Retry-After,
// WWW-Authenticate). For HEAD requests where the renderer does not commit
// the response, the framework sends a status-only response (no body).
//
// If ErrorRenderer does not write a response ([Response.Committed] remains
// false), the framework sends a status-only response for HEAD, or falls back
// to the default RFC 7807 JSON renderer for other methods.
//
// Register a custom renderer with [App.SetErrorRenderer].
type ErrorRenderer func(ctx *Context, info ErrorInfo)

// SuccessRenderer formats a successful response, given the status code and the
// payload a handler wants to send. It is the success-side mirror of
// [ErrorRenderer]: opt-in, never installed by default, and consulted only
// through [Context.Render] — the raw [Response] helpers ([Response.JSON],
// [Response.XML], [Response.Text], [Response.Blob], and the streaming writers)
// stay un-intercepted so webhooks, health probes, and third-party response
// shapes always bypass any house envelope.
//
// A non-nil error returned by the renderer flows into the normal error pipeline
// (classification, logging, [ErrorRenderer]), exactly as if the handler had
// returned it. The renderer owns committing the response; if it writes nothing,
// the framework treats the call as complete.
//
// Register a custom renderer with [App.SetSuccessRenderer]. The single
// status+data seam is also the integration point a future typed-endpoint layer
// would route its typed result through, so one envelope policy covers both.
type SuccessRenderer func(c *Context, status int, data any) error

// Middleware is the single middleware type used throughout Credo.
// All middleware — global, group, and route level — uses this signature.
type Middleware func(next Handler) Handler

// StdMiddleware is a stdlib-compatible middleware signature.
// Use [WrapStdMiddleware] to convert stdlib middleware into [Middleware].
type StdMiddleware = func(http.Handler) http.Handler

// WrapStdMiddleware converts stdlib middleware (func(http.Handler) http.Handler)
// into a Credo [Middleware] (func(Handler) Handler). This allows using any
// existing Go community middleware with Credo's unified middleware stack.
//
//	app.GlobalMiddleware(credo.WrapStdMiddleware(corsMiddleware))
func WrapStdMiddleware(m StdMiddleware) Middleware {
	return func(next Handler) Handler {
		return func(ctx *Context) error {
			origReq := ctx.request.Request
			origWriter := ctx.response.ResponseWriter
			// The stdlib middleware's request/writer substitutions are only
			// valid while its ServeHTTP runs. Restore the originals afterwards
			// (also on panic) so that later writes — the error pipeline,
			// recovery — never hit a writer the middleware already finalized.
			defer func() {
				ctx.request.Request = origReq
				ctx.response.ResponseWriter = origWriter
			}()
			var handlerErr error
			nextHTTP := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx.request.Request = r         // stdlib MW may modify request
				ctx.response.ResponseWriter = w // stdlib MW may wrap writer
				handlerErr = next(ctx)
			})
			// Pass the underlying ResponseWriter (not the Response wrapper)
			// so that if the stdlib middleware wraps w, we avoid circular
			// delegation when setting ctx.response.ResponseWriter = w.
			m(nextHTTP).ServeHTTP(origWriter, origReq)
			return handlerErr
		}
	}
}

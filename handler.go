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

// ErrorRenderer shapes the body of an error response given a classified
// [ErrorInfo]. The framework owns everything around that shape: error
// classification, logging, the status code (info.Problem.Status — mutate it
// before returning to change it), the Content-Type, HEAD handling, and
// committed-response guards.
//
// The return value is the response body. A non-nil value is encoded as JSON
// with the application's JSON profile and written with info.Problem.Status;
// returning nil renders the default RFC 7807 Problem Details instead. Either
// way the renderer never writes the response itself in the common case — it
// only decides the shape.
//
// The renderer is called for all HTTP methods including HEAD, so it can set
// response headers (e.g., Retry-After, WWW-Authenticate) on the [Context]
// before returning; for HEAD the framework then sends a status-only response
// and the returned body is discarded. Setting headers and returning nil is
// the way to decorate the default RFC 7807 body.
//
// For the rare response that is not JSON at all, the renderer may commit the
// response itself through the [Context] (as any handler could); once
// [Response.Committed] reports true the return value is ignored.
//
// Register a custom renderer with [App.SetErrorRenderer].
type ErrorRenderer func(ctx *Context, info ErrorInfo) any

// RenderInfo carries a successful response's status, payload, and optional
// envelope side channels to the [SuccessRenderer].
type RenderInfo struct {
	// Status is the HTTP status code the framework writes.
	Status int

	// Data is the payload the handler passed to [Context.Render].
	Data any

	// MessageKey is an optional i18n message key attached via
	// [RenderMessageKey]; empty when none was given. A renderer whose
	// envelope carries a human-readable message resolves it, typically
	// through [Context.T].
	MessageKey string

	// Meta is optional structured metadata attached via [RenderMeta]
	// (pagination, request echo, …); nil when none was given.
	Meta any
}

// SuccessRenderer shapes the body of a successful response sent through
// [Context.Render]. It is the success-side mirror of [ErrorRenderer] and
// follows the same shape-only contract: the renderer returns the body, and the
// framework owns the write — status (info.Status), the application JSON
// profile, and the body-forbidding status rule (1xx/204/304 render
// status-only) all apply centrally.
//
// The return value is the response body. A non-nil value is encoded as JSON
// with the application's JSON profile and written with info.Status; returning
// nil writes info.Data plain — the escape for a renderer that envelopes
// selectively. For the rare response that is not JSON at all, the renderer may
// commit the response itself through the [Context] (as any handler could);
// once [Response.Committed] reports true the return value is ignored.
//
// It is opt-in, never installed by default, and consulted only through
// [Context.Render] — the raw [Response] helpers ([Response.JSON],
// [Response.XML], [Response.Text], [Response.Blob], and the streaming writers)
// stay un-intercepted so webhooks, health probes, and third-party response
// shapes always bypass any house envelope. A renderer that panics is treated
// like a handler panic and caught by the built-in recovery layer.
//
// Register a custom renderer with [App.SetSuccessRenderer]. The single
// [RenderInfo] seam is also the integration point a future typed-endpoint
// layer would route its typed result through, so one envelope policy covers
// both.
type SuccessRenderer func(ctx *Context, info RenderInfo) any

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

package credo

import internalrequestid "github.com/credo-go/credo/internal/requestid"

// requestIDKey is the context-store key for the request ID.
// Uses the same value as middleware.RequestIDKey for compatibility.
const requestIDKey = internalrequestid.Key

// builtinRequestID assigns a unique ID to each request and enriches the
// request-scoped logger with a "request_id" attribute. It is applied in
// compile() between builtinRecover and builtinAccessLog. Disabled via
// [WithoutRequestID].
//
// If the incoming request carries a valid X-Request-Id header, that value is
// preserved. Otherwise a new cryptographically random ID is generated. The
// current request ID can be read via [Context.RequestID].
func builtinRequestID(next Handler) Handler {
	return func(ctx *Context) error {
		id := internalrequestid.Resolve(
			ctx.Request().Header.Get(internalrequestid.Header),
			internalrequestid.DefaultLimit,
			internalrequestid.Generate,
		)

		ctx.Set(requestIDKey, id)
		ctx.Response().Header().Set(internalrequestid.Header, id)

		// Enrich request-scoped logger with request_id.
		ctx.AddLogAttrs("request_id", id)

		return next(ctx)
	}
}

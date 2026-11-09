// Package middleware provides built-in HTTP middleware for the Credo framework.
//
// All middleware in this package returns [credo.Middleware], the single
// middleware type used throughout Credo:
//
//	func(credo.Handler) credo.Handler
//
// Stdlib-compatible middleware (func(http.Handler) http.Handler) can be
// adapted using [credo.WrapStdMiddleware].
//
// # Config Struct Pattern
//
// Middleware with options uses an optional config parameter:
//
//	app.GlobalMiddleware(middleware.CORS())                            // default config
//	app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{...}))  // custom config
//
// # Framework Features Are Not Middleware
//
// Panic recovery, request IDs, access logging, response compression, request
// decompression and locale detection are framework-owned HTTP features of the
// root package, not middleware: recovery is on by default (configure it with
// [credo.WithRecoverConfig], disable it with [credo.WithoutRecover]) and the
// others are installed once per App through [credo.App.UseRequestID],
// [credo.App.UseAccessLog], [credo.App.UseCompress], [credo.App.UseDecompress]
// and [credo.App.UseI18n]. The request executor runs them in a fixed order
// around the whole user chain, so they observe the final response — error
// envelopes and panic responses included — and cannot be applied per route or
// per group. Handlers read the current request ID via c.RequestID().
//
// Most middleware in this package expose a [Skipper] for selective application.
//
// # Recommended Middleware Order
//
// Add global middleware for cross-cutting concerns:
//
//	app.GlobalMiddleware(
//	    middleware.Secure(),  // Security headers
//	    middleware.CORS(),    // CORS headers
//	)
//
// Global middleware runs on every request, including 404 and 405 responses.
// Group middleware (group.Middleware) only runs on matched routes in that group.
// Group middleware is captured at route registration time: calling
// group.Middleware(...) affects routes registered after that call, not previously
// registered routes.
//
// Middleware in this package:
//   - Rewrite(cfg ...RewriteConfig) — pre-dispatch URL path rewriting
//   - CORS(cfg ...CORSConfig)
//   - CSRF(cfg ...CSRFConfig) — Sec-Fetch-Site based, no tokens
//   - Secure(cfg ...SecureConfig)
//   - Timeout(cfg ...TimeoutConfig)
//
// Rate limiting is the package github.com/credo-go/credo/middleware/ratelimit
// (ratelimit.Middleware(cfg ...ratelimit.Config)), so that importing this
// package does not link go-limiter.
//
// Maturity: beta
package middleware

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
//	app.GlobalMiddleware(middleware.AccessLog())                          // default config
//	app.GlobalMiddleware(middleware.AccessLog(middleware.AccessLogConfig{...})) // custom config
//
// # Built-in vs Configurable
//
// Credo provides built-in versions of Recover, RequestID, and AccessLog that are
// auto-enabled with zero configuration. Use the middleware package equivalents
// when you need custom configuration (e.g., custom header, skipper, custom
// logger). Disable the built-in first to avoid duplicates:
//
//	app, _ := credo.New(
//	    credo.WithoutRequestID(),     // disable built-in
//	    credo.WithoutAccessLog(), // disable built-in
//	)
//	app.GlobalMiddleware(
//	    middleware.RequestID(middleware.RequestIDConfig{Header: "X-Trace-Id"}),
//	    middleware.AccessLog(middleware.AccessLogConfig{Skipper: mySkipper}),
//	)
//
// Most configurable middleware in this package expose a [Skipper] for
// selective application. RequestID and Recover intentionally do not:
// RequestID is expected to run on every request, and Recover is intended to
// remain the outermost safety net.
//
// Handlers can read the current request ID via ctx.RequestID() or
// [GetRequestID].
//
// # Recommended Middleware Order
//
// Built-in middleware (recover, requestID, access log) runs automatically.
// Add extra global middleware for additional cross-cutting concerns:
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
// Additional middleware in this package:
//   - Rewrite(rules ...RewriteRule) — pre-dispatch URL path rewriting
//   - CORS(cfg ...CORSConfig)
//   - CSRF(cfg ...CSRFConfig) — Sec-Fetch-Site based, no tokens
//   - Secure(cfg ...SecureConfig)
//   - Compress(cfg ...CompressConfig)
//   - Timeout(cfg ...TimeoutConfig)
//   - RateLimit(cfg ...RateLimitConfig)
//
// # RateLimit Lifecycle
//
// For explicit lifecycle control, use [NewRateLimiter] and register
// limiter.Shutdown with app.OnShutdown:
//
//	ratelimiter := middleware.NewRateLimiter(middleware.RateLimitConfig{Tokens: 120})
//	app.GlobalMiddleware(ratelimiter.Middleware())
//	app.OnShutdown(ratelimiter.Shutdown)
package middleware

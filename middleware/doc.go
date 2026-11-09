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
//	app.PreRouting(middleware.CORS())                            // default config
//	app.PreRouting(middleware.CORS(middleware.CORSConfig{...}))  // custom config
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
// The user tiers run PreRouting → routed → Group → Route. A middleware that
// acts before the match or on a request no route matches goes before routing;
// one that reads the matched route goes on the routed tier, which runs on
// every route of the App and never for the router's 404 and 405:
//
//	app.PreRouting(
//	    middleware.Secure(), // headers on the 404 and 405 too
//	    middleware.CORS(),   // answers a preflight no route serves
//	)
//	app.Middleware(middleware.ContractGuard()) // reads each route's meta
//
// Rewrite (it changes the path that routing matches) and RateLimit stay
// before routing; CSRF works on either tier; authentication and
// ContractGuard go on the routed tier, a group or a route. Group middleware
// (group.Middleware) runs on the routes of that group. Routed and group
// middleware are collected when the routes' chains are compiled, so they
// apply to routes registered before the call too.
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

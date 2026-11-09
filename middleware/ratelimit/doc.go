// Package ratelimit provides token-bucket rate-limiting middleware for the
// Credo framework, over go-limiter's store contract.
//
// [Middleware] follows the configuration-struct convention of the
// middleware package: zero arguments for the defaults (60 requests a minute
// per client IP), one [Config] for custom settings:
//
//	app.GlobalMiddleware(ratelimit.Middleware())
//	api.Middleware(ratelimit.Middleware(ratelimit.Config{Tokens: 120}))
//
// The response of every request it counts carries X-RateLimit-Limit,
// X-RateLimit-Remaining and X-RateLimit-Reset; a denied one also carries
// Retry-After and answers 429 through the error pipeline, or through
// Config.DeniedHandler. The default key is Request.RealIP, so behind a reverse
// proxy configure credo.WithTrustedProxies; Config.KeyFunc keys by anything
// else, such as an API key.
//
// Rate limiting lives in a package of its own so that only the applications
// that use it compile go-limiter: importing the middleware package, for CORS
// say, does not link it.
//
// # Lifecycle
//
// The middleware is not a component and is never handed to app.Manage. A
// value is a component when it owns something the App must release or
// prepare; being middleware is a reason neither way. The middleware a
// [Middleware] call returns holds one store, shared by every route and group
// it is attached to, so attaching one value to several routes gives them one
// budget per key, while each call keeps a budget of its own:
//
//	limit := ratelimit.Middleware(ratelimit.Config{Tokens: 120})
//	app.POST("/login", login).Middleware(limit)
//	app.POST("/password-reset", resetPassword).Middleware(limit) // same budget
//
// Without Config.Store the middleware creates an in-memory store that runs
// no goroutine — expired buckets are swept inline during Take — so there is
// nothing to stop at shutdown; its memory goes with the middleware. A store
// passed in through Config.Store, a shared Redis store or go-limiter's
// memorystore with its sweeper goroutine, stays its owner's: the middleware
// never closes it, and the application closes it, or hands it to the App as
// a component, as it does any other resource it opened.
//
// Maturity: beta
package ratelimit

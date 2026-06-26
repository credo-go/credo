// Package auth provides authentication infrastructure for Credo applications.
//
// Instead of a built-in User field on Context, Credo exposes the
// authenticated principal through generic *credo.Context methods, and this
// package provides the Authenticator[T] strategy interface and a middleware
// factory that populates it. See ADR-012 for the design rationale.
//
// # User Accessors
//
// The authenticated user is attached and read through Context methods, with
// compile-time type safety:
//
//	// In middleware (auth.Middleware calls SetUser for you)
//	ctx.SetUser(myUser)
//
//	// In a Credo handler
//	user, err := ctx.RequireUser[*MyUser]()
//	if err != nil { ... }
//
// # Authenticator Interface
//
// Implement Authenticator[T] for your auth strategy (JWT, session, API key):
//
//	type MyAuth struct { ... }
//	func (a *MyAuth) Authenticate(r *http.Request) (*User, error) { ... }
//
// # Middleware Factory
//
// Create middleware from any Authenticator:
//
//	app.GlobalMiddleware(auth.Middleware[*User](myAuth, nil))
//
// For custom auth failure handling, provide an ErrorFunc:
//
//	auth.Middleware[*User](myAuth, func(err error, ctx *credo.Context) error {
//	    return ctx.Response().JSON(401, map[string]string{"error": err.Error()})
//	})
//
// # Built-in Authenticators
//
// This package includes reusable Authenticator implementations:
//
//   - NewJWTAuthenticator[T]
//   - NewAPIKeyAuthenticator[T]
//   - NewBasicAuthenticator[T]
//
// # JWT: Simple and Advanced Tiers
//
// JWTConfig is two-tiered. The simple tier is Credo-typed: token
// extraction, signing keys, registered-claim validation (Issuer, Audience,
// Leeway, RequireExpiry), and ParseClaims, which receives a [JWTClaims]
// view with typed accessors (ExpiresAt as time.Time, GetString for custom
// claims).
//
// # Advanced golang-jwt Integration
//
// The JWTConfig.Advanced field exposes the underlying golang-jwt/jwt/v5
// primitives — KeyFunc (e.g. JWKS lookup), NewClaims (typed claims
// structs), ParseToken (raw *jwt.Token access), ParserOptions — for setups
// the simple tier cannot express. Configuration placed there is coupled to
// the golang-jwt API by design; see [JWTAdvanced].
package auth

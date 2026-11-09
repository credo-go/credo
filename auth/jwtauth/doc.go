// Package jwtauth provides a JWT authenticator for Credo applications. It
// lives apart from package auth so that only an application that verifies
// JWTs compiles golang-jwt/jwt/v5.
//
// An [Authenticator] implements auth.Authenticator[T]; auth.Middleware turns
// it into a middleware that stores the user on the request:
//
//	a, err := jwtauth.NewAuthenticator(jwtauth.Config[User]{
//	    SigningKey: key,
//	    ParseClaims: func(claims jwtauth.Claims) (User, error) {
//	        return User{ID: claims.Subject()}, nil
//	    },
//	})
//	if err != nil { ... }
//	app.Middleware(auth.Middleware[User](a, nil))
//
// The name is not jwt, golang-jwt's package name, which code that configures
// [Config.Advanced] imports beside it.
//
// # Simple and Advanced Tiers
//
// [Config] is two-tiered. The simple tier is Credo-typed: token extraction,
// signing keys, registered-claim validation (Issuer, Audience, Leeway,
// RequireExpiry), and ParseClaims, which receives a [Claims] view with typed
// accessors (ExpiresAt as time.Time, GetString for custom claims).
//
// # Advanced golang-jwt Integration
//
// The Config.Advanced field exposes the underlying golang-jwt/jwt/v5
// primitives — KeyFunc (e.g. JWKS lookup), NewClaims (typed claims
// structs), ParseToken (raw *jwt.Token access), ParserOptions — for setups
// the simple tier cannot express. Configuration placed there is coupled to
// the golang-jwt API by design; see [Advanced].
//
// Maturity: beta
package jwtauth

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
//	c.SetUser(myUser)
//
//	// In a Credo handler
//	user, err := c.RequireUser[*MyUser]()
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
//	app.Middleware(auth.Middleware[*User](myAuth)) // every route, mounts included
//
// A failed authentication is a 401 by default. To decide the failure response,
// pass a [Config] with an ErrorHandler; like every callback in the framework
// it takes the context first:
//
//	auth.Middleware[*User](myAuth, auth.Config{
//	    ErrorHandler: func(c *credo.Context, err error) error {
//	        return credo.NewHTTPError(http.StatusUnauthorized, "session_expired").WithInternal(err)
//	    },
//	})
//
// # Built-in Authenticators
//
// This package includes reusable Authenticator implementations:
//
//   - NewAPIKeyAuthenticator[T]
//   - NewBasicAuthenticator[T]
//
// The JWT authenticator is in package auth/jwtauth, so that only an
// application that verifies JWTs compiles golang-jwt.
//
// Maturity: beta
package auth

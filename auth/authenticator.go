package auth

import (
	"net/http"

	"github.com/credo-go/credo"
)

// Authenticator validates a request and returns the authenticated user.
// T is the application's user type (e.g., *MyUser, Claims, etc.).
type Authenticator[T any] interface {
	Authenticate(r *http.Request) (T, error)
}

// ErrorFunc is called when authentication fails. It receives the
// error from the Authenticator and should return an appropriate
// HTTP error (or nil to use the default 401 response).
type ErrorFunc func(err error, ctx *credo.Context) error

// Middleware creates an credo.Middleware that authenticates requests
// using the given Authenticator. If authentication succeeds, the user is
// stored on the request via ctx.SetUser and is then accessible to
// downstream handlers via ctx.GetUser[T]().
//
// When authentication fails and onError is nil (or returns nil), the middleware returns
// credo.ErrUnauthorized with the authenticator's error as Internal.
// Provide an ErrorFunc to customize the failure response.
//
// Panics if a is nil: a missing authenticator is a wiring error, so it fails
// fast at construction instead of rejecting every request at runtime.
func Middleware[T any](a Authenticator[T], onError ErrorFunc) credo.Middleware {
	if a == nil {
		panic("auth: Middleware requires a non-nil authenticator")
	}

	handleAuthError := func(err error, ctx *credo.Context) error {
		if onError != nil {
			if handledErr := onError(err, ctx); handledErr != nil {
				return handledErr
			}
		}
		return credo.ErrUnauthorized.WithInternal(err)
	}

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			user, err := a.Authenticate(ctx.Request().Request)
			if err != nil {
				return handleAuthError(err, ctx)
			}

			// Attach the authenticated user to the request.
			ctx.SetUser(user)

			return next(ctx)
		}
	}
}

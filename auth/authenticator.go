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

// Config configures [Middleware]. A zero field selects its default, so
// passing no config is the same as passing the zero value.
type Config struct {
	// ErrorHandler maps a failed authentication to the error the middleware
	// returns. It receives the error from the Authenticator, and may set
	// response headers before it returns, as [BasicErrorHandler] sets
	// WWW-Authenticate. A handler that returns nil selects the default too:
	// a failed authentication never reaches the route's handler.
	// Default: credo.ErrUnauthorized with the authenticator's error as the
	// internal error.
	ErrorHandler func(ctx *credo.Context, err error) error
}

// Middleware creates an credo.Middleware that authenticates requests
// using the given Authenticator. If authentication succeeds, the user is
// stored on the request via c.SetUser and is then accessible to
// downstream handlers via c.GetUser[T]().
//
// When authentication fails, the middleware returns what
// [Config.ErrorHandler] returns: by default, and when the handler returns
// nil, credo.ErrUnauthorized with the authenticator's error as Internal.
//
// Panics if a is nil: a missing authenticator is a wiring error, so it fails
// fast at construction instead of rejecting every request at runtime.
func Middleware[T any](a Authenticator[T], cfg ...Config) credo.Middleware {
	if a == nil {
		panic("auth: Middleware requires a non-nil authenticator")
	}

	var config Config
	if len(cfg) > 0 {
		config = cfg[0]
	}

	handleAuthError := func(ctx *credo.Context, err error) error {
		if config.ErrorHandler != nil {
			if handledErr := config.ErrorHandler(ctx, err); handledErr != nil {
				return handledErr
			}
		}
		return credo.ErrUnauthorized.WithInternal(err)
	}

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			user, err := a.Authenticate(ctx.Request().Std())
			if err != nil {
				return handleAuthError(ctx, err)
			}

			// Attach the authenticated user to the request.
			ctx.SetUser(user)

			return next(ctx)
		}
	}
}

package auth

import (
	"errors"
	"net/http"
	"reflect"

	"github.com/credo-go/credo"
)

var (
	// ErrAuthenticatorRequired is returned when auth middleware is built
	// without an authenticator.
	ErrAuthenticatorRequired = errors.New("auth: authenticator is required")
)

func isNilAuthenticator[T any](a Authenticator[T]) bool {
	if a == nil {
		return true
	}
	v := reflect.ValueOf(a)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

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
func Middleware[T any](a Authenticator[T], onError ErrorFunc) credo.Middleware {
	handleAuthError := func(err error, ctx *credo.Context) error {
		if onError != nil {
			if handledErr := onError(err, ctx); handledErr != nil {
				return handledErr
			}
		}
		return credo.ErrUnauthorized.WithInternal(err)
	}

	if isNilAuthenticator(a) {
		return func(credo.Handler) credo.Handler {
			return func(ctx *credo.Context) error {
				return handleAuthError(ErrAuthenticatorRequired, ctx)
			}
		}
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

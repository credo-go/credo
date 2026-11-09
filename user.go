package credo

import "errors"

// ErrUserMissing is returned by [Context.RequireUser] when no authenticated
// user of the requested type is present on the request. RequireUser wraps it
// inside [ErrUnauthorized], so a handler that returns the error renders a 401
// while errors.Is(err, ErrUserMissing) still reports the underlying cause.
var ErrUserMissing = errors.New("credo: no user in context (or type mismatch)")

// userKey is the unexported, per-type key under which the authenticated
// principal is stored on the request's context, as a value of [Context.Set]
// is. The type parameter gives every user type its own slot, so distinct
// principals — e.g. a JWT end-user and an API-key service account — coexist
// within one request without colliding. Because the key type is unexported,
// no other package can read or overwrite the slot: the only access path is
// the user methods of the Context.
type userKey[T any] struct{}

// SetUser stores the authenticated user on the request, keyed by its type T.
// It is the framework's blessed way to attach a principal: T is inferred from
// the argument, so call sites read c.SetUser(user) with no explicit type
// argument. Storing a different type adds a separate slot; storing the same
// type again replaces the previous value.
//
// [github.com/credo-go/credo/auth.Middleware] calls this after a successful
// Authenticate; custom middleware may call it directly. Like a value of
// [Context.Set], a user set inside a scope outlives it.
func (c *Context) SetUser[T any](user T) {
	c.setValue(userKey[T]{}, user)
}

// GetUser returns the authenticated user previously stored under type T and
// reports whether one was present. The type argument is required because it
// cannot be inferred from a return value: c.GetUser[*User](). Retrieve with
// the same T that was stored — a value set under a concrete type is not
// visible through an interface type parameter.
func (c *Context) GetUser[T any]() (T, bool) {
	user, ok := c.lookup(userKey[T]{}).(T)
	return user, ok
}

// RequireUser is like [Context.GetUser] but returns a handler-ready error when
// the user is absent: [ErrUnauthorized] wrapping [ErrUserMissing]. The
// framework renders it as 401, and errors.Is(err, ErrUserMissing) still
// reports the cause. The type argument is required: c.RequireUser[*User]().
func (c *Context) RequireUser[T any]() (T, error) {
	user, ok := c.GetUser[T]()
	if !ok {
		var zero T
		return zero, ErrUnauthorized.WithInternal(ErrUserMissing)
	}
	return user, nil
}

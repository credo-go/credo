package auth

import (
	"context"
	"errors"
)

var (
	// ErrUserMissing is returned when no authenticated user is available
	// in context for the requested type.
	ErrUserMissing = errors.New("auth: no user in context (or type mismatch)")
)

// userKey is an unexported generic struct type used as context key.
// Being unexported prevents collisions with other packages; the type
// parameter gives every user type its own slot, so e.g. a JWT user and an
// API-key service account can coexist in one request context.
type userKey[T any] struct{}

// SetUser stores the authenticated user in the context under a slot
// derived from T. Returns a new context.Context with the user value set.
// Users of different types coexist; setting the same type again replaces
// the previous value.
func SetUser[T any](ctx context.Context, user T) context.Context {
	return context.WithValue(ctx, userKey[T]{}, user)
}

// GetUser retrieves the authenticated user stored under type T.
// Returns the user and true if present, zero value and false otherwise.
// T must be the same type that was passed to [SetUser] — the slot is
// keyed by type, so a value stored as a concrete type is not visible
// through an interface type parameter.
func GetUser[T any](ctx context.Context) (T, bool) {
	user, ok := ctx.Value(userKey[T]{}).(T)
	return user, ok
}

// RequireUser retrieves the authenticated user from the context.
// Returns ErrUserMissing if the user is absent or has a different type.
func RequireUser[T any](ctx context.Context) (T, error) {
	user, ok := GetUser[T](ctx)
	if !ok {
		var zero T
		return zero, ErrUserMissing
	}
	return user, nil
}

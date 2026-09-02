package auth_test

import (
	"testing"

	"github.com/credo-go/credo/auth"
)

func TestMiddleware_NilAuthenticatorPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for nil authenticator")
		}
		if got, want := r, "auth: Middleware requires a non-nil authenticator"; got != want {
			t.Fatalf("panic = %v, want %q", got, want)
		}
	}()
	auth.Middleware[any](nil, nil)
}

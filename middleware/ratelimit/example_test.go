package ratelimit_test

import (
	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware/ratelimit"
)

// One Middleware value holds one store: attached to two routes, it gives a
// client one budget of 120 requests a minute across both.
func ExampleMiddleware() {
	app, err := credo.New()
	if err != nil {
		panic(err)
	}

	limit := ratelimit.Middleware(ratelimit.Config{Tokens: 120})
	app.POST("/login", login).Middleware(limit)
	app.POST("/password-reset", resetPassword).Middleware(limit)
}

func login(*credo.Context) error         { return nil }
func resetPassword(*credo.Context) error { return nil }

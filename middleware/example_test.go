package middleware_test

import (
	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func ExampleNewRateLimiter() {
	app, err := credo.New()
	if err != nil {
		panic(err)
	}

	limiter := middleware.NewRateLimiter(middleware.RateLimitConfig{Tokens: 120})
	app.GlobalMiddleware(limiter.Middleware())
	app.OnShutdown(limiter.Shutdown)
}

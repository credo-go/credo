package middleware_test

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func ExampleAccessLog() {
	app, err := credo.New(credo.WithoutAccessLog())
	if err != nil {
		panic(err)
	}

	app.GlobalMiddleware(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "ok")
	})
}

func ExampleRequestID_customHeader() {
	app, err := credo.New(credo.WithoutRequestID(), credo.WithoutAccessLog())
	if err != nil {
		panic(err)
	}

	app.GET("/", func(ctx *credo.Context) error {
		traceID := middleware.GetRequestID(ctx)
		_ = traceID
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.RequestID(middleware.RequestIDConfig{
		Header: "X-Trace-Id",
	}))
}

func ExampleNewRateLimiter() {
	app, err := credo.New()
	if err != nil {
		panic(err)
	}

	limiter := middleware.NewRateLimiter(middleware.RateLimitConfig{Tokens: 120})
	app.GlobalMiddleware(limiter.Middleware())
	app.OnShutdown(limiter.Shutdown)
}

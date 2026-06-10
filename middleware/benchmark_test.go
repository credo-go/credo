package middleware_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

// BenchmarkRequestID measures the full RequestID middleware cycle:
// generate ID, ctx.Set (no context.WithValue alloc), set response header.
func BenchmarkRequestID(b *testing.B) {
	app := mustNewBench(b)
	app.GlobalMiddleware(middleware.RequestID(middleware.RequestIDConfig{
		Generator: func() string { return "bench-id-0123456789" },
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "ok")
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkRequestID_GetRequestID measures reading the request ID back from
// the context-local store via GetRequestID.
func BenchmarkRequestID_GetRequestID(b *testing.B) {
	app := mustNewBench(b)
	app.GlobalMiddleware(middleware.RequestID(middleware.RequestIDConfig{
		Generator: func() string { return "bench-id-0123456789" },
	}))
	app.GET("/", func(ctx *credo.Context) error {
		_ = middleware.GetRequestID(ctx)
		return ctx.Response().Text(http.StatusOK, "ok")
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkSecure measures security headers with pre-computed HSTS value.
func BenchmarkSecure(b *testing.B) {
	app := mustNewBench(b, credo.WithTrustedProxies("10.0.0.0/8"))
	app.GlobalMiddleware(middleware.Secure(middleware.SecureConfig{
		XSSProtection:      "1; mode=block",
		ContentTypeNosniff: "nosniff",
		XFrameOptions:      "SAMEORIGIN",
		HSTSMaxAge:         31536000,
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "ok")
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-Proto", "https")

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkAccessLog measures the access log middleware with fixed-size [8]slog.Attr
// array and io.Discard output to isolate middleware overhead from I/O.
func BenchmarkAccessLog(b *testing.B) {
	app := mustNewBench(b)
	app.GlobalMiddleware(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "ok")
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkCompress_Gzip measures gzip compression with pooled writers.
func BenchmarkCompress_Gzip(b *testing.B) {
	app := mustNewBench(b)
	app.GlobalMiddleware(middleware.Compress())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "hello compressed world")
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkCompress_Parallel measures concurrent gzip compression,
// stressing the gzip writer pool under contention.
func BenchmarkCompress_Parallel(b *testing.B) {
	app := mustNewBench(b)
	app.GlobalMiddleware(middleware.Compress())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "hello compressed world")
	})

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		w := newNoopResponseWriter()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		for pb.Next() {
			clear(w.h)
			app.ServeHTTP(w, r)
		}
	})
}

// BenchmarkRateLimit measures rate limiting with the sharded in-memory store.
// Uses high token count (1M) so requests never get denied during the benchmark.
func BenchmarkRateLimit(b *testing.B) {
	rl := middleware.NewRateLimiter(middleware.RateLimitConfig{
		Tokens:   1_000_000,
		Interval: time.Hour,
	})
	b.Cleanup(func() { _ = rl.Shutdown(context.Background()) })

	app := mustNewBench(b)
	app.GlobalMiddleware(rl.Middleware())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "ok")
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.168.1.1:12345"

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkRateLimit_Parallel measures concurrent rate limiting,
// stressing the sharded mutex under contention.
func BenchmarkRateLimit_Parallel(b *testing.B) {
	rl := middleware.NewRateLimiter(middleware.RateLimitConfig{
		Tokens:   1_000_000,
		Interval: time.Hour,
	})
	b.Cleanup(func() { _ = rl.Shutdown(context.Background()) })

	app := mustNewBench(b)
	app.GlobalMiddleware(rl.Middleware())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "ok")
	})

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		w := newNoopResponseWriter()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "192.168.1.1:12345"
		for pb.Next() {
			clear(w.h)
			app.ServeHTTP(w, r)
		}
	})
}

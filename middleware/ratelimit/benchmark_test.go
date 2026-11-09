package ratelimit_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware/ratelimit"
)

// BenchmarkMiddleware measures rate limiting with the sharded in-memory store.
// Uses high token count (1M) so requests never get denied during the benchmark.
func BenchmarkMiddleware(b *testing.B) {
	app := mustNewBench(b)
	app.GlobalMiddleware(ratelimit.Middleware(ratelimit.Config{
		Tokens:   1_000_000,
		Interval: time.Hour,
	}))
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

// BenchmarkMiddleware_Parallel measures concurrent rate limiting,
// stressing the sharded mutex under contention.
func BenchmarkMiddleware_Parallel(b *testing.B) {
	app := mustNewBench(b)
	app.GlobalMiddleware(ratelimit.Middleware(ratelimit.Config{
		Tokens:   1_000_000,
		Interval: time.Hour,
	}))
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

package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

// BenchmarkSecure measures security headers with pre-computed HSTS value.
func BenchmarkSecure(b *testing.B) {
	app := mustNewBench(b, credo.WithTrustedProxies("10.0.0.0/8"))
	app.PreRouting(middleware.Secure(middleware.SecureConfig{
		XSSProtection: "1; mode=block",
		HSTSMaxAge:    31536000,
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

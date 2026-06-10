package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func TestSecure_DefaultHeaders(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.Secure())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-XSS-Protection"); got != "1; mode=block" {
		t.Fatalf("X-XSS-Protection = %q, want %q", got, "1; mode=block")
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
}

func TestSecure_HSTS_FromForwardedProto(t *testing.T) {
	app := mustNew(t, credo.WithTrustedProxies("10.0.0.0/8"))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.Secure(middleware.SecureConfig{
		Skipper:               middleware.DefaultSkipper,
		HSTSMaxAge:            3600,
		HSTSPreloadEnabled:    true,
		HSTSExcludeSubdomains: false,
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-Proto", "https")
	app.ServeHTTP(w, r)

	want := "max-age=3600; includeSubDomains; preload"
	if got := w.Header().Get("Strict-Transport-Security"); got != want {
		t.Fatalf("Strict-Transport-Security = %q, want %q", got, want)
	}
}

func TestSecure_Skipper(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.Secure(middleware.SecureConfig{
		Skipper: func(ctx *credo.Context) bool {
			return true
		},
		XFrameOptions: "DENY",
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-Frame-Options"); got != "" {
		t.Fatalf("X-Frame-Options = %q, want empty (skipped)", got)
	}
}

func TestSecure_HSTS_ForwardedProtoIgnoredWhenUntrusted(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.Secure(middleware.SecureConfig{
		Skipper:    middleware.DefaultSkipper,
		HSTSMaxAge: 3600,
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	app.ServeHTTP(w, r)

	if got := w.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("Strict-Transport-Security = %q, want empty", got)
	}
}

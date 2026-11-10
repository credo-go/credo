package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

// secureHeaders are the headers Secure can set. A test case names the ones it
// expects; every other one must be absent.
var secureHeaders = []string{
	"X-XSS-Protection",
	"X-Content-Type-Options",
	"X-Frame-Options",
	"Strict-Transport-Security",
	"Content-Security-Policy",
	"Content-Security-Policy-Report-Only",
	"Referrer-Policy",
}

// TestSecure_ZeroFieldSelectsItsDefault pins the config rule for Secure: a
// field left out keeps its default, so nosniff and X-Frame-Options are sent
// unless their Disable field says otherwise, and no other header is sent
// unless its field is set.
func TestSecure_ZeroFieldSelectsItsDefault(t *testing.T) {
	const (
		nosniff    = "X-Content-Type-Options"
		frame      = "X-Frame-Options"
		sameOrigin = "SAMEORIGIN"
	)
	tests := []struct {
		name string
		cfg  []middleware.SecureConfig
		want map[string]string
	}{
		{
			name: "no config",
			want: map[string]string{nosniff: "nosniff", frame: sameOrigin},
		},
		{
			name: "zero config",
			cfg:  []middleware.SecureConfig{{}},
			want: map[string]string{nosniff: "nosniff", frame: sameOrigin},
		},
		{
			name: "HSTS only keeps both defaults",
			cfg:  []middleware.SecureConfig{{HSTSMaxAge: 31536000}},
			want: map[string]string{
				nosniff:                     "nosniff",
				frame:                       sameOrigin,
				"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
			},
		},
		{
			name: "XFrameOptions set keeps nosniff",
			cfg:  []middleware.SecureConfig{{XFrameOptions: "DENY"}},
			want: map[string]string{nosniff: "nosniff", frame: "DENY"},
		},
		{
			name: "CSP and Referrer-Policy keep both defaults",
			cfg: []middleware.SecureConfig{{
				ContentSecurityPolicy: "default-src 'self'",
				ReferrerPolicy:        "no-referrer",
			}},
			want: map[string]string{
				nosniff:                   "nosniff",
				frame:                     sameOrigin,
				"Content-Security-Policy": "default-src 'self'",
				"Referrer-Policy":         "no-referrer",
			},
		},
		{
			name: "report-only CSP",
			cfg: []middleware.SecureConfig{{
				ContentSecurityPolicy: "default-src 'self'",
				CSPReportOnly:         true,
			}},
			want: map[string]string{
				nosniff:                               "nosniff",
				frame:                                 sameOrigin,
				"Content-Security-Policy-Report-Only": "default-src 'self'",
			},
		},
		{
			name: "DisableContentTypeNosniff keeps X-Frame-Options",
			cfg:  []middleware.SecureConfig{{DisableContentTypeNosniff: true}},
			want: map[string]string{frame: sameOrigin},
		},
		{
			name: "DisableXFrameOptions keeps nosniff",
			cfg:  []middleware.SecureConfig{{DisableXFrameOptions: true}},
			want: map[string]string{nosniff: "nosniff"},
		},
		{
			name: "DisableXFrameOptions wins over XFrameOptions",
			cfg:  []middleware.SecureConfig{{DisableXFrameOptions: true, XFrameOptions: "DENY"}},
			want: map[string]string{nosniff: "nosniff"},
		},
		{
			name: "both disabled",
			cfg: []middleware.SecureConfig{{
				DisableContentTypeNosniff: true,
				DisableXFrameOptions:      true,
			}},
			want: map[string]string{},
		},
		{
			name: "X-XSS-Protection only when set",
			cfg:  []middleware.SecureConfig{{XSSProtection: "0"}},
			want: map[string]string{nosniff: "nosniff", frame: sameOrigin, "X-XSS-Protection": "0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// HSTS is sent on an https request only, so every case is one.
			app := mustNew(t, credo.WithTrustedProxies("10.0.0.0/8"))
			app.GET("/", func(ctx *credo.Context) error {
				return ctx.Response().NoContent(http.StatusOK)
			}).Middleware(middleware.Secure(tt.cfg...))

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.0.0.1:1234"
			r.Header.Set("X-Forwarded-Proto", "https")
			app.ServeHTTP(w, r)

			for _, name := range secureHeaders {
				got := w.Header().Values(name)
				want, wanted := tt.want[name]
				switch {
				case wanted && (len(got) != 1 || got[0] != want):
					t.Errorf("%s = %q, want %q", name, got, want)
				case !wanted && len(got) > 0:
					t.Errorf("%s = %q, want the header absent", name, got)
				}
			}
		})
	}
}

func TestSecure_HSTS_FromForwardedProto(t *testing.T) {
	app := mustNew(t, credo.WithTrustedProxies("10.0.0.0/8"))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.Secure(middleware.SecureConfig{
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

	for _, name := range secureHeaders {
		if got := w.Header().Values(name); len(got) > 0 {
			t.Errorf("%s = %q, want the header absent (skipped)", name, got)
		}
	}
}

func TestSecure_HSTS_ForwardedProtoIgnoredWhenUntrusted(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.Secure(middleware.SecureConfig{
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

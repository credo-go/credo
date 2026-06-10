package credo_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

func TestRequestSchemeAndRealIP(t *testing.T) {
	tests := []struct {
		name           string
		trustedProxies []string
		remoteAddr     string
		headers        map[string]string
		want           string
	}{
		{
			name:       "default deny ignores forwarded headers",
			remoteAddr: "203.0.113.1:1234",
			headers: map[string]string{
				"X-Forwarded-For":   "1.1.1.1",
				"X-Forwarded-Proto": "https",
			},
			want: "http|203.0.113.1",
		},
		{
			name:           "trusted proxy accepts forwarded scheme and ip",
			trustedProxies: []string{"10.0.0.0/8"},
			remoteAddr:     "10.0.0.1:1234",
			headers: map[string]string{
				"X-Forwarded-For":   "1.1.1.1",
				"X-Forwarded-Proto": "https",
			},
			want: "https|1.1.1.1",
		},
		{
			name:           "trusted chain walks from right",
			trustedProxies: []string{"10.0.0.0/8"},
			remoteAddr:     "10.0.0.1:1234",
			headers: map[string]string{
				"X-Forwarded-For": "1.1.1.1, 10.0.0.2",
			},
			want: "http|1.1.1.1",
		},
		{
			name:           "all xff hops trusted uses leftmost valid",
			trustedProxies: []string{"10.0.0.0/8"},
			remoteAddr:     "10.0.0.9:1234",
			headers: map[string]string{
				"X-Forwarded-For": "10.0.0.5, 10.0.0.3, 10.0.0.1",
			},
			want: "http|10.0.0.5",
		},
		{
			name:           "hop limit clamps long xff",
			trustedProxies: []string{"10.0.0.0/8"},
			remoteAddr:     "10.0.0.9:1234",
			headers: map[string]string{
				"X-Forwarded-For": longRequestForwardedForChain(),
			},
			want: "http|10.0.0.1",
		},
		{
			name:           "ipv4 mapped ipv6 xff is unmapped",
			trustedProxies: []string{"10.0.0.0/8"},
			remoteAddr:     "10.0.0.1:1234",
			headers: map[string]string{
				"X-Forwarded-For": "::ffff:1.1.1.1",
			},
			want: "http|1.1.1.1",
		},
		{
			name:           "invalid xff entry is skipped",
			trustedProxies: []string{"10.0.0.0/8"},
			remoteAddr:     "10.0.0.1:1234",
			headers: map[string]string{
				"X-Forwarded-For": "evil, 10.0.0.2",
			},
			want: "http|10.0.0.2",
		},
		{
			name:           "x-real-ip fallback",
			trustedProxies: []string{"10.0.0.0/8"},
			remoteAddr:     "10.0.0.1:1234",
			headers: map[string]string{
				"X-Real-IP": "198.51.100.20",
			},
			want: "http|198.51.100.20",
		},
		{
			name:           "empty xff and xri falls back to trusted peer",
			trustedProxies: []string{"10.0.0.0/8"},
			remoteAddr:     "10.0.0.1:1234",
			headers: map[string]string{
				"X-Forwarded-For": " ",
				"X-Real-IP":       " ",
			},
			want: "http|10.0.0.1",
		},
		{
			name:           "invalid scheme header falls back",
			trustedProxies: []string{"10.0.0.0/8"},
			remoteAddr:     "10.0.0.1:1234",
			headers: map[string]string{
				"X-Forwarded-Proto": "//evil.com",
			},
			want: "http|10.0.0.1",
		},
		{
			name:           "trusted ipv6 peer",
			trustedProxies: []string{"::1/128"},
			remoteAddr:     "[::1]:1234",
			headers: map[string]string{
				"X-Forwarded-Ssl": "on",
				"X-Forwarded-For": "2001:db8::1",
			},
			want: "https|2001:db8::1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t, credo.WithTrustedProxies(tt.trustedProxies...), credo.WithoutAccessLog())
			app.GET("/", func(ctx *credo.Context) error {
				return ctx.Response().Text(http.StatusOK, ctx.Request().Scheme()+"|"+ctx.Request().RealIP())
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}

			app.ServeHTTP(w, r)
			if got := w.Body.String(); got != tt.want {
				t.Fatalf("response = %q, want %q", got, tt.want)
			}
		})
	}
}

func longRequestForwardedForChain() string {
	const maxForwardedForHops = 32
	hops := make([]string, maxForwardedForHops+1)
	hops[0] = "203.0.113.10"
	for i := 1; i < len(hops); i++ {
		hops[i] = "10.0.0.1"
	}
	return strings.Join(hops, ", ")
}

func TestRequestProxyMetadataCache(t *testing.T) {
	app := mustNew(t, credo.WithTrustedProxies("10.0.0.0/8"), credo.WithoutAccessLog())
	app.GET("/", func(ctx *credo.Context) error {
		first := ctx.Request().Scheme() + "|" + ctx.Request().RealIP()
		ctx.Request().Header.Set("X-Forwarded-Proto", "http")
		ctx.Request().Header.Set("X-Forwarded-For", "198.51.100.99")
		second := ctx.Request().Scheme() + "|" + ctx.Request().RealIP()
		return ctx.Response().Text(http.StatusOK, first+";"+second)
	})

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodGet, "/", nil)
	firstReq.RemoteAddr = "10.0.0.1:1234"
	firstReq.Header.Set("X-Forwarded-Proto", "https")
	firstReq.Header.Set("X-Forwarded-For", "203.0.113.10")
	app.ServeHTTP(first, firstReq)
	if got, want := first.Body.String(), "https|203.0.113.10;https|203.0.113.10"; got != want {
		t.Fatalf("first response = %q, want %q", got, want)
	}

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodGet, "/", nil)
	secondReq.RemoteAddr = "10.0.0.1:1234"
	app.ServeHTTP(second, secondReq)
	if got, want := second.Body.String(), "http|10.0.0.1;http|10.0.0.1"; got != want {
		t.Fatalf("second response = %q, want %q", got, want)
	}
}

package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func corsAllowOrigin(t *testing.T, allowOrigins []string, origin string) string {
	t.Helper()
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{AllowOrigins: allowOrigins}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", origin)
	app.ServeHTTP(w, r)
	return w.Header().Get("Access-Control-Allow-Origin")
}

func TestCORS_OriginGrammar_Matching(t *testing.T) {
	tests := []struct {
		name   string
		allow  []string
		origin string
		want   string // expected Access-Control-Allow-Origin; "" = not allowed
	}{
		{"exact", []string{"https://example.com"}, "https://example.com", "https://example.com"},
		{"exact case-insensitive host", []string{"https://example.com"}, "https://EXAMPLE.com", "https://EXAMPLE.com"},
		{"exact implied default port", []string{"https://example.com"}, "https://example.com:443", "https://example.com:443"},
		{"exact explicit port mismatch", []string{"https://example.com"}, "https://example.com:8443", ""},
		{"exact scheme mismatch", []string{"https://example.com"}, "http://example.com", ""},
		{"wildcard one label", []string{"https://*.example.com"}, "https://app.example.com", "https://app.example.com"},
		{"wildcard rejects apex", []string{"https://*.example.com"}, "https://example.com", ""},
		{"wildcard rejects two labels", []string{"https://*.example.com"}, "https://a.b.example.com", ""},
		{"wildcard keeps port", []string{"https://*.example.com:8443"}, "https://app.example.com:8443", "https://app.example.com:8443"},
		{"wildcard port mismatch", []string{"https://*.example.com:8443"}, "https://app.example.com", ""},
		{"malformed origin header", []string{"https://example.com"}, "null", ""},
		{"origin with path never matches", []string{"https://example.com"}, "https://example.com/app", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := corsAllowOrigin(t, tt.allow, tt.origin); got != tt.want {
				t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCORS_OriginGrammar_InvalidPatternPanics(t *testing.T) {
	tests := []struct {
		name  string
		allow string
	}{
		{"mid-label wildcard", "https://api-*-prod.example.com"},
		{"two wildcards", "https://*.*.example.com"},
		{"wildcard not left-most", "https://app.*.example.com"},
		{"single-label wildcard suffix", "https://*.com"},
		{"missing scheme", "example.com"},
		{"unsupported scheme", "ftp://example.com"},
		{"path", "https://example.com/app"},
		{"userinfo", "https://user@example.com"},
		{"surrounding whitespace", " https://example.com"},
		{"empty entry", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected panic for AllowOrigins entry %q", tt.allow)
				}
				msg, _ := r.(string)
				if !strings.HasPrefix(msg, "credo: middleware.CORS: AllowOrigins[0] ") {
					t.Fatalf("panic = %v, want AllowOrigins[0] prefix", r)
				}
			}()
			middleware.CORS(middleware.CORSConfig{AllowOrigins: []string{tt.allow}})
		})
	}
}

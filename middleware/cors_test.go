package middleware_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func TestCORS_SimpleRequest_AllowedOrigin(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
		AllowOrigins: []string{"https://example.com"},
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://example.com")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want https://example.com", got)
	}
}

// TestCORS_MultipleOrigins locks the multi-origin contract: the matched
// origin (and only that origin) is echoed back per request, unmatched
// origins get no allow header, and every response — matched or not —
// carries Vary: Origin so shared caches never serve one origin's response
// to another.
func TestCORS_MultipleOrigins(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
		AllowOrigins: []string{"https://app.example.com", "https://admin.example.com"},
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	})

	tests := []struct {
		name   string
		origin string
		want   string // expected Access-Control-Allow-Origin ("" = absent)
	}{
		{"first origin matched", "https://app.example.com", "https://app.example.com"},
		{"second origin matched", "https://admin.example.com", "https://admin.example.com"},
		{"unlisted origin rejected", "https://evil.example.com", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Origin", tt.origin)
			app.ServeHTTP(w, r)

			if got := w.Header().Get("Access-Control-Allow-Origin"); got != tt.want {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tt.want)
			}
			if got := w.Header().Values("Vary"); !slices.Contains(got, "Origin") {
				t.Errorf("Vary = %v, want it to contain Origin", got)
			}
		})
	}
}

func TestCORS_Preflight_Allowed(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
		AllowOrigins: []string{"https://example.com"},
		AllowMethods: []string{http.MethodGet, http.MethodPost},
		AllowHeaders: []string{"Authorization", "Content-Type"},
		MaxAge:       600,
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodOptions, "/", nil)
	r.Header.Set("Origin", "https://example.com")
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "Authorization")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want https://example.com", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got != "GET,POST" {
		t.Fatalf("Access-Control-Allow-Methods = %q, want GET,POST", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "Authorization,Content-Type" {
		t.Fatalf("Access-Control-Allow-Headers = %q, want Authorization,Content-Type", got)
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got != "600" {
		t.Fatalf("Access-Control-Max-Age = %q, want 600", got)
	}
}

func TestCORS_DefaultPreflight_AllowsQUERY(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CORS())
	app.QUERY("/search", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodOptions, "/search", nil)
	r.Header.Set("Origin", "https://example.com")
	r.Header.Set("Access-Control-Request-Method", "QUERY")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); !slices.Contains(strings.Split(got, ","), "QUERY") {
		t.Fatalf("Access-Control-Allow-Methods = %q, want QUERY", got)
	}
}

func TestCORS_Preflight_DisallowedOrigin(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
		AllowOrigins: []string{"https://example.com"},
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodOptions, "/", nil)
	r.Header.Set("Origin", "https://evil.com")
	r.Header.Set("Access-Control-Request-Method", "GET")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func TestCORS_WildcardWithCredentials_UsesRequestOrigin(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowCredentials: true,
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://tenant.example.com")
	app.ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://tenant.example.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want request origin", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Access-Control-Allow-Credentials = %q, want true", got)
	}
}

func TestCORS_DefaultConstructor(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CORS())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://any-origin.example")
	app.ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

func TestCORS_WildcardPattern(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
		AllowOrigins: []string{"https://*.tenant.example.com"},
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://foo.tenant.example.com")
	app.ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://foo.tenant.example.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want request origin", got)
	}
}

func TestCORS_WildcardPattern_CaseInsensitive(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
		AllowOrigins: []string{"https://*.Example.COM"},
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://foo.example.com")
	app.ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://foo.example.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want request origin (case-insensitive wildcard match)", got)
	}
}

func TestCORS_WildcardPattern_NoOverlapMatch(t *testing.T) {
	// The prefix and suffix of a wildcard pattern must match disjoint
	// regions of the origin. "https://api-*-prod.example.com" must not
	// match "https://api-prod.example.com" (prefix "https://api-" and
	// suffix "-prod.example.com" would overlap on the single "-").
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
		AllowOrigins: []string{"https://api-*-prod.example.com"},
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	})

	tests := []struct {
		origin string
		want   string // expected Access-Control-Allow-Origin
	}{
		{"https://api-prod.example.com", ""},                                 // overlap — rejected
		{"https://api-x-prod.example.com", "https://api-x-prod.example.com"}, // genuine wildcard text
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Origin", tt.origin)
		app.ServeHTTP(w, r)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != tt.want {
			t.Errorf("origin %q: Access-Control-Allow-Origin = %q, want %q", tt.origin, got, tt.want)
		}
	}
}

func TestCORS_AllowOriginFuncError(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
		AllowOriginFunc: func(_ *credo.Context, _ string) (string, bool, error) {
			return "", false, errors.New("lookup failed")
		},
	}))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://example.com")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

// TestPartialConfig_KeepsOtherDefaults pins the package's one rule for a
// config struct: a zero field selects its default, so a config that sets one
// field behaves like no config in every other respect. Each case leaves
// Skipper nil, which skips no request. Secure's cases are in
// TestSecure_ZeroFieldSelectsItsDefault.
func TestPartialConfig_KeepsOtherDefaults(t *testing.T) {
	ok := func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "ok")
	}

	t.Run("CORS: MaxAge only", func(t *testing.T) {
		app := mustNew(t)
		app.PreRouting(middleware.CORS(middleware.CORSConfig{MaxAge: 600}))
		app.GET("/", ok)

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodOptions, "/", nil)
		r.Header.Set("Origin", "https://anywhere.example")
		r.Header.Set("Access-Control-Request-Method", http.MethodGet)
		app.ServeHTTP(w, r)

		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", w.Code)
		}
		for name, want := range map[string]string{
			"Access-Control-Allow-Origin":  "*",
			"Access-Control-Allow-Methods": "GET,HEAD,PUT,PATCH,POST,DELETE,QUERY",
			"Access-Control-Max-Age":       "600",
		} {
			if got := w.Header().Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
	})

	t.Run("CSRF: TrustedOrigins only", func(t *testing.T) {
		app := mustNew(t)
		app.PreRouting(middleware.CSRF(middleware.CSRFConfig{
			TrustedOrigins: []string{"https://app.example.com"},
		}))
		app.POST("/", ok)

		tests := []struct {
			name   string
			origin string
			want   int
		}{
			{"trusted origin passes", "https://app.example.com", http.StatusOK},
			{"another origin gets the default 403", "https://evil.example", http.StatusForbidden},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, "/", nil)
				r.Header.Set("Sec-Fetch-Site", "cross-site")
				r.Header.Set("Origin", tt.origin)
				app.ServeHTTP(w, r)
				if w.Code != tt.want {
					t.Errorf("status = %d, want %d", w.Code, tt.want)
				}
			})
		}
	})

	t.Run("ContractGuard: AllowMissingContentType only", func(t *testing.T) {
		app, g := contractGroup(t, middleware.ContractConfig{AllowMissingContentType: true})
		g.GET("/data", ok).SetMeta(middleware.MetaAPIVersion, []string{"2"})

		tests := []struct {
			name   string
			header string
			value  string
			want   int
		}{
			{"the default version header is read", "X-API-Version", "2", http.StatusOK},
			{"another header is not", "X-Version", "2", http.StatusBadRequest},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/g/data", nil)
				r.Header.Set(tt.header, tt.value)
				app.ServeHTTP(w, r)
				if w.Code != tt.want {
					t.Errorf("status = %d, want %d", w.Code, tt.want)
				}
			})
		}
	})

	t.Run("Rewrite: Rules only", func(t *testing.T) {
		app := mustNew(t)
		app.PreRouting(middleware.Rewrite(middleware.RewriteConfig{
			Rules: []middleware.RewriteRule{{From: "/old", To: "/new"}},
		}))
		app.GET("/new", ok)

		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/old", nil))
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (a nil Skipper skips no request)", w.Code)
		}
	})

	t.Run("Timeout: Timeout only", func(t *testing.T) {
		app := mustNew(t)
		app.GET("/", func(ctx *credo.Context) error {
			<-ctx.Context().Done()
			return ctx.Context().Err()
		}).Middleware(middleware.Timeout(middleware.TimeoutConfig{Timeout: 15 * time.Millisecond}))

		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 from the default ErrorHandler", w.Code)
		}
		if body := w.Body.String(); !strings.Contains(body, `"request_timeout"`) {
			t.Errorf("body = %s, want the code request_timeout", body)
		}
	})
}

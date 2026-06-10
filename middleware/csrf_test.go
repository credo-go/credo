package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

// newCSRFApp wires a GET + POST route behind the CSRF middleware.
func newCSRFApp(t *testing.T, cfg ...middleware.CSRFConfig) *credo.App {
	t.Helper()
	app := mustNew(t)
	app.GlobalMiddleware(middleware.CSRF(cfg...))
	ok := func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "ok")
	}
	app.GET("/form", ok)
	app.POST("/form", ok)
	app.POST("/webhooks/github", ok)
	return app
}

func TestCSRF_CrossSiteUnsafeBlocked(t *testing.T) {
	app := newCSRFApp(t)

	tests := []struct {
		name      string
		method    string
		fetchSite string
		want      int
	}{
		{"cross-site POST blocked", http.MethodPost, "cross-site", http.StatusForbidden},
		{"same-site POST blocked (subdomains are cross-origin)", http.MethodPost, "same-site", http.StatusForbidden},
		{"same-origin POST allowed", http.MethodPost, "same-origin", http.StatusOK},
		{"none POST allowed (direct navigation)", http.MethodPost, "none", http.StatusOK},
		{"cross-site GET allowed (safe method)", http.MethodGet, "cross-site", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(tt.method, "/form", nil)
			r.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			app.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestCSRF_NonBrowserRequestAllowed(t *testing.T) {
	app := newCSRFApp(t)

	// No Sec-Fetch-Site, no Origin: curl, server-to-server, mobile SDKs.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/form", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestCSRF_OriginFallback_OldBrowsers(t *testing.T) {
	app := newCSRFApp(t)

	t.Run("mismatched Origin blocked", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "http://example.com/form", nil)
		r.Header.Set("Origin", "https://evil.example")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("Origin matching Host allowed", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "http://example.com/form", nil)
		r.Header.Set("Origin", "http://example.com")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})
}

func TestCSRF_TrustedOrigin(t *testing.T) {
	app := newCSRFApp(t, middleware.CSRFConfig{
		TrustedOrigins: []string{"https://app.example.com"},
	})

	t.Run("trusted origin allowed", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/form", nil)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Origin", "https://app.example.com")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})

	t.Run("other origin still blocked", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/form", nil)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Origin", "https://evil.example")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})
}

func TestCSRF_InsecureBypassPattern(t *testing.T) {
	app := newCSRFApp(t, middleware.CSRFConfig{
		InsecureBypassPatterns: []string{"/webhooks/"},
	})

	t.Run("bypassed path allowed", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/webhooks/github", nil)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})

	t.Run("non-bypassed path still blocked", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/form", nil)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})
}

func TestCSRF_DefaultDeny_RFC7807(t *testing.T) {
	app := newCSRFApp(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/form", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	// The detector's reason stays internal — never in the response body.
	if body := w.Body.String(); strings.Contains(body, "Sec-Fetch-Site") {
		t.Errorf("response body leaks rejection reason: %s", body)
	}
}

func TestCSRF_CustomErrorHandler(t *testing.T) {
	app := newCSRFApp(t, middleware.CSRFConfig{
		ErrorHandler: func(_ *credo.Context, err error) error {
			if err == nil {
				t.Error("ErrorHandler called with nil err")
			}
			return credo.NewHTTPError(http.StatusTeapot, "blocked by custom handler")
		},
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/form", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	app.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTeapot)
	}
}

func TestCSRF_Skipper(t *testing.T) {
	app := newCSRFApp(t, middleware.CSRFConfig{
		Skipper: func(*credo.Context) bool { return true },
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/form", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (skipper must bypass the check)", w.Code, http.StatusOK)
	}
}

func TestCSRF_InvalidTrustedOriginPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("CSRF with scheme-less trusted origin did not panic")
		}
	}()
	middleware.CSRF(middleware.CSRFConfig{
		TrustedOrigins: []string{"app.example.com"}, // missing scheme
	})
}

func TestCSRF_InvalidBypassPatternPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("CSRF with invalid bypass pattern did not panic")
		}
	}()
	middleware.CSRF(middleware.CSRFConfig{
		InsecureBypassPatterns: []string{"/x/{bad"}, // malformed wildcard
	})
}

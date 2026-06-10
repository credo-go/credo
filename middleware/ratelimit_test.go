package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
	"github.com/sethvargo/go-limiter/memorystore"
)

func TestRateLimit_DeniesAfterLimitReached(t *testing.T) {
	store, err := memorystore.New(&memorystore.Config{
		Tokens:   1,
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close(t.Context())
	})

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.RateLimit(middleware.RateLimitConfig{
		Store: store,
		KeyFunc: func(ctx *credo.Context) (string, error) {
			return "client-1", nil
		},
	}))

	first := httptest.NewRecorder()
	app.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}

	second := httptest.NewRecorder()
	app.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", second.Code)
	}

	if got := second.Header().Get("X-RateLimit-Limit"); got != "1" {
		t.Fatalf("X-RateLimit-Limit = %q, want 1", got)
	}
	if got := second.Header().Get("Retry-After"); got == "" {
		t.Fatal("Retry-After header is empty")
	}
}

func TestRateLimit_KeyFuncError(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.RateLimit(middleware.RateLimitConfig{
		KeyFunc: func(ctx *credo.Context) (string, error) {
			return "", http.ErrNoCookie
		},
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestRateLimit_CustomDeniedHandler(t *testing.T) {
	store, err := memorystore.New(&memorystore.Config{
		Tokens:   1,
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close(t.Context())
	})

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.RateLimit(middleware.RateLimitConfig{
		Store: store,
		KeyFunc: func(ctx *credo.Context) (string, error) {
			return "client-1", nil
		},
		DeniedHandler: func(ctx *credo.Context, limit, remaining uint64, reset time.Time) error {
			return ctx.Response().JSON(http.StatusTooManyRequests, map[string]any{
				"limit":     limit,
				"remaining": remaining,
			})
		},
	}))

	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", got)
	}
}

func TestRateLimit_DefaultMiddlewareConstructor(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.RateLimit())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.42:8080"
	app.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got == "" {
		t.Fatal("X-RateLimit-Limit header is empty")
	}
}

func TestRateLimit_DefaultKeyUsesRealIP(t *testing.T) {
	store, err := memorystore.New(&memorystore.Config{
		Tokens:   1,
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close(t.Context())
	})

	app := mustNew(t, credo.WithTrustedProxies("10.0.0.0/8"))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.RateLimit(middleware.RateLimitConfig{Store: store}))

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodGet, "/", nil)
	firstReq.RemoteAddr = "10.0.0.1:1234"
	firstReq.Header.Set("X-Forwarded-For", "203.0.113.10")
	app.ServeHTTP(first, firstReq)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodGet, "/", nil)
	secondReq.RemoteAddr = "10.0.0.1:1234"
	secondReq.Header.Set("X-Forwarded-For", "203.0.113.11")
	app.ServeHTTP(second, secondReq)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", second.Code)
	}

	third := httptest.NewRecorder()
	thirdReq := httptest.NewRequest(http.MethodGet, "/", nil)
	thirdReq.RemoteAddr = "10.0.0.1:1234"
	thirdReq.Header.Set("X-Forwarded-For", "203.0.113.10")
	app.ServeHTTP(third, thirdReq)
	if third.Code != http.StatusTooManyRequests {
		t.Fatalf("third status = %d, want 429", third.Code)
	}
}

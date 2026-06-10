package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func TestTimeout_DeadlineExceeded(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		<-ctx.Request().Context().Done()
		return ctx.Request().Context().Err()
	}).Middleware(middleware.Timeout(middleware.TimeoutConfig{Timeout: 15 * time.Millisecond}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestTimeout_NoTimeout(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(middleware.Timeout(middleware.TimeoutConfig{Timeout: 100 * time.Millisecond}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestTimeout_ContextRestoredAfterHandler(t *testing.T) {
	app := mustNew(t)

	var ctxCancelledAfterHandler bool
	outerMiddleware := func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			err := next(ctx)
			// After timeout middleware returns, the request context should
			// be the original (non-timeout) context, not a cancelled one.
			ctxCancelledAfterHandler = ctx.Request().Context().Err() != nil
			return err
		}
	}

	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(outerMiddleware, middleware.Timeout(middleware.TimeoutConfig{Timeout: 100 * time.Millisecond}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ctxCancelledAfterHandler {
		t.Fatal("request context should not be cancelled after timeout middleware returns")
	}
}

func TestTimeout_CustomErrorHandler(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		<-ctx.Request().Context().Done()
		return ctx.Request().Context().Err()
	}).Middleware(middleware.Timeout(middleware.TimeoutConfig{
		Timeout: 10 * time.Millisecond,
		ErrorHandler: func(ctx *credo.Context, err error) error {
			if errors.Is(err, context.DeadlineExceeded) {
				return credo.NewHTTPError(http.StatusGatewayTimeout, "gateway timeout")
			}
			return err
		},
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", w.Code)
	}
}

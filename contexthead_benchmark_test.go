package credo_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

type benchUser struct{ name string }

type benchSpanKey struct{}

var benchTenant = credo.NewKey[string]("bench.tenant")

// BenchmarkContextHead_NestedChain prices the request values over the head on
// a nested chain: a stdlib middleware adding a span through
// WrapStdMiddleware, then middleware.Timeout, then authentication setting the
// user, then a handler setting one more value it reads from the request, with an access-log result
// filter reading the user after the response.
func BenchmarkContextHead_NestedChain(b *testing.B) {
	app := mustNewBench(b)
	var sawUser int
	app.UseAccessLog(credo.AccessLogConfig{
		Logger: slog.New(slog.DiscardHandler),
		ResultFilter: func(ex *credo.Exchange) bool {
			if _, ok := ex.GetUser[*benchUser](); ok {
				sawUser++
			}
			return false
		},
	})
	span := credo.WrapStdMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), benchSpanKey{}, "span")))
		})
	})
	user := &benchUser{name: "alice"}
	auth := func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			c.SetUser(user)
			return next(c)
		}
	}
	app.GET("/x", func(c *credo.Context) error {
		c.Set(benchTenant, c.Request().Header.Get("X-Tenant"))
		return c.Response().NoContent(http.StatusNoContent)
	}).Middleware(span, middleware.Timeout(middleware.TimeoutConfig{Timeout: time.Minute}), auth)

	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("X-Tenant", "acme")
	benchExpect(b, app, r, http.StatusNoContent, "")
	w := newNoopResponseWriter()
	b.ReportAllocs()
	for b.Loop() {
		app.ServeHTTP(w, r)
	}
	b.ReportMetric(float64(sawUser)/float64(b.N+1), "user-seen/op")
}

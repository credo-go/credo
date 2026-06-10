package credo_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// noopResponseWriter discards all output. Avoids httptest.ResponseRecorder
// allocation noise in benchmarks.
type noopResponseWriter struct {
	h http.Header
}

func newNoopResponseWriter() *noopResponseWriter {
	return &noopResponseWriter{h: make(http.Header)}
}

func (w *noopResponseWriter) Header() http.Header         { return w.h }
func (w *noopResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *noopResponseWriter) WriteHeader(int)             {}

// benchMiddleware creates a minimal passthrough middleware that sets one header.
func benchMiddleware(name string) credo.Middleware {
	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set(name, "1")
			return next(ctx)
		}
	}
}

type benchPayload struct {
	Message string `json:"message"`
	Count   int    `json:"count"`
}

func mustNewBench(b *testing.B, opts ...credo.Option) *credo.App {
	b.Helper()
	app, err := credo.New(opts...)
	if err != nil {
		b.Fatal(err)
	}
	return app
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

// BenchmarkServeHTTP_Static measures the full request cycle for a static route:
// sync.Pool get/put, radix tree static match, text response write.
func BenchmarkServeHTTP_Static(b *testing.B) {
	app := mustNewBench(b)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "ok")
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkServeHTTP_Param measures the overhead of param extraction
// (map[string]string allocation in dispatch.go).
func BenchmarkServeHTTP_Param(b *testing.B) {
	app := mustNewBench(b)
	app.GET("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Request().RouteParam("id"))
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/users/42", nil)

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkServeHTTP_JSON measures JSON encoding overhead on the hot path.
func BenchmarkServeHTTP_JSON(b *testing.B) {
	app := mustNewBench(b)
	app.GET("/json", func(ctx *credo.Context) error {
		return ctx.Response().JSON(http.StatusOK, benchPayload{
			Message: "hello",
			Count:   42,
		})
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/json", nil)

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkServeHTTP_Middleware measures middleware chain overhead scaling.
func BenchmarkServeHTTP_Middleware(b *testing.B) {
	for _, n := range []int{0, 1, 5, 10} {
		b.Run(fmt.Sprintf("MW_%d", n), func(b *testing.B) {
			app := mustNewBench(b)

			mws := make([]credo.Middleware, n)
			for i := range n {
				mws[i] = benchMiddleware(fmt.Sprintf("X-Bench-%d", i))
			}
			if len(mws) > 0 {
				app.GlobalMiddleware(mws...)
			}

			app.GET("/", func(ctx *credo.Context) error {
				return ctx.Response().Text(http.StatusOK, "ok")
			})

			w := newNoopResponseWriter()
			r := httptest.NewRequest(http.MethodGet, "/", nil)

			b.ReportAllocs()
			for b.Loop() {
				clear(w.h)
				app.ServeHTTP(w, r)
			}
		})
	}
}

// BenchmarkServeHTTP_Parallel stresses sync.Pool contention under
// concurrent load (context pool + route context pool).
func BenchmarkServeHTTP_Parallel(b *testing.B) {
	app := mustNewBench(b)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "ok")
	})

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		w := newNoopResponseWriter()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for pb.Next() {
			clear(w.h)
			app.ServeHTTP(w, r)
		}
	})
}

// BenchmarkServeHTTP_ParallelJSON measures pool contention + JSON encoding
// under concurrent load.
func BenchmarkServeHTTP_ParallelJSON(b *testing.B) {
	app := mustNewBench(b)
	app.GET("/json", func(ctx *credo.Context) error {
		return ctx.Response().JSON(http.StatusOK, benchPayload{
			Message: "hello",
			Count:   42,
		})
	})

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		w := newNoopResponseWriter()
		r := httptest.NewRequest(http.MethodGet, "/json", nil)
		for pb.Next() {
			clear(w.h)
			app.ServeHTTP(w, r)
		}
	})
}

// BenchmarkServeHTTP_NotFound measures the 404 path through
// the error handling pipeline + ProblemDetails JSON encoding.
func BenchmarkServeHTTP_NotFound(b *testing.B) {
	app := mustNewBench(b)
	app.GET("/exists", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "ok")
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/notfound", nil)

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkServeHTTP_Text measures Text() response with io.WriteString
// optimization (avoids []byte conversion).
func BenchmarkServeHTTP_Text(b *testing.B) {
	app := mustNewBench(b)
	app.GET("/text", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "hello world benchmark")
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/text", nil)

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkServeHTTP_HTML measures HTML() response with io.WriteString
// optimization (avoids []byte conversion).
func BenchmarkServeHTTP_HTML(b *testing.B) {
	app := mustNewBench(b)
	app.GET("/html", func(ctx *credo.Context) error {
		return ctx.Response().HTML(http.StatusOK, "<h1>Hello</h1>")
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/html", nil)

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkServeHTTP_QueryParam measures single QueryParam() call with
// cached query parameter parsing.
func BenchmarkServeHTTP_QueryParam(b *testing.B) {
	app := mustNewBench(b)
	app.GET("/q", func(ctx *credo.Context) error {
		_ = ctx.Request().QueryParam("page")
		return ctx.Response().Text(http.StatusOK, "ok")
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/?page=1&sort=name&order=asc", nil)

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkServeHTTP_QueryParamMulti measures 3 QueryParam() calls on the
// same request, exercising the cached query parameter parsing (second and
// third calls reuse the parsed url.Values).
func BenchmarkServeHTTP_QueryParamMulti(b *testing.B) {
	app := mustNewBench(b)
	app.GET("/q", func(ctx *credo.Context) error {
		_ = ctx.Request().QueryParam("page")
		_ = ctx.Request().QueryParam("sort")
		_ = ctx.Request().QueryParam("order")
		return ctx.Response().Text(http.StatusOK, "ok")
	})

	w := newNoopResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/?page=1&sort=name&order=asc", nil)

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

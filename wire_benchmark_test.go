package credo

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo/validation"
)

// wireBenchmarkWriter removes body-buffer allocation noise while preserving
// the Header operations performed by the response path.
type wireBenchmarkWriter struct {
	header http.Header
}

func newWireBenchmarkWriter() *wireBenchmarkWriter {
	return &wireBenchmarkWriter{header: make(http.Header)}
}

func (w *wireBenchmarkWriter) Header() http.Header         { return w.header }
func (w *wireBenchmarkWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *wireBenchmarkWriter) WriteString(s string) (int, error) {
	return len(s), nil
}
func (w *wireBenchmarkWriter) WriteHeader(int) {}
func (w *wireBenchmarkWriter) ReadFrom(src io.Reader) (int64, error) {
	return io.Copy(io.Discard, src)
}

// wireBenchmarkFeatures selects the HTTP features a benchmark app installs.
type wireBenchmarkFeatures struct {
	requestID bool
	accessLog bool
	accessCfg AccessLogConfig
}

func newWireBenchmarkApp(b *testing.B, coreOnly bool) *App {
	b.Helper()
	if coreOnly {
		return newWireBenchmarkAppWithOptions(b)
	}
	app := newWireBenchmarkAppWithOptions(b)
	app.UseRequestID()
	app.UseAccessLog()
	return app
}

func installWireBenchmarkFeatures(app *App, f wireBenchmarkFeatures) {
	if f.requestID {
		app.UseRequestID()
	}
	if f.accessLog {
		app.UseAccessLog(f.accessCfg)
	}
}

func newWireBenchmarkAppWithOptions(b *testing.B, opts ...Option) *App {
	b.Helper()
	opts = append([]Option{
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}, opts...)
	app, err := New(opts...)
	if err != nil {
		b.Fatal(err)
	}
	return app
}

func runWireBenchmark(b *testing.B, app *App, method, path string) {
	b.Helper()
	r := httptest.NewRequest(method, path, nil)
	runWireBenchmarkRequest(b, app, r)
}

func runWireBenchmarkRequest(b *testing.B, app *App, r *http.Request) {
	b.Helper()
	w := newWireBenchmarkWriter()
	b.ReportAllocs()
	for b.Loop() {
		clear(w.header)
		app.ServeHTTP(w, r)
	}
}

// BenchmarkWireObservability decomposes the default request ID and access-log
// costs. Incoming IDs are measured separately because generating a random ID
// is intentionally more expensive than validating a trusted upstream value.
func BenchmarkWireObservability(b *testing.B) {
	tests := []struct {
		name      string
		opts      []Option
		features  wireBenchmarkFeatures
		requestID string
	}{
		{name: "Core"},
		{name: "RequestID/Generated", features: wireBenchmarkFeatures{requestID: true}},
		{name: "RequestID/Incoming", features: wireBenchmarkFeatures{requestID: true}, requestID: "upstream-request-id"},
		{name: "AccessLog/Enabled", features: wireBenchmarkFeatures{accessLog: true}},
		{
			name:     "AccessLog/MinLevelFiltered",
			features: wireBenchmarkFeatures{accessLog: true, accessCfg: AccessLogConfig{MinLevel: slog.LevelError}},
		},
		{
			name: "AccessLog/HandlerFiltered",
			opts: []Option{
				WithLogger(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))),
			},
			features: wireBenchmarkFeatures{accessLog: true},
		},
		{name: "Both/Generated", features: wireBenchmarkFeatures{requestID: true, accessLog: true}},
		{name: "Both/Incoming", features: wireBenchmarkFeatures{requestID: true, accessLog: true}, requestID: "upstream-request-id"},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			app := newWireBenchmarkAppWithOptions(b, tt.opts...)
			installWireBenchmarkFeatures(app, tt.features)
			app.GET("/bench", func(ctx *Context) error {
				return ctx.Response().NoContent(http.StatusNoContent)
			})
			r := httptest.NewRequest(http.MethodGet, "/bench", nil)
			if tt.requestID != "" {
				r.Header.Set("X-Request-Id", tt.requestID)
			}
			runWireBenchmarkRequest(b, app, r)
		})
	}
}

// BenchmarkWireRequestIDComponents isolates the two mutable operations in the
// request ID tier. It helps distinguish random-ID generation from context-store
// boxing and response-header publication.
func BenchmarkWireRequestIDComponents(b *testing.B) {
	tests := []struct {
		name           string
		withIncomingID bool
		handler        Handler
	}{
		{
			name: "Core",
			handler: func(ctx *Context) error {
				return ctx.Response().NoContent(http.StatusNoContent)
			},
		},
		{
			name:           "ContextStore",
			withIncomingID: true,
			handler: func(ctx *Context) error {
				ctx.Set(requestIDKey, ctx.Request().Header.Get("X-Request-Id"))
				return ctx.Response().NoContent(http.StatusNoContent)
			},
		},
		{
			name:           "ResponseHeader",
			withIncomingID: true,
			handler: func(ctx *Context) error {
				ctx.Response().Header().Set("X-Request-Id", ctx.Request().Header.Get("X-Request-Id"))
				return ctx.Response().NoContent(http.StatusNoContent)
			},
		},
		{
			name:           "StoreAndHeader",
			withIncomingID: true,
			handler: func(ctx *Context) error {
				id := ctx.Request().Header.Get("X-Request-Id")
				ctx.Set(requestIDKey, id)
				ctx.Response().Header().Set("X-Request-Id", id)
				return ctx.Response().NoContent(http.StatusNoContent)
			},
		},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			app := newWireBenchmarkApp(b, true)
			app.GET("/bench", tt.handler)
			r := httptest.NewRequest(http.MethodGet, "/bench", nil)
			if tt.withIncomingID {
				r.Header.Set("X-Request-Id", "upstream-request-id")
			}
			runWireBenchmarkRequest(b, app, r)
		})
	}
}

type wireBenchmarkPayload struct {
	Message string `json:"message"`
	Count   int    `json:"count"`
}

type wireBenchmarkSuccessEnvelope struct {
	Success bool `json:"success"`
	Data    any  `json:"data"`
}

const wireBenchmarkStreamSize = 4 << 10

var wireBenchmarkStreamBody = strings.Repeat("x", wireBenchmarkStreamSize)

// wireBenchmarkReader deliberately implements io.Reader but not io.WriterTo.
// It exposes whether Response.Stream can reach the destination's io.ReaderFrom
// fast path or falls back to io.Copy's generic buffer.
type wireBenchmarkReader struct {
	remaining int
}

func (r *wireBenchmarkReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	r.remaining -= n
	return n, nil
}

// BenchmarkWireSuccess separates the request-ID + access-log feature set from
// the response core. The Core cases are the default profile — recovery and
// centralized error handling only; the Features cases install UseRequestID
// and UseAccessLog.
func BenchmarkWireSuccess(b *testing.B) {
	for _, coreOnly := range []bool{false, true} {
		mode := "Features"
		if coreOnly {
			mode = "Core"
		}

		b.Run(mode+"/JSON", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			app.GET("/bench", func(ctx *Context) error {
				return ctx.Response().JSON(http.StatusOK, wireBenchmarkPayload{Message: "hello", Count: 42})
			})
			runWireBenchmark(b, app, http.MethodGet, "/bench")
		})

		b.Run(mode+"/Render", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			app.GET("/bench", func(ctx *Context) error {
				return ctx.Render(http.StatusOK, wireBenchmarkPayload{Message: "hello", Count: 42})
			})
			runWireBenchmark(b, app, http.MethodGet, "/bench")
		})

		b.Run(mode+"/RenderWithRenderer", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			app.UseSuccessRenderer(func(_ *Context, info RenderInfo) any {
				return wireBenchmarkSuccessEnvelope{Success: true, Data: info.Data}
			})
			app.GET("/bench", func(ctx *Context) error {
				return ctx.Render(http.StatusOK, wireBenchmarkPayload{Message: "hello", Count: 42})
			})
			runWireBenchmark(b, app, http.MethodGet, "/bench")
		})

		b.Run(mode+"/RenderWithOptions", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			app.UseSuccessRenderer(func(_ *Context, info RenderInfo) any {
				return wireBenchmarkSuccessEnvelope{Success: true, Data: info.Data}
			})
			app.GET("/bench", func(ctx *Context) error {
				return ctx.Render(
					http.StatusOK,
					wireBenchmarkPayload{Message: "hello", Count: 42},
					RenderMessageKey("created"),
					RenderMeta(map[string]any{"page": 1}),
				)
			})
			runWireBenchmark(b, app, http.MethodGet, "/bench")
		})

		b.Run(mode+"/Text", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			app.GET("/bench", func(ctx *Context) error {
				return ctx.Response().Text(http.StatusOK, "hello")
			})
			runWireBenchmark(b, app, http.MethodGet, "/bench")
		})

		b.Run(mode+"/StreamWriterTo", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			app.GET("/bench", func(ctx *Context) error {
				return ctx.Response().Stream(
					http.StatusOK,
					"application/octet-stream",
					strings.NewReader(wireBenchmarkStreamBody),
				)
			})
			runWireBenchmark(b, app, http.MethodGet, "/bench")
		})

		b.Run(mode+"/StreamReaderOnly", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			app.GET("/bench", func(ctx *Context) error {
				reader := wireBenchmarkReader{remaining: wireBenchmarkStreamSize}
				return ctx.Response().Stream(http.StatusOK, "application/octet-stream", &reader)
			})
			runWireBenchmark(b, app, http.MethodGet, "/bench")
		})

		b.Run(mode+"/NoContent", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			app.GET("/bench", func(ctx *Context) error {
				return ctx.Response().NoContent(http.StatusNoContent)
			})
			runWireBenchmark(b, app, http.MethodGet, "/bench")
		})
	}
}

// BenchmarkWireError covers each error classifier branch and the first-party
// RFC 9457 renderer, again with and without the request-ID + access-log
// feature set.
func BenchmarkWireError(b *testing.B) {
	for _, coreOnly := range []bool{false, true} {
		mode := "Features"
		if coreOnly {
			mode = "Core"
		}

		b.Run(mode+"/Sentinel404", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			app.GET("/bench", func(*Context) error { return ErrNotFound })
			runWireBenchmark(b, app, http.MethodGet, "/bench")
		})

		b.Run(mode+"/Constructed404", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			app.GET("/bench", func(*Context) error {
				return NewHTTPError(http.StatusNotFound, "user_not_found")
			})
			runWireBenchmark(b, app, http.MethodGet, "/bench")
		})

		b.Run(mode+"/Generic500", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			errBoom := errors.New("boom")
			app.GET("/bench", func(*Context) error { return errBoom })
			runWireBenchmark(b, app, http.MethodGet, "/bench")
		})

		b.Run(mode+"/Validation", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			errValidation := validation.Errors{
				{Field: "email", Code: "required", Message: "is required"},
				{Field: "name", Code: "length", Message: "has invalid length", Params: map[string]any{"min": 2, "max": 100}},
			}
			app.POST("/bench", func(*Context) error { return errValidation })
			runWireBenchmark(b, app, http.MethodPost, "/bench")
		})

		b.Run(mode+"/Bind", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			errBind := &BindError{Reason: BindReasonSyntax, Offset: 12}
			app.POST("/bench", func(*Context) error { return errBind })
			runWireBenchmark(b, app, http.MethodPost, "/bench")
		})

		b.Run(mode+"/RFC9457", func(b *testing.B) {
			app := newWireBenchmarkApp(b, coreOnly)
			app.UseErrorRenderer(RFC9457ErrorRenderer())
			app.GET("/bench", func(*Context) error { return ErrNotFound })
			runWireBenchmark(b, app, http.MethodGet, "/bench")
		})
	}
}

var wireBenchmarkOptionsSink jsonv2.Options

// BenchmarkWireJSONOptions isolates option assembly from JSON encoding. It
// makes accidental per-error option allocation visible in a stable microbench.
func BenchmarkWireJSONOptions(b *testing.B) {
	app := newWireBenchmarkApp(b, true)
	cached := app.errorJSONOptions()

	b.Run("SuccessOptions", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			wireBenchmarkOptionsSink = app.jsonOptions()
		}
	})

	b.Run("ErrorOptions", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			wireBenchmarkOptionsSink = app.errorJSONOptions()
		}
	})

	b.Run("PrecomputedErrorOptions", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			wireBenchmarkOptionsSink = cached
		}
	})
}

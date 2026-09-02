package credo_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func TestBuiltinAccessLog_EmitsRoutePattern(t *testing.T) {
	var buf bytes.Buffer
	app, err := credo.New(credo.WithAccessLogLogger(slog.New(slog.NewJSONHandler(&buf, nil))))
	if err != nil {
		t.Fatalf("credo.New: %v", err)
	}
	app.GET("/items/{id}", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/items/42", nil))
	if got := buf.String(); !strings.Contains(got, `"route":"/items/{id}"`) || !strings.Contains(got, `"path":"/items/42"`) {
		t.Fatalf("matched request log = %s, want route pattern and concrete path", got)
	}

	buf.Reset()
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if got := buf.String(); strings.Contains(got, `"route":`) {
		t.Fatalf("unmatched request log = %s, want no route attribute", got)
	}
}

func TestAccessLogMiddleware_EmitsRoutePattern(t *testing.T) {
	var buf bytes.Buffer
	app, err := credo.New(credo.WithoutAccessLog())
	if err != nil {
		t.Fatalf("credo.New: %v", err)
	}
	var seen credo.AccessLogEntry
	app.GlobalMiddleware(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: slog.New(slog.NewJSONHandler(&buf, nil)),
		ResultFilter: func(_ *credo.Context, entry credo.AccessLogEntry) bool {
			seen = entry
			return true
		},
	}))
	app.GET("/items/{id}", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/items/42", nil))
	if seen.Route != "/items/{id}" {
		t.Fatalf("AccessLogEntry.Route = %q, want /items/{id}", seen.Route)
	}
	if got := buf.String(); !strings.Contains(got, `"route":"/items/{id}"`) {
		t.Fatalf("middleware log = %s, want route attribute", got)
	}
}

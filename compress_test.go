package credo_test

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

func gunzipBody(t *testing.T, r io.Reader) string {
	t.Helper()
	gr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("new gzip reader: %v", err)
	}
	defer gr.Close()
	b, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	return string(b)
}

func TestCompress_Gzip(t *testing.T) {
	body := "hello compressed world"

	app := mustNew(t)
	app.UseCompress()
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, body)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	app.ServeHTTP(w, r)

	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	if got := gunzipBody(t, w.Body); got != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

func TestCompress_NoAcceptEncoding(t *testing.T) {
	body := "plain response"

	app := mustNew(t)
	app.UseCompress()
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, body)
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if got := w.Body.String(); got != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

func TestCompress_UnsupportedContentType(t *testing.T) {
	body := []byte{0x00, 0x01, 0x02, 0x03}

	app := mustNew(t)
	app.UseCompress()
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Blob(http.StatusOK, "application/octet-stream", body)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	app.ServeHTTP(w, r)

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if got := w.Body.Bytes(); string(got) != string(body) {
		t.Fatalf("body = %v, want %v", got, body)
	}
}

func TestCompress_ErrorEnvelopeIsCompressed(t *testing.T) {
	// The writer stays installed through centralized error rendering, so a
	// returned error, a 404 and a panic response are compressed like handler
	// output.
	tests := []struct {
		name   string
		path   string
		status int
		code   string
	}{
		{"returned error", "/err", http.StatusBadRequest, "bad_request"},
		{"not found", "/missing", http.StatusNotFound, "not_found"},
		{"panic", "/panic", http.StatusInternalServerError, "internal_server_error"},
	}

	app := mustNew(t)
	app.UseCompress()
	app.GET("/err", func(*credo.Context) error { return credo.ErrBadRequest })
	app.GET("/panic", func(*credo.Context) error { panic("boom") })

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, tt.path, nil)
			r.Header.Set("Accept-Encoding", "gzip")
			app.ServeHTTP(w, r)

			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if got := w.Header().Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip", got)
			}
			var body credo.ErrorResponse
			if err := json.Unmarshal([]byte(gunzipBody(t, w.Body)), &body); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			if body.Error.Code != tt.code {
				t.Fatalf("error.code = %q, want %q", body.Error.Code, tt.code)
			}
		})
	}
}

func TestCompress_HeadAndBodilessKeepBehavior(t *testing.T) {
	app := mustNew(t)
	app.UseCompress()
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "hello")
	})
	app.GET("/empty", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusNoContent)
	})

	// HEAD is answered by the GET twin; the transport drops the body, the
	// negotiated encoding headers stay.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodHead, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("HEAD Content-Encoding = %q, want gzip", got)
	}

	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/empty", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	app.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("204 status/body = %d/%d, want 204/0", w.Code, w.Body.Len())
	}
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("204 Content-Encoding = %q, want empty", got)
	}
}

func TestCompress_SkipperSeesOriginalRequest(t *testing.T) {
	var seenPath string
	app := mustNew(t)
	app.UseCompress(credo.CompressConfig{
		Skipper: func(ctx *credo.Context) bool {
			seenPath = ctx.Request().URL.Path
			return ctx.Route() != nil // route is never matched yet
		},
	})
	app.GET("/legacy", func(ctx *credo.Context) error {
		return ctx.Rewrite("/new")
	})
	app.GET("/new", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "rewritten")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/legacy", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	app.ServeHTTP(w, r)

	if seenPath != "/legacy" {
		t.Fatalf("skipper saw path %q, want /legacy (original request, before routing)", seenPath)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip (selected once, rewrite does not re-evaluate)", got)
	}
	if got := gunzipBody(t, w.Body); got != "rewritten" {
		t.Fatalf("body = %q, want rewritten", got)
	}
}

func TestCompress_AccessLogCountsCompressedBytes(t *testing.T) {
	logger, buf := newTestLogger(t)
	body := strings.Repeat("compressible text ", 200)

	app := mustNew(t, credo.WithLogger(logger))
	app.UseAccessLog()
	app.UseCompress()
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, body)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse log: %v\nraw: %s", err, buf.String())
	}
	logged, _ := entry["bytes"].(float64)
	if int(logged) != w.Body.Len() {
		t.Fatalf("logged bytes = %d, want compressed transport bytes %d", int(logged), w.Body.Len())
	}
	if w.Body.Len() >= len(body) {
		t.Fatalf("compressed size %d not smaller than %d", w.Body.Len(), len(body))
	}
}

func TestCompress_AccessLogCountsUncompressedPassthrough(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger))
	app.UseAccessLog()
	app.UseCompress()
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Blob(http.StatusOK, "application/octet-stream", []byte("raw-bytes"))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse log: %v\nraw: %s", err, buf.String())
	}
	if logged, _ := entry["bytes"].(float64); int(logged) != len("raw-bytes") {
		t.Fatalf("logged bytes = %d, want %d", int(logged), len("raw-bytes"))
	}
}

func TestCompress_CustomTypesAndLevel(t *testing.T) {
	app := mustNew(t)
	app.UseCompress(credo.CompressConfig{Level: 1, Types: []string{"application/x-custom"}})
	app.GET("/custom", func(ctx *credo.Context) error {
		return ctx.Response().Blob(http.StatusOK, "application/x-custom", []byte(strings.Repeat("x", 512)))
	})
	app.GET("/text", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, strings.Repeat("x", 512))
	})

	for path, want := range map[string]string{"/custom": "gzip", "/text": ""} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Accept-Encoding", "gzip")
		app.ServeHTTP(w, r)
		if got := w.Header().Get("Content-Encoding"); got != want {
			t.Fatalf("%s: Content-Encoding = %q, want %q", path, got, want)
		}
	}
}

func TestUseCompress_Misuse(t *testing.T) {
	expectPanic := func(t *testing.T, want string, fn func()) {
		t.Helper()
		defer func() {
			r := recover()
			if r == nil || !strings.Contains(fmt.Sprint(r), want) {
				t.Fatalf("panic = %v, want containing %q", r, want)
			}
		}()
		fn()
	}

	t.Run("twice", func(t *testing.T) {
		app := mustNew(t)
		app.UseCompress()
		expectPanic(t, "called twice", func() { app.UseCompress() })
	})
	t.Run("two configs", func(t *testing.T) {
		app := mustNew(t)
		expectPanic(t, "at most one config", func() {
			app.UseCompress(credo.CompressConfig{}, credo.CompressConfig{})
		})
	})
	t.Run("level out of range", func(t *testing.T) {
		app := mustNew(t)
		expectPanic(t, "Level 10", func() { app.UseCompress(credo.CompressConfig{Level: 10}) })
	})
	t.Run("after prepare", func(t *testing.T) {
		app := mustNew(t)
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		expectPanic(t, "App.UseCompress", func() { app.UseCompress() })
	})
}

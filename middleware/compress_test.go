package middleware_test

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func TestCompress_Gzip(t *testing.T) {
	body := "hello compressed world"

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, body)
	}).Middleware(middleware.Compress())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	app.ServeHTTP(w, r)

	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}

	gr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("new gzip reader: %v", err)
	}
	defer gr.Close()

	decompressed, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}

	if string(decompressed) != body {
		t.Fatalf("body = %q, want %q", string(decompressed), body)
	}
}

func TestCompress_NoAcceptEncoding(t *testing.T) {
	body := "plain response"

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, body)
	}).Middleware(middleware.Compress())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	app.ServeHTTP(w, r)

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
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Blob(http.StatusOK, "application/octet-stream", body)
	}).Middleware(middleware.Compress())

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

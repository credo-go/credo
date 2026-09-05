package credo_test

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

func gzipBytes(t *testing.T, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zlibBytes(t *testing.T, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func rawDeflateBytes(t *testing.T, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newDecompressApp binds {"name": ...} from the body and echoes the name.
func newDecompressApp(t *testing.T, cfg ...credo.DecompressConfig) *credo.App {
	t.Helper()
	app := mustNew(t)
	app.UseDecompress(cfg...)
	app.POST("/items", func(ctx *credo.Context) error {
		var in struct {
			Name string `json:"name"`
		}
		if err := ctx.Request().BindBody(&in); err != nil {
			return err
		}
		return ctx.Response().Text(http.StatusOK, in.Name)
	})
	return app
}

func postEncoded(app *credo.App, body []byte, coding string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/items", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if coding != "" {
		r.Header.Set("Content-Encoding", coding)
	}
	app.ServeHTTP(w, r)
	return w
}

func TestDecompress_Codings(t *testing.T) {
	payload := `{"name":"Bob"}`
	tests := []struct {
		name   string
		coding string
		body   func(*testing.T, string) []byte
	}{
		{"gzip", "gzip", gzipBytes},
		{"x-gzip", "x-gzip", gzipBytes},
		{"gzip mixed case", "GZip", gzipBytes},
		{"deflate zlib", "deflate", zlibBytes},
		{"deflate raw", "deflate", rawDeflateBytes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newDecompressApp(t)
			w := postEncoded(app, tt.body(t, payload), tt.coding)
			if w.Code != http.StatusOK || w.Body.String() != "Bob" {
				t.Fatalf("status = %d body = %q, want 200 Bob", w.Code, w.Body.String())
			}
		})
	}
}

func TestDecompress_PassThrough(t *testing.T) {
	app := newDecompressApp(t)
	for _, coding := range []string{"", "identity", "identity, identity"} {
		w := postEncoded(app, []byte(`{"name":"Bob"}`), coding)
		if w.Code != http.StatusOK || w.Body.String() != "Bob" {
			t.Fatalf("coding %q: status = %d body = %q, want 200 Bob", coding, w.Code, w.Body.String())
		}
	}
}

func TestDecompress_OffByDefault(t *testing.T) {
	app := mustNew(t)
	app.POST("/items", func(ctx *credo.Context) error {
		var in struct {
			Name string `json:"name"`
		}
		return ctx.Request().BindBody(&in)
	})
	w := postEncoded(app, gzipBytes(t, `{"name":"Bob"}`), "gzip")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d body = %s, want 415 without UseDecompress", w.Code, w.Body.String())
	}
}

func TestDecompress_UnsupportedCoding(t *testing.T) {
	app := newDecompressApp(t)
	for _, coding := range []string{"br", "zstd", "gzip, br"} {
		w := postEncoded(app, gzipBytes(t, `{"name":"Bob"}`), coding)
		if w.Code != http.StatusUnsupportedMediaType || !strings.Contains(w.Body.String(), credo.CodeUnsupportedContentEncoding) {
			t.Fatalf("coding %q: status = %d body = %s, want 415 %s", coding, w.Code, w.Body.String(), credo.CodeUnsupportedContentEncoding)
		}
	}
}

func TestDecompress_RejectsBeforeUserMiddleware(t *testing.T) {
	var middlewareRan bool
	app := newDecompressApp(t)
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			middlewareRan = true
			return next(ctx)
		}
	})
	w := postEncoded(app, []byte("zzz"), "br")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", w.Code)
	}
	if middlewareRan {
		t.Fatal("global middleware ran for a request rejected by decompression")
	}
}

func TestDecompress_GlobalMiddlewareSeesDecodedBody(t *testing.T) {
	var seen string
	app := newDecompressApp(t)
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			req := ctx.Request()
			if got := req.Header.Get("Content-Encoding"); got != "" {
				t.Errorf("Content-Encoding reached global middleware: %q", got)
			}
			if req.ContentLength != -1 {
				t.Errorf("ContentLength = %d, want -1 (unknown) after decompression", req.ContentLength)
			}
			b, err := io.ReadAll(req.Body)
			if err != nil {
				return err
			}
			seen = string(b)
			req.Body = io.NopCloser(bytes.NewReader(b))
			return next(ctx)
		}
	})
	w := postEncoded(app, gzipBytes(t, `{"name":"Bob"}`), "gzip")
	if w.Code != http.StatusOK || w.Body.String() != "Bob" {
		t.Fatalf("status = %d body = %q, want 200 Bob", w.Code, w.Body.String())
	}
	if seen != `{"name":"Bob"}` {
		t.Fatalf("global middleware read %q, want the decoded body", seen)
	}
}

func TestDecompress_MalformedStream(t *testing.T) {
	app := newDecompressApp(t)

	// Corrupt gzip header fails when the stream is opened.
	w := postEncoded(app, []byte("definitely not gzip"), "gzip")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"bind_failed"`) || !strings.Contains(w.Body.String(), `"syntax"`) {
		t.Fatalf("corrupt header: status = %d body = %s, want 400 bind_failed/syntax", w.Code, w.Body.String())
	}

	// A truncated member fails during decode and still maps to bind_failed.
	full := gzipBytes(t, `{"name":"Bob","padding":"`+strings.Repeat("x", 4096)+`"}`)
	w = postEncoded(app, full[:len(full)/2], "gzip")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"bind_failed"`) {
		t.Fatalf("truncated: status = %d body = %s, want 400 bind_failed", w.Code, w.Body.String())
	}
}

func TestDecompress_BombBoundedByMaxBytes(t *testing.T) {
	// 1 MiB of zeros compresses to about a kilobyte; the decompressed limit
	// must stop it, not the wire size.
	inflated := `{"name":"` + strings.Repeat("0", 1<<20) + `"}`
	app := newDecompressApp(t, credo.DecompressConfig{MaxBytes: 1024})
	w := postEncoded(app, gzipBytes(t, inflated), "gzip")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body = %s, want 413", w.Code, w.Body.String())
	}
}

func TestDecompress_EmptyBodyPassesThrough(t *testing.T) {
	app := mustNew(t)
	app.UseDecompress()
	app.POST("/ping", func(ctx *credo.Context) error {
		if got := ctx.Request().Header.Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding reached the handler: %q", got)
		}
		return ctx.Response().NoContent(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/ping", http.NoBody)
	r.Header.Set("Content-Encoding", "gzip")
	app.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
}

func TestDecompress_Skipper(t *testing.T) {
	app := newDecompressApp(t, credo.DecompressConfig{
		Skipper: func(*credo.Context) bool { return true },
	})
	// Skipped: the compressed body reaches BindBody, which rejects the coding.
	w := postEncoded(app, gzipBytes(t, `{"name":"Bob"}`), "gzip")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d body = %s, want 415 from BindBody", w.Code, w.Body.String())
	}
}

func TestUseDecompress_Misuse(t *testing.T) {
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

	t.Run("negative MaxBytes", func(t *testing.T) {
		app := mustNew(t)
		expectPanic(t, "MaxBytes must be >= 0", func() {
			app.UseDecompress(credo.DecompressConfig{MaxBytes: -1})
		})
	})
	t.Run("twice", func(t *testing.T) {
		app := mustNew(t)
		app.UseDecompress()
		expectPanic(t, "called twice", func() { app.UseDecompress() })
	})
	t.Run("after shutdown", func(t *testing.T) {
		app := mustNew(t)
		if err := app.Shutdown(t.Context()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		expectPanic(t, "App.UseDecompress", func() { app.UseDecompress() })
	})
}

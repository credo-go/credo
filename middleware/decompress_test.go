package middleware_test

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
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
func newDecompressApp(t *testing.T, cfg ...middleware.DecompressConfig) *credo.App {
	t.Helper()
	app := mustNew(t)
	app.POST("/items", func(ctx *credo.Context) error {
		var in struct {
			Name string `json:"name"`
		}
		if err := ctx.Request().BindBody(&in); err != nil {
			return err
		}
		return ctx.Response().Text(http.StatusOK, in.Name)
	}).Middleware(middleware.Decompress(cfg...))
	return app
}

func postEncoded(app *credo.App, body []byte, coding string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/items", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if coding != "" {
		r.Header.Set("Content-Encoding", coding)
	}
	return runServe(app, w, r)
}

func runServe(app *credo.App, w *httptest.ResponseRecorder, r *http.Request) *httptest.ResponseRecorder {
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

func TestDecompress_UnsupportedCoding(t *testing.T) {
	app := newDecompressApp(t)
	for _, coding := range []string{"br", "zstd", "gzip, br"} {
		w := postEncoded(app, gzipBytes(t, `{"name":"Bob"}`), coding)
		if w.Code != http.StatusUnsupportedMediaType || !strings.Contains(w.Body.String(), credo.CodeUnsupportedContentEncoding) {
			t.Fatalf("coding %q: status = %d body = %s, want 415 %s", coding, w.Code, w.Body.String(), credo.CodeUnsupportedContentEncoding)
		}
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
	app := newDecompressApp(t, middleware.DecompressConfig{MaxBytes: 1024})
	w := postEncoded(app, gzipBytes(t, inflated), "gzip")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body = %s, want 413", w.Code, w.Body.String())
	}
}

func TestDecompress_EmptyBodyPassesThrough(t *testing.T) {
	app := mustNew(t)
	app.POST("/ping", func(ctx *credo.Context) error {
		if got := ctx.Request().Header.Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding reached the handler: %q", got)
		}
		return ctx.Response().NoContent(http.StatusNoContent)
	}).Middleware(middleware.Decompress())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/ping", http.NoBody)
	r.Header.Set("Content-Encoding", "gzip")
	app.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
}

func TestDecompress_Skipper(t *testing.T) {
	app := newDecompressApp(t, middleware.DecompressConfig{
		Skipper: func(*credo.Context) bool { return true },
	})
	// Skipped: the compressed body reaches BindBody, which rejects the coding.
	w := postEncoded(app, gzipBytes(t, `{"name":"Bob"}`), "gzip")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d body = %s, want 415 from BindBody", w.Code, w.Body.String())
	}
}

func TestDecompress_NegativeMaxBytesPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for negative MaxBytes")
		}
	}()
	middleware.Decompress(middleware.DecompressConfig{MaxBytes: -1})
}

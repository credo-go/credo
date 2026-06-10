package middleware

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

type countingResponseWriter struct {
	header          http.Header
	writeHeaderCall int
}

func (w *countingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *countingResponseWriter) Write(b []byte) (int, error) {
	return len(b), nil
}

func (w *countingResponseWriter) WriteHeader(_ int) {
	w.writeHeaderCall++
}

type hijackableWriter struct {
	http.ResponseWriter
}

func (w *hijackableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, nil
}

type pushWriter struct {
	http.ResponseWriter
	pushedTarget string
}

func (w *pushWriter) Push(target string, _ *http.PushOptions) error {
	w.pushedTarget = target
	return nil
}

func TestSelectCompressionEncoding_QValues(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "gzip", header: "gzip", want: "gzip"},
		{name: "deflate", header: "deflate", want: "deflate"},
		{name: "prefer higher q", header: "gzip;q=0.2, deflate;q=0.8", want: "deflate"},
		{name: "both disabled", header: "gzip;q=0, deflate;q=0", want: ""},
		{name: "wildcard fallback", header: "br, *;q=0.5", want: "gzip"},
		{name: "invalid q ignored", header: "gzip;q=bogus, deflate", want: "deflate"},
		{name: "same q prefers gzip", header: "gzip;q=0.8, deflate;q=0.8", want: "gzip"},
		{name: "empty", header: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectCompressionEncoding(tt.header); got != tt.want {
				t.Fatalf("selectCompressionEncoding(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestBuildCompressibleTypes(t *testing.T) {
	exact, wildcards := buildCompressibleTypes([]string{" application/json ", "text/*", "", "TEXT/HTML"})

	if _, ok := exact["application/json"]; !ok {
		t.Fatal("expected application/json exact type")
	}
	if _, ok := exact["text/html"]; !ok {
		t.Fatal("expected text/html exact type")
	}
	if _, ok := wildcards["text"]; !ok {
		t.Fatal("expected text wildcard type")
	}
}

func TestNewCompressor_Unsupported(t *testing.T) {
	if _, err := newCompressor("br", 5, io.Discard); err == nil {
		t.Fatal("expected unsupported encoding error")
	}
}

func TestCompressResponseWriter_WriteHeaderCalledOnce(t *testing.T) {
	base := &countingResponseWriter{}
	cw := acquireCompressResponseWriter(
		base,
		"gzip",
		5,
		map[string]struct{}{"text/plain": {}},
		nil,
	)
	cw.Header().Set("Content-Type", "text/plain")

	cw.WriteHeader(http.StatusOK)
	cw.WriteHeader(http.StatusCreated)

	if base.writeHeaderCall != 1 {
		t.Fatalf("WriteHeader calls = %d, want 1", base.writeHeaderCall)
	}
}

func TestCompressResponseWriter_FlushAndInterfaces(t *testing.T) {
	t.Run("flush enabled", func(t *testing.T) {
		rec := httptest.NewRecorder()
		cw := acquireCompressResponseWriter(
			rec,
			"gzip",
			5,
			map[string]struct{}{"text/plain": {}},
			nil,
		)
		cw.Header().Set("Content-Type", "text/plain")
		cw.WriteHeader(http.StatusOK)
		cw.Flush()
	})

	t.Run("hijack unavailable", func(t *testing.T) {
		rec := httptest.NewRecorder()
		cw := acquireCompressResponseWriter(rec, "gzip", 5, nil, nil)
		if _, _, err := cw.Hijack(); err == nil {
			t.Fatal("expected hijack unavailable error")
		}
	})

	t.Run("hijack available", func(t *testing.T) {
		rec := &hijackableWriter{ResponseWriter: httptest.NewRecorder()}
		cw := acquireCompressResponseWriter(rec, "gzip", 5, nil, nil)
		if _, _, err := cw.Hijack(); err != nil {
			t.Fatalf("unexpected hijack error: %v", err)
		}
	})

	t.Run("push unavailable", func(t *testing.T) {
		rec := httptest.NewRecorder()
		cw := acquireCompressResponseWriter(rec, "gzip", 5, nil, nil)
		if err := cw.Push("/asset.js", nil); err == nil {
			t.Fatal("expected push unavailable error")
		}
	})

	t.Run("push available", func(t *testing.T) {
		rec := &pushWriter{ResponseWriter: httptest.NewRecorder()}
		cw := acquireCompressResponseWriter(rec, "gzip", 5, nil, nil)
		if err := cw.Push("/asset.js", nil); err != nil {
			t.Fatalf("unexpected push error: %v", err)
		}
		if rec.pushedTarget != "/asset.js" {
			t.Fatalf("push target = %q, want /asset.js", rec.pushedTarget)
		}
	})
}

func TestCompressResponseWriter_Unwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := acquireCompressResponseWriter(
		rec,
		"gzip",
		5,
		map[string]struct{}{"text/plain": {}},
		nil,
	)
	defer releaseCompressResponseWriter(cw)

	if got := cw.Unwrap(); got != rec {
		t.Fatalf("Unwrap() = %T, want original writer %T", got, rec)
	}
}

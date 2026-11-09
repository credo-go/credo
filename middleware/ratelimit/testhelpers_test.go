package ratelimit_test

import (
	"net/http"
	"testing"

	"github.com/credo-go/credo"
)

func mustNew(t *testing.T, opts ...credo.Option) *credo.App {
	t.Helper()
	app, err := credo.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func mustNewBench(b *testing.B, opts ...credo.Option) *credo.App {
	b.Helper()
	app, err := credo.New(opts...)
	if err != nil {
		b.Fatal(err)
	}
	return app
}

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

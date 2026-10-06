package httpwriter_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo/internal/httpwriter"
)

// basicWriter has only the methods of http.ResponseWriter: embedding the
// interface type promotes no Flush, whatever the value behind it.
type basicWriter struct {
	http.ResponseWriter
}

type flushErrorWriter struct {
	http.ResponseWriter
}

func (flushErrorWriter) FlushError() error { return nil }

func TestCanFlush(t *testing.T) {
	t.Parallel()

	cycleA := &unwrapWriter{ResponseWriter: httptest.NewRecorder()}
	cycleB := &unwrapWriter{ResponseWriter: httptest.NewRecorder(), next: cycleA}
	cycleA.next = cycleB

	chain := func(depth int, bottom http.ResponseWriter) http.ResponseWriter {
		w := bottom
		for range depth {
			w = &unwrapWriter{ResponseWriter: httptest.NewRecorder(), next: w}
		}
		return w
	}

	tests := []struct {
		name string
		w    http.ResponseWriter
		want bool
	}{
		{name: "nil writer", w: nil, want: false},
		{name: "Flusher", w: httptest.NewRecorder(), want: true},
		{name: "FlushError", w: flushErrorWriter{ResponseWriter: httptest.NewRecorder()}, want: true},
		{name: "neither", w: basicWriter{ResponseWriter: httptest.NewRecorder()}, want: false},
		{name: "Flusher behind Unwrap", w: chain(2, httptest.NewRecorder()), want: true},
		{name: "neither behind Unwrap", w: chain(2, basicWriter{ResponseWriter: httptest.NewRecorder()}), want: false},
		{name: "nil Unwrap", w: &unwrapWriter{ResponseWriter: httptest.NewRecorder()}, want: false},
		{name: "Unwrap cycle", w: cycleA, want: false},
		{name: "deepest resolvable chain", w: chain(63, httptest.NewRecorder()), want: true},
		{name: "chain beyond the bound", w: chain(64, httptest.NewRecorder()), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := httpwriter.CanFlush(tt.w); got != tt.want {
				t.Fatalf("CanFlush() = %t, want %t", got, tt.want)
			}
		})
	}
}

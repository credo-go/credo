package credo

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscardBodyWriter_Unwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &discardBodyWriter{ResponseWriter: rec}

	// http.ResponseController reaches optional interfaces through Unwrap.
	// Without Unwrap, Flush would fail with ErrNotSupported even though
	// the underlying recorder implements http.Flusher.
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		t.Fatalf("Flush through discardBodyWriter = %v, want nil", err)
	}
	if !rec.Flushed {
		t.Error("expected the underlying writer to be flushed")
	}
}

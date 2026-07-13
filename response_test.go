package credo_test

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
)

type hijackResponseWriter struct {
	*httptest.ResponseRecorder
	hijackErr        error
	hijackCalls      int
	writeHeaderCalls int
}

func newHijackResponseWriter() *hijackResponseWriter {
	return &hijackResponseWriter{ResponseRecorder: httptest.NewRecorder()}
}

func (w *hijackResponseWriter) WriteHeader(code int) {
	w.writeHeaderCalls++
	w.ResponseRecorder.WriteHeader(code)
}

func (w *hijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijackCalls++
	return nil, nil, w.hijackErr
}

type unwrapResponseWriter struct {
	http.ResponseWriter
}

func (w *unwrapResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func TestResponse_StateAccessors(t *testing.T) {
	resp := credo.NewResponse(httptest.NewRecorder())

	if resp.Committed() || resp.Hijacked() || resp.Status() != 0 || resp.Size() != 0 {
		t.Fatalf("fresh response: Status=%d Size=%d Committed=%v Hijacked=%v, want 0/0/false/false",
			resp.Status(), resp.Size(), resp.Committed(), resp.Hijacked())
	}

	resp.WriteHeader(http.StatusTeapot)
	body := []byte("short and stout")
	n, err := resp.Write(body)
	if err != nil || n != len(body) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(body))
	}

	if !resp.Committed() {
		t.Error("Committed() = false after WriteHeader, want true")
	}
	if resp.Status() != http.StatusTeapot {
		t.Errorf("Status() = %d, want %d", resp.Status(), http.StatusTeapot)
	}
	if resp.Size() != int64(len(body)) {
		t.Errorf("Size() = %d, want %d", resp.Size(), len(body))
	}

	// A second WriteHeader is a no-op once committed.
	resp.WriteHeader(http.StatusOK)
	if resp.Status() != http.StatusTeapot {
		t.Errorf("Status() after second WriteHeader = %d, want %d", resp.Status(), http.StatusTeapot)
	}

	// Reset returns the tracking state to zero for pool reuse.
	resp.Reset(httptest.NewRecorder())
	if resp.Committed() || resp.Hijacked() || resp.Status() != 0 || resp.Size() != 0 {
		t.Errorf("after Reset: Status=%d Size=%d Committed=%v Hijacked=%v, want 0/0/false/false",
			resp.Status(), resp.Size(), resp.Committed(), resp.Hijacked())
	}
}

func TestResponse_HijackedTracksActualOutcomeAndSuppressesHTTPWrites(t *testing.T) {
	underlying := newHijackResponseWriter()
	resp := credo.NewResponse(&unwrapResponseWriter{ResponseWriter: underlying})

	resp.WriteHeader(http.StatusSwitchingProtocols)
	if resp.Hijacked() {
		t.Fatal("Hijacked() = true after status 101 without actual Hijack")
	}
	if _, _, err := resp.Hijack(); err != nil {
		t.Fatalf("Hijack() error = %v", err)
	}
	if !resp.Hijacked() {
		t.Fatal("Hijacked() = false after successful actual Hijack")
	}
	if underlying.hijackCalls != 1 {
		t.Fatalf("underlying Hijack calls = %d, want 1", underlying.hijackCalls)
	}

	resp.WriteHeader(http.StatusInternalServerError)
	if underlying.writeHeaderCalls != 1 {
		t.Fatalf("WriteHeader calls after Hijack = %d, want original 101 only", underlying.writeHeaderCalls)
	}
	if n, err := resp.Write([]byte("must not leak")); n != 0 || !errors.Is(err, http.ErrHijacked) {
		t.Fatalf("Write after Hijack = %d, %v; want 0, http.ErrHijacked", n, err)
	}

	resp.Reset(httptest.NewRecorder())
	if resp.Hijacked() {
		t.Fatal("Hijacked() = true after Reset")
	}
}

func TestResponse_HijackFailureDoesNotSetState(t *testing.T) {
	wantErr := errors.New("hijack failed")
	underlying := newHijackResponseWriter()
	underlying.hijackErr = wantErr
	resp := credo.NewResponse(underlying)
	resp.WriteHeader(http.StatusSwitchingProtocols)

	if _, _, err := resp.Hijack(); !errors.Is(err, wantErr) {
		t.Fatalf("Hijack() error = %v, want %v", err, wantErr)
	}
	if resp.Hijacked() {
		t.Fatal("Hijacked() = true after failed actual Hijack")
	}
}

func TestResponse_ImplicitCommitOnWrite(t *testing.T) {
	resp := credo.NewResponse(httptest.NewRecorder())

	if _, err := resp.Write([]byte("hi")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if !resp.Committed() {
		t.Error("Committed() = false after Write, want true")
	}
	if resp.Status() != http.StatusOK {
		t.Errorf("Status() = %d, want %d (implicit 200 on first Write)", resp.Status(), http.StatusOK)
	}
}

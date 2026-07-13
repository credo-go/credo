package httpwriter_test

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo/internal/httpwriter"
)

type unwrapWriter struct {
	http.ResponseWriter
	next http.ResponseWriter
}

func (w *unwrapWriter) Unwrap() http.ResponseWriter {
	return w.next
}

type hijackWriter struct {
	http.ResponseWriter
	err   error
	calls int
}

func (w *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.calls++
	return nil, nil, w.err
}

type forwardingWriter struct {
	*unwrapWriter
	calls int
}

func (w *forwardingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.calls++
	return nil, nil, nil
}

func TestResolveHijacker(t *testing.T) {
	t.Parallel()

	direct := &hijackWriter{ResponseWriter: httptest.NewRecorder()}
	nested := &unwrapWriter{
		ResponseWriter: httptest.NewRecorder(),
		next: &unwrapWriter{
			ResponseWriter: httptest.NewRecorder(),
			next:           direct,
		},
	}
	forwarding := &forwardingWriter{unwrapWriter: &unwrapWriter{
		ResponseWriter: httptest.NewRecorder(),
		next:           httptest.NewRecorder(),
	}}

	tests := []struct {
		name    string
		writer  http.ResponseWriter
		want    http.Hijacker
		wantErr error
	}{
		{name: "nil", wantErr: httpwriter.ErrNilWriter},
		{name: "direct", writer: direct, want: direct},
		{name: "nested", writer: nested, want: direct},
		{
			name:    "non_hijacker",
			writer:  httptest.NewRecorder(),
			wantErr: httpwriter.ErrHijackerUnavailable,
		},
		{
			name:    "forwarding_method_does_not_override_unwrap",
			writer:  forwarding,
			wantErr: httpwriter.ErrHijackerUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := httpwriter.ResolveHijacker(tt.writer)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ResolveHijacker() error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ResolveHijacker() = %T, want %T", got, tt.want)
			}
		})
	}

	if forwarding.calls != 0 {
		t.Fatalf("forwarding Hijack calls = %d, want 0", forwarding.calls)
	}
}

func TestResolveHijackerRejectsMalformedUnwrapChains(t *testing.T) {
	t.Parallel()

	nilUnwrap := &unwrapWriter{ResponseWriter: httptest.NewRecorder()}
	self := &unwrapWriter{ResponseWriter: httptest.NewRecorder()}
	self.next = self
	cycleA := &unwrapWriter{ResponseWriter: httptest.NewRecorder()}
	cycleB := &unwrapWriter{ResponseWriter: httptest.NewRecorder()}
	cycleA.next = cycleB
	cycleB.next = cycleA

	deep := http.ResponseWriter(httptest.NewRecorder())
	for range 65 {
		deep = &unwrapWriter{ResponseWriter: httptest.NewRecorder(), next: deep}
	}

	tests := []struct {
		name    string
		writer  http.ResponseWriter
		wantErr error
	}{
		{name: "nil_unwrap", writer: nilUnwrap, wantErr: httpwriter.ErrNilUnwrap},
		{name: "self", writer: self, wantErr: httpwriter.ErrUnwrapCycle},
		{name: "cycle", writer: cycleA, wantErr: httpwriter.ErrUnwrapCycle},
		{name: "depth", writer: deep, wantErr: httpwriter.ErrUnwrapDepth},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := httpwriter.ResolveHijacker(tt.writer)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ResolveHijacker() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestHijackInvokesResolvedWriterAndPreservesFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("dynamic hijack failure")
	hijacker := &hijackWriter{ResponseWriter: httptest.NewRecorder(), err: wantErr}
	wrapper := &unwrapWriter{ResponseWriter: httptest.NewRecorder(), next: hijacker}

	_, _, err := httpwriter.Hijack(wrapper)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Hijack() error = %v, want %v", err, wantErr)
	}
	if hijacker.calls != 1 {
		t.Fatalf("Hijack calls = %d, want 1", hijacker.calls)
	}
}

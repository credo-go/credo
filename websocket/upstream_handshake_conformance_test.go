package websocket

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	coderwebsocket "github.com/coder/websocket"
	"github.com/credo-go/credo"
)

type coderHandshakeRecorder struct {
	header   http.Header
	statuses []int
	body     strings.Builder
	events   []string
}

func newCoderHandshakeRecorder() *coderHandshakeRecorder {
	return &coderHandshakeRecorder{header: make(http.Header)}
}

func (w *coderHandshakeRecorder) Header() http.Header {
	return w.header
}

func (w *coderHandshakeRecorder) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
	w.events = append(w.events, "write-header")
}

func (w *coderHandshakeRecorder) Write(p []byte) (int, error) {
	if len(w.statuses) == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.events = append(w.events, "write-body")
	return w.body.Write(p)
}

type coderHijackRecorder struct {
	*coderHandshakeRecorder
	hijackErr error
	peer      net.Conn
}

func (w *coderHijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.events = append(w.events, "hijack")
	if w.hijackErr != nil {
		return nil, nil, w.hijackErr
	}

	server, peer := net.Pipe()
	w.peer = peer
	return server, bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)), nil
}

func newCoderValidHandshakeRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://example.com/ws", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return r
}

func TestCoderAcceptPreUpgradeFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		mutate       func(*http.Request)
		status       int
		headers      map[string]string
		absentHeader string
	}{
		{
			name: "http_1_0_precedes_capability_check",
			mutate: func(r *http.Request) {
				r.Proto = "HTTP/1.0"
				r.ProtoMajor = 1
				r.ProtoMinor = 0
			},
			status: http.StatusUpgradeRequired,
		},
		{
			name: "missing_upgrade_precedes_capability_check",
			mutate: func(r *http.Request) {
				r.Header.Del("Connection")
			},
			status: http.StatusUpgradeRequired,
			headers: map[string]string{
				"Connection": "Upgrade",
				"Upgrade":    "websocket",
			},
		},
		{
			name: "method_precedes_capability_check",
			mutate: func(r *http.Request) {
				r.Method = http.MethodPost
			},
			status:       http.StatusMethodNotAllowed,
			absentHeader: "Allow",
		},
		{
			name: "version_precedes_capability_check",
			mutate: func(r *http.Request) {
				r.Header.Set("Sec-WebSocket-Version", "12")
			},
			status: http.StatusBadRequest,
			headers: map[string]string{
				"Sec-WebSocket-Version": "13",
			},
		},
		{
			name: "origin_precedes_capability_check",
			mutate: func(r *http.Request) {
				r.Header.Set("Origin", "https://evil.example")
			},
			status: http.StatusForbidden,
		},
		{
			name:   "missing_hijacker",
			mutate: func(*http.Request) {},
			status: http.StatusNotImplemented,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newCoderValidHandshakeRequest()
			tt.mutate(r)
			w := newCoderHandshakeRecorder()

			conn, err := coderwebsocket.Accept(w, r, nil)
			if err == nil {
				t.Fatal("Accept() error = nil, want failure")
			}
			if conn != nil {
				t.Fatal("Accept() connection is non-nil after failure")
			}
			if len(w.statuses) != 1 || w.statuses[0] != tt.status {
				t.Fatalf("status writes = %v, want [%d]", w.statuses, tt.status)
			}
			for key, want := range tt.headers {
				if got := w.Header().Get(key); got != want {
					t.Fatalf("header %q = %q, want %q", key, got, want)
				}
			}
			if tt.absentHeader != "" {
				if _, ok := w.Header()[tt.absentHeader]; ok {
					t.Fatalf("header %q is present, want absent", tt.absentHeader)
				}
			}
		})
	}
}

func TestCoderAcceptCommits101BeforeActualHijack(t *testing.T) {
	base := newCoderHandshakeRecorder()
	w := &coderHijackRecorder{coderHandshakeRecorder: base}
	response := credo.NewResponse(w)

	conn, err := coderwebsocket.Accept(response, newCoderValidHandshakeRequest(), nil)
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	t.Cleanup(func() {
		_ = conn.CloseNow()
		_ = w.peer.Close()
	})

	if got, want := base.events, []string{"write-header", "hijack"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if !response.Committed() || response.Status() != http.StatusSwitchingProtocols {
		t.Fatalf(
			"response state = status %d, committed %v; want 101, true",
			response.Status(),
			response.Committed(),
		)
	}
	if len(base.statuses) != 1 || base.statuses[0] != http.StatusSwitchingProtocols {
		t.Fatalf("status writes = %v, want [101]", base.statuses)
	}
	got := response.Header().Get("Sec-WebSocket-Accept")
	want := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got != want {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, want)
	}
}

func TestCoderAcceptHijackFailureOccursAfter101(t *testing.T) {
	hijackErr := errors.New("hijack failed")
	base := newCoderHandshakeRecorder()
	w := &coderHijackRecorder{coderHandshakeRecorder: base, hijackErr: hijackErr}
	response := credo.NewResponse(w)

	conn, err := coderwebsocket.Accept(response, newCoderValidHandshakeRequest(), nil)
	if !errors.Is(err, hijackErr) {
		t.Fatalf("Accept() error = %v, want wrapped hijack failure", err)
	}
	if conn != nil {
		t.Fatal("Accept() connection is non-nil after hijack failure")
	}

	wantEvents := []string{"write-header", "hijack", "write-body"}
	if !slices.Equal(base.events, wantEvents) {
		t.Fatalf("events = %v, want %v", base.events, wantEvents)
	}
	if len(base.statuses) != 1 || base.statuses[0] != http.StatusSwitchingProtocols {
		t.Fatalf("status writes = %v, want [101]", base.statuses)
	}
	if !response.Committed() || response.Status() != http.StatusSwitchingProtocols {
		t.Fatalf(
			"response state = status %d, committed %v; want 101, true",
			response.Status(),
			response.Committed(),
		)
	}
	if got := base.body.String(); got != http.StatusText(http.StatusInternalServerError)+"\n" {
		t.Fatalf("post-101 body = %q, want upstream second error body", got)
	}
}

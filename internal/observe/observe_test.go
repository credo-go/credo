package observe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/credo-go/credo/fault"
)

type statusError struct{ code int }

func (e statusError) Error() string   { return "status error" }
func (e statusError) HTTPStatus() int { return e.code }

type semanticStatusError struct {
	kind   fault.Kind
	legacy int
}

func (e semanticStatusError) Error() string         { return "semantic status error" }
func (e semanticStatusError) FaultKind() fault.Kind { return e.kind }
func (e semanticStatusError) HTTPStatus() int       { return e.legacy }

func TestStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		err    error
		want   int
	}{
		{"tracked status wins over error", 201, errors.New("ignored"), 201},
		{"no status, no error is 200", 0, nil, http.StatusOK},
		{"semantic status", 0, semanticStatusError{kind: fault.KindNotFound}, http.StatusNotFound},
		{
			"semantic status wins over conflicting legacy status",
			0,
			semanticStatusError{kind: fault.KindNotFound, legacy: http.StatusServiceUnavailable},
			http.StatusNotFound,
		},
		{
			"unknown semantic status fails closed",
			0,
			semanticStatusError{kind: fault.KindUnknown, legacy: http.StatusTeapot},
			http.StatusInternalServerError,
		},
		{"status-provider error", 0, statusError{code: 404}, 404},
		{"generic error is 500", 0, errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Status(tt.status, tt.err); got != tt.want {
				t.Errorf("Status(%d, %v) = %d, want %d", tt.status, tt.err, got, tt.want)
			}
		})
	}
}

func TestLevel(t *testing.T) {
	tests := []struct {
		status int
		want   slog.Level
	}{
		{200, slog.LevelInfo},
		{302, slog.LevelInfo},
		{404, slog.LevelWarn},
		{499, slog.LevelWarn},
		{500, slog.LevelError},
		{503, slog.LevelError},
	}
	for _, tt := range tests {
		if got := Level(tt.status); got != tt.want {
			t.Errorf("Level(%d) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestAccessLogAttrs(t *testing.T) {
	t.Run("base attributes only", func(t *testing.T) {
		if _, n := AccessLogAttrs("GET", "/x", 200, 12, time.Second, "1.2.3.4", "curl", "", ""); n != 7 {
			t.Errorf("attr count = %d, want 7", n)
		}
	})

	t.Run("adds path_original when rewritten", func(t *testing.T) {
		attrs, n := AccessLogAttrs("GET", "/new", 200, 0, 0, "", "", "/old", "")
		if n != 8 {
			t.Fatalf("attr count = %d, want 8", n)
		}
		if attrs[7].Key != "path_original" {
			t.Errorf("attrs[7].Key = %q, want path_original", attrs[7].Key)
		}
	})

	t.Run("skips path_original when unchanged", func(t *testing.T) {
		if _, n := AccessLogAttrs("GET", "/x", 200, 0, 0, "", "", "/x", ""); n != 7 {
			t.Errorf("attr count = %d, want 7 (path_original equals path)", n)
		}
	})

	t.Run("adds request_id when present", func(t *testing.T) {
		attrs, n := AccessLogAttrs("GET", "/x", 200, 0, 0, "", "", "", "req-1")
		if n != 8 {
			t.Fatalf("attr count = %d, want 8", n)
		}
		if attrs[7].Key != "request_id" {
			t.Errorf("attrs[7].Key = %q, want request_id", attrs[7].Key)
		}
	})

	t.Run("adds both extras", func(t *testing.T) {
		if _, n := AccessLogAttrs("GET", "/new", 200, 0, 0, "", "", "/old", "req-1"); n != 9 {
			t.Errorf("attr count = %d, want 9", n)
		}
	})
}

func TestEmitAccessLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	EmitAccessLog(context.Background(), logger, "GET", "/x", 503, 42, time.Second, "1.2.3.4", "curl", "/orig", "req-9")

	out := buf.String()
	for _, want := range []string{
		"level=ERROR", `msg="request completed"`, "method=GET", "status=503",
		"path_original=/orig", "request_id=req-9",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("access log missing %q\ngot: %s", want, out)
		}
	}
}

func TestPanicError(t *testing.T) {
	t.Run("error value is returned as-is", func(t *testing.T) {
		sentinel := errors.New("boom")
		if got := PanicError(sentinel); !errors.Is(got, sentinel) {
			t.Errorf("PanicError(err) = %v, want the original error", got)
		}
	})

	t.Run("non-error value is wrapped", func(t *testing.T) {
		got := PanicError("kaboom")
		if got == nil || !strings.Contains(got.Error(), "kaboom") {
			t.Errorf("PanicError(string) = %v, want a wrapped error mentioning the value", got)
		}
	})
}

func TestPanicAttrs(t *testing.T) {
	t.Run("base attributes", func(t *testing.T) {
		if attrs := PanicAttrs("boom", "POST", "/x", "", ""); len(attrs) != 3 {
			t.Errorf("len = %d, want 3", len(attrs))
		}
	})

	t.Run("with request id and stack", func(t *testing.T) {
		attrs := PanicAttrs("boom", "POST", "/x", "req-1", "stackdata")
		if len(attrs) != 5 {
			t.Fatalf("len = %d, want 5", len(attrs))
		}
		if attrs[3].Key != "request_id" || attrs[4].Key != "stack" {
			t.Errorf("keys = %q,%q, want request_id,stack", attrs[3].Key, attrs[4].Key)
		}
	})
}

func TestStackTrace(t *testing.T) {
	t.Run("unbounded returns a stack", func(t *testing.T) {
		if StackTrace(0) == "" {
			t.Error("StackTrace(0) returned empty")
		}
	})

	t.Run("bounded is truncated to valid utf8", func(t *testing.T) {
		s := StackTrace(16)
		if len(s) > 16 {
			t.Errorf("len = %d, want <= 16", len(s))
		}
		if !utf8.ValidString(s) {
			t.Error("truncated stack is not valid UTF-8")
		}
	})
}

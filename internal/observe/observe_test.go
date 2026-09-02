package observe

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
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
		{101, slog.LevelInfo},
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

func TestIsTypedNilLeveler(t *testing.T) {
	var typedNil *slog.LevelVar
	if IsTypedNilLeveler(nil) {
		t.Error("nil interface reported as typed-nil")
	}
	if !IsTypedNilLeveler(typedNil) {
		t.Error("nil *slog.LevelVar was not reported as typed-nil")
	}
	if IsTypedNilLeveler(slog.LevelInfo) {
		t.Error("slog.LevelInfo reported as typed-nil")
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

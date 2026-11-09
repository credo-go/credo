package observe

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"
)

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

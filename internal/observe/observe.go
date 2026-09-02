package observe

import (
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"runtime/debug"
	"unicode/utf8"

	"github.com/credo-go/credo/fault"
	internalfaultstatus "github.com/credo-go/credo/internal/faultstatus"
)

// Status resolves the final HTTP status from a tracked response status and an
// optional returned error.
func Status(status int, err error) int {
	if status != 0 {
		return status
	}
	if err == nil {
		return http.StatusOK
	}
	if provider, ok := fault.ProviderOf(err); ok {
		if semanticStatus, known := internalfaultstatus.HTTP(provider.FaultKind()); known {
			return semanticStatus
		}
		return http.StatusInternalServerError
	}
	if provider, ok := internalfaultstatus.ProviderOf(err); ok {
		return provider.HTTPStatus()
	}
	return http.StatusInternalServerError
}

// Level maps an HTTP status code to the structured log level Credo uses.
func Level(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// IsTypedNilLeveler reports whether level is a non-nil interface containing a
// nil value. A nil interface is the documented default and is not typed-nil.
// Callers use this during construction so a custom Leveler cannot panic later
// on the request path.
func IsTypedNilLeveler(level slog.Leveler) bool {
	if level == nil {
		return false
	}
	v := reflect.ValueOf(level)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// PanicError converts a recovered panic value into an error.
func PanicError(v any) error {
	if err, ok := v.(error); ok {
		return err
	}
	return fmt.Errorf("panic: %v", v)
}

// PanicAttrs builds the common structured attributes used by Credo's built-in
// recovery and the configurable middleware.Recover.
func PanicAttrs(value any, method string, path string, requestID string, stack string) []slog.Attr {
	const basePanicAttrCount = 3
	const maxPanicAttrCount = 5

	var attrs [maxPanicAttrCount]slog.Attr
	attrs[0] = slog.Any("panic", value)
	attrs[1] = slog.String("method", method)
	attrs[2] = slog.String("path", path)
	n := basePanicAttrCount
	if requestID != "" {
		attrs[n] = slog.String("request_id", requestID)
		n++
	}
	if stack != "" {
		attrs[n] = slog.String("stack", stack)
		n++
	}
	return attrs[:n]
}

// StackTrace returns the current goroutine stack. If limit is positive, the
// returned string is truncated to at most limit bytes without splitting UTF-8.
func StackTrace(limit int) string {
	stack := debug.Stack()
	if limit > 0 && len(stack) > limit {
		stack = stack[:limit]
		for !utf8.Valid(stack) {
			stack = stack[:len(stack)-1]
		}
	}
	return string(stack)
}

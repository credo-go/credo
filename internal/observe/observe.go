package observe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
	"unicode/utf8"
)

type httpStatusProvider interface {
	error
	HTTPStatus() int
}

// Status resolves the final HTTP status from a tracked response status and an
// optional returned error.
func Status(status int, err error) int {
	if status != 0 {
		return status
	}
	if err == nil {
		return http.StatusOK
	}
	if provider, ok := errors.AsType[httpStatusProvider](err); ok {
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

// EmitAccessLog assembles the standard access-log attributes and writes a
// single "request completed" entry at the status-derived level. It is the one
// source for the attribute set, message, and level shared by the built-in
// access logger and middleware.AccessLog; callers collect the per-request
// primitives (this package cannot import the root credo package).
func EmitAccessLog(
	ctx context.Context,
	logger *slog.Logger,
	method string,
	path string,
	status int,
	bytes int64,
	duration time.Duration,
	remoteAddr string,
	userAgent string,
	originalPath string,
	requestID string,
) {
	attrs, n := AccessLogAttrs(method, path, status, bytes, duration, remoteAddr, userAgent, originalPath, requestID)
	logger.LogAttrs(ctx, Level(status), "request completed", attrs[:n]...)
}

// PanicError converts a recovered panic value into an error.
func PanicError(v any) error {
	if err, ok := v.(error); ok {
		return err
	}
	return fmt.Errorf("panic: %v", v)
}

// AccessLogAttrs builds the common structured attributes used by Credo's
// built-in access logger and the configurable middleware.AccessLog.
func AccessLogAttrs(
	method string,
	path string,
	status int,
	bytes int64,
	duration time.Duration,
	remoteAddr string,
	userAgent string,
	originalPath string,
	requestID string,
) ([9]slog.Attr, int) {
	const baseAccessLogAttrCount = 7

	var attrs [9]slog.Attr
	attrs[0] = slog.String("method", method)
	attrs[1] = slog.String("path", path)
	attrs[2] = slog.Int("status", status)
	attrs[3] = slog.Int64("bytes", bytes)
	attrs[4] = slog.Duration("duration", duration)
	attrs[5] = slog.String("remote_addr", remoteAddr)
	attrs[6] = slog.String("user_agent", userAgent)
	n := baseAccessLogAttrCount
	if originalPath != "" && originalPath != path {
		attrs[n] = slog.String("path_original", originalPath)
		n++
	}
	if requestID != "" {
		attrs[n] = slog.String("request_id", requestID)
		n++
	}
	return attrs, n
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

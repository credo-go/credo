package httpclient

import (
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// NewLoggingTransport wraps base with structured outbound logging: one slog
// line per attempt with method, URL (query string and userinfo stripped —
// they routinely carry secrets), status or transport error, duration, the
// attempt number when the retry transport is active, and the trace ID when
// the request context carries one (see [SetTraceContext]).
//
// Levels follow Credo's access-log convention: Error for transport errors
// and 5xx, Warn for 4xx, Info otherwise. Headers and bodies are never
// logged.
//
// Panics if logger is nil (config misuse). A nil base defaults to
// [http.DefaultTransport].
func NewLoggingTransport(base http.RoundTripper, logger *slog.Logger) http.RoundTripper {
	if logger == nil {
		panic("httpclient: NewLoggingTransport called with nil logger")
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &loggingTransport{base: base, logger: logger}
}

type loggingTransport struct {
	base   http.RoundTripper
	logger *slog.Logger
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	duration := time.Since(start)

	attrs := make([]slog.Attr, 0, 7)
	attrs = append(attrs,
		slog.String("method", req.Method),
		slog.String("url", redactURL(req.URL)),
	)
	level := slog.LevelInfo
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
		level = slog.LevelError
	} else {
		attrs = append(attrs, slog.Int("status", resp.StatusCode))
		switch {
		case resp.StatusCode >= 500:
			level = slog.LevelError
		case resp.StatusCode >= 400:
			level = slog.LevelWarn
		}
	}
	attrs = append(attrs, slog.Duration("duration", duration))
	if n, ok := attemptFromContext(req.Context()); ok {
		attrs = append(attrs, slog.Int("attempt", n))
	}
	if tc, ok := GetTraceContext(req.Context()); ok {
		if id := tc.traceID(); id != "" {
			attrs = append(attrs, slog.String("trace_id", id))
		}
	}
	t.logger.LogAttrs(req.Context(), level, "outbound request", attrs...)

	return resp, err
}

// redactURL renders scheme://host/path, stripping the query string and
// userinfo — both routinely carry secrets (?api_key=, signed URLs, basic
// auth). Path and host are enough for diagnostics.
func redactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	r := *u
	r.User = nil
	r.RawQuery = ""
	r.ForceQuery = false
	r.Fragment = ""
	r.RawFragment = ""
	return r.String()
}

package credo_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func TestBuiltinAccessLog_Logs200(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "hello")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
	}

	if entry["method"] != "GET" {
		t.Errorf("method = %v, want GET", entry["method"])
	}
	if entry["path"] != "/test" {
		t.Errorf("path = %v, want /test", entry["path"])
	}
	if s, ok := entry["status"].(float64); !ok || int(s) != 200 {
		t.Errorf("status = %v, want 200", entry["status"])
	}
	if b, ok := entry["bytes"].(float64); !ok || int(b) != 5 {
		t.Errorf("bytes = %v, want 5", entry["bytes"])
	}
	if entry["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", entry["level"])
	}
	if entry["msg"] != "request completed" {
		t.Errorf("msg = %v, want 'request completed'", entry["msg"])
	}
}

func TestBuiltinAccessLog_UsesRealIP(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID(), credo.WithTrustedProxies("10.0.0.0/8"))
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "hello")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.10")
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
	}
	if entry["remote_addr"] != "203.0.113.10" {
		t.Fatalf("remote_addr = %v, want 203.0.113.10", entry["remote_addr"])
	}
}

func TestBuiltinAccessLog_LogLevel(t *testing.T) {
	tests := []struct {
		name   string
		status int
		level  string
	}{
		{"2xx → INFO", 200, "INFO"},
		{"3xx → INFO", 301, "INFO"},
		{"4xx → WARN", 404, "WARN"},
		{"5xx → ERROR", 500, "ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, buf := newTestLogger(t)

			app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
			status := tt.status
			app.GET("/", func(ctx *credo.Context) error {
				return ctx.Response().NoContent(status)
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/", nil)
			app.ServeHTTP(w, r)

			var entry map[string]any
			if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
				t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
			}

			if entry["level"] != tt.level {
				t.Errorf("level = %v, want %s for status %d", entry["level"], tt.level, tt.status)
			}
			if s, ok := entry["status"].(float64); !ok || int(s) != tt.status {
				t.Errorf("status = %v, want %d", entry["status"], tt.status)
			}
		})
	}
}

func TestBuiltinAccessLog_404(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/exists", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/not-found", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
	}

	if s, ok := entry["status"].(float64); !ok || int(s) != 404 {
		t.Errorf("status = %v, want 404", entry["status"])
	}
	if entry["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", entry["level"])
	}
}

func TestBuiltinAccessLog_405(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
	}

	if s, ok := entry["status"].(float64); !ok || int(s) != 405 {
		t.Errorf("status = %v, want 405", entry["status"])
	}
	if entry["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", entry["level"])
	}
}

func TestBuiltinAccessLog_PanicLogged(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/", func(ctx *credo.Context) error {
		panic("boom")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}

	entries := parseJSONLines(t, buf.Bytes())
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 log entries (access log + panic), got %d:\n%s",
			len(entries), buf.String())
	}

	// Find the access log entry.
	var accessEntry map[string]any
	for _, e := range entries {
		if e["msg"] == "request completed" {
			accessEntry = e
			break
		}
	}
	if accessEntry == nil {
		t.Fatal("expected 'request completed' log entry")
	}
	if s, ok := accessEntry["status"].(float64); !ok || int(s) != 500 {
		t.Errorf("access log status = %v, want 500", accessEntry["status"])
	}
	if accessEntry["level"] != "ERROR" {
		t.Errorf("access log level = %v, want ERROR", accessEntry["level"])
	}
	// bytes must be > 0: builtinRecover writes the 500 response body
	// before builtinAccessLog's defer fires (recover is an inner frame).
	if b, ok := accessEntry["bytes"].(float64); !ok || int(b) == 0 {
		t.Errorf("access log bytes = %v, want > 0 (panic response body)", accessEntry["bytes"])
	}

	// Verify panic log entry also exists.
	var panicEntry map[string]any
	for _, e := range entries {
		if e["msg"] == "panic recovered" {
			panicEntry = e
			break
		}
	}
	if panicEntry == nil {
		t.Fatal("expected 'panic recovered' log entry")
	}
}

func TestBuiltinAccessLog_ErrorResponse_BytesAndStatus(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/fail", func(ctx *credo.Context) error {
		return credo.NewHTTPError(404)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/fail", nil)
	app.ServeHTTP(w, r)

	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}

	entries := parseJSONLines(t, buf.Bytes())
	var accessEntry map[string]any
	for _, e := range entries {
		if e["msg"] == "request completed" {
			accessEntry = e
			break
		}
	}
	if accessEntry == nil {
		t.Fatal("expected 'request completed' log entry")
	}

	if s, ok := accessEntry["status"].(float64); !ok || int(s) != 404 {
		t.Errorf("access log status = %v, want 404", accessEntry["status"])
	}
	if accessEntry["level"] != "WARN" {
		t.Errorf("access log level = %v, want WARN", accessEntry["level"])
	}
	// bytes must be > 0 because the error response body (RFC 7807 JSON)
	// is now written before the access log fires.
	if b, ok := accessEntry["bytes"].(float64); !ok || int(b) == 0 {
		t.Errorf("access log bytes = %v, want > 0 (error response body)", accessEntry["bytes"])
	}
}

func TestBuiltinAccessLog_ErrorResponse_DurationIncludesRenderer(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/fail", func(ctx *credo.Context) error {
		return credo.NewHTTPError(500, "test.error")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/fail", nil)
	app.ServeHTTP(w, r)

	entries := parseJSONLines(t, buf.Bytes())
	var accessEntry map[string]any
	for _, e := range entries {
		if e["msg"] == "request completed" {
			accessEntry = e
			break
		}
	}
	if accessEntry == nil {
		t.Fatal("expected 'request completed' log entry")
	}

	// Verify duration is present and non-negative.
	if d, ok := accessEntry["duration"].(float64); !ok || d < 0 {
		t.Errorf("access log duration = %v, want >= 0", accessEntry["duration"])
	}
}

func TestBuiltinAccessLog_IncludesRequestID(t *testing.T) {
	logger, buf := newTestLogger(t)

	// Both built-in RequestID and AccessLog active.
	app := mustNew(t, credo.WithLogger(logger))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
	}

	reqID, ok := entry["request_id"].(string)
	if !ok || reqID == "" {
		t.Error("expected request_id in access log entry")
	}
	if got := w.Header().Get("X-Request-Id"); got != reqID {
		t.Errorf("header X-Request-Id = %q, log request_id = %q", got, reqID)
	}
}

func TestWithoutAccessLog_Disables(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutAccessLog())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if buf.Len() != 0 {
		t.Errorf("expected no log with WithoutAccessLog, got: %s", buf.String())
	}
}

func TestBuiltinAccessLog_BothDisabled(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID(), credo.WithoutAccessLog())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log with both disabled, got: %s", buf.String())
	}
	if got := w.Header().Get("X-Request-Id"); got != "" {
		t.Errorf("X-Request-Id = %q, want empty", got)
	}
}

func TestBuiltinAccessLog_FallbackRequestID(t *testing.T) {
	// When built-in RequestID is disabled but middleware.RequestID() is used,
	// the access log should still include request_id via the context store fallback.
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GlobalMiddleware(middleware.RequestID())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
	}

	reqID, ok := entry["request_id"].(string)
	if !ok || reqID == "" {
		t.Error("expected request_id in access log via context store fallback")
	}
	if got := w.Header().Get("X-Request-Id"); got != reqID {
		t.Errorf("header = %q, log request_id = %q", got, reqID)
	}
}

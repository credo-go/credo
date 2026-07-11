package middleware_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/fault"
	"github.com/credo-go/credo/middleware"
	"github.com/credo-go/credo/validation"
)

type accessLogSemanticError struct {
	kind         fault.Kind
	legacyStatus int
}

func (e accessLogSemanticError) Error() string         { return "semantic access-log error" }
func (e accessLogSemanticError) FaultKind() fault.Kind { return e.kind }
func (e accessLogSemanticError) HTTPStatus() int       { return e.legacyStatus }

func TestAccessLog_ErrorClassificationMatchesRoot(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantLevel  string
	}{
		{
			name:       "semantic kind wins over conflicting legacy status",
			err:        accessLogSemanticError{kind: fault.KindNotFound, legacyStatus: http.StatusServiceUnavailable},
			wantStatus: http.StatusNotFound,
			wantLevel:  "WARN",
		},
		{
			name: "explicit HTTP error overrides wrapped semantic kind",
			err: credo.NewHTTPError(http.StatusTeapot).WithInternal(
				accessLogSemanticError{kind: fault.KindNotFound},
			),
			wantStatus: http.StatusTeapot,
			wantLevel:  "WARN",
		},
		{
			name: "unknown semantic kind fails closed",
			err: accessLogSemanticError{
				kind:         fault.KindUnknown,
				legacyStatus: http.StatusTeapot,
			},
			wantStatus: http.StatusInternalServerError,
			wantLevel:  "ERROR",
		},
		{
			name: "wrapped validation errors remain 422",
			err: errors.Join(
				errors.New("validation context"),
				validation.Errors{{Field: "name", Code: "required", Message: "required"}},
			),
			wantStatus: http.StatusUnprocessableEntity,
			wantLevel:  "WARN",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, buf := newTestLogger(t)
			app := mustNew(t, credo.WithoutAccessLog())
			app.GET("/", func(*credo.Context) error {
				return tt.err
			}).Middleware(middleware.AccessLog(middleware.AccessLogConfig{Logger: logger}))

			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

			if w.Code != tt.wantStatus {
				t.Fatalf("response status = %d, want %d", w.Code, tt.wantStatus)
			}
			var entry map[string]any
			if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
				t.Fatalf("parse access log: %v\nraw: %s", err, buf.String())
			}
			if got := int(entry["status"].(float64)); got != tt.wantStatus {
				t.Errorf("logged status = %d, want %d", got, tt.wantStatus)
			}
			if got := entry["level"]; got != tt.wantLevel {
				t.Errorf("logged level = %v, want %s", got, tt.wantLevel)
			}
		})
	}
}

func TestAccessLog_NoDuplicateRequestID(t *testing.T) {
	logger, buf := newTestLogger(t)

	// RequestID enriches the request-scoped logger; AccessLog must not add
	// a second explicit request_id attribute on top of it.
	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID(), credo.WithoutAccessLog())
	app.GlobalMiddleware(middleware.AccessLog(), middleware.RequestID())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", "trace-dup")
	app.ServeHTTP(w, r)

	if got := strings.Count(buf.String(), `"request_id"`); got != 1 {
		t.Fatalf("request_id key count = %d, want exactly 1\nraw: %s", got, buf.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse log: %v", err)
	}
	if entry["request_id"] != "trace-dup" {
		t.Errorf("request_id = %v, want trace-dup", entry["request_id"])
	}
}

func TestAccessLog_CustomLoggerGetsExplicitRequestID(t *testing.T) {
	logger, buf := newTestLogger(t)

	// A custom Logger never carries the request-scoped enrichment, so the
	// explicit request_id attribute must be added even when RequestID ran.
	app := mustNew(t, credo.WithoutRequestID(), credo.WithoutAccessLog())
	app.GlobalMiddleware(
		middleware.AccessLog(middleware.AccessLogConfig{Logger: logger}),
		middleware.RequestID(),
	)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", "trace-custom")
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse log: %v\nraw: %s", err, buf.String())
	}
	if entry["request_id"] != "trace-custom" {
		t.Errorf("request_id = %v, want trace-custom (explicit attr on custom logger)", entry["request_id"])
	}
}

func TestAccessLog_Logs200(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "hello")
	}).Middleware(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: logger,
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}

	if entry["method"] != "GET" {
		t.Errorf("method = %v, want GET", entry["method"])
	}
	if entry["path"] != "/test" {
		t.Errorf("path = %v, want /test", entry["path"])
	}
	if status, ok := entry["status"].(float64); !ok || int(status) != 200 {
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

func TestAccessLog_UsesRealIP(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithTrustedProxies("10.0.0.0/8"))
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "hello")
	}).Middleware(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: logger,
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.10")
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}
	if entry["remote_addr"] != "203.0.113.10" {
		t.Fatalf("remote_addr = %v, want 203.0.113.10", entry["remote_addr"])
	}
}

func TestAccessLog_LogLevel(t *testing.T) {
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

			app := mustNew(t)
			status := tt.status
			app.GET("/", func(ctx *credo.Context) error {
				return ctx.Response().NoContent(status)
			}).Middleware(middleware.AccessLog(middleware.AccessLogConfig{
				Logger: logger,
			}))

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/", nil)
			app.ServeHTTP(w, r)

			var entry map[string]any
			if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
				t.Fatalf("failed to parse log: %v", err)
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

func TestAccessLog_Skipper(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t)
	mw := middleware.AccessLog(middleware.AccessLogConfig{
		Logger: logger,
		Skipper: func(ctx *credo.Context) bool {
			return ctx.Request().URL.Path == "/health"
		},
	})

	app.GET("/health", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(mw)
	app.GET("/api", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(mw)

	// Skipped request
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	app.ServeHTTP(w, r)

	if buf.Len() != 0 {
		t.Error("expected no log for skipped request")
	}

	// Logged request
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/api", nil)
	app.ServeHTTP(w, r)

	if buf.Len() == 0 {
		t.Error("expected log for non-skipped request")
	}
}

func TestAccessLog_Skipper_Nil(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.AccessLog(middleware.AccessLogConfig{
		Logger:  logger,
		Skipper: nil, // all requests logged
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if buf.Len() == 0 {
		t.Error("expected log when Skipper is nil")
	}
}

func TestAccessLog_RequestID(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(
		middleware.RequestID(),
		middleware.AccessLog(middleware.AccessLogConfig{
			Logger: logger,
		}),
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}

	reqID, ok := entry["request_id"].(string)
	if !ok || reqID == "" {
		t.Error("expected request_id in log when stacked with RequestID middleware")
	}
}

func TestAccessLog_NoRequestID(t *testing.T) {
	logger, buf := newTestLogger(t)

	// Disable built-in request ID so GetRequestID returns "" in middleware access log.
	app := mustNew(t, credo.WithoutRequestID())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: logger,
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}

	if _, exists := entry["request_id"]; exists {
		t.Error("request_id should not be present without RequestID middleware")
	}
}

func TestAccessLog_BytesCount(t *testing.T) {
	logger, buf := newTestLogger(t)

	body := "hello world!" // 12 bytes
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, body)
	}).Middleware(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: logger,
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}

	if b, ok := entry["bytes"].(float64); !ok || int(b) != 12 {
		t.Errorf("bytes = %v, want 12", entry["bytes"])
	}
}

func TestAccessLog_Duration(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: logger,
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}

	dur, ok := entry["duration"].(float64)
	if !ok {
		t.Fatal("expected duration in log entry")
	}
	if dur < 0 {
		t.Errorf("duration = %v, want non-negative", dur)
	}
}

func TestAccessLog_LogsOriginalPathAfterRewrite(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithoutAccessLog())
	app.GlobalMiddleware(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: logger,
	}))
	app.GET("/old", func(ctx *credo.Context) error {
		return ctx.Rewrite("/new")
	})
	app.GET("/new", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/old", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}

	if entry["path"] != "/new" {
		t.Errorf("path = %v, want /new", entry["path"])
	}
	if entry["path_original"] != "/old" {
		t.Errorf("path_original = %v, want /old", entry["path_original"])
	}
}

func TestAccessLog_ImplicitStatus200(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	}).Middleware(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: logger,
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}

	if status, ok := entry["status"].(float64); !ok || int(status) != 200 {
		t.Errorf("status = %v, want 200", entry["status"])
	}
}

func TestAccessLog_DefaultConfig(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.AccessLog())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r) // should not panic
}

func TestAccessLog_MetaSilencesRoute(t *testing.T) {
	logger, buf := newTestLogger(t)

	// Built-in access logger disabled; the configurable middleware honours the
	// MetaAccessLog route meta (read after next, once the route is known),
	// matching the built-in logger's behaviour.
	app := mustNew(t, credo.WithoutAccessLog())
	app.GlobalMiddleware(middleware.AccessLog(middleware.AccessLogConfig{Logger: logger}))
	app.GET("/silent", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).SetMeta(credo.MetaAccessLog, false)
	app.GET("/loud", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/silent", nil))
	if buf.Len() != 0 {
		t.Errorf("expected no log for silenced route, got: %s", buf.String())
	}

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/loud", nil))
	if buf.Len() == 0 {
		t.Error("expected log for non-silenced sibling route")
	}
}

func TestAccessLog_MetaSilenceWithCustomLogger(t *testing.T) {
	logger, buf := newTestLogger(t)

	// AccessLog attached as route middleware with a custom Logger; the route
	// meta still silences it (covers the route-attached, custom-logger path).
	app := mustNew(t)
	app.GET("/silent", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).
		Middleware(middleware.AccessLog(middleware.AccessLogConfig{Logger: logger})).
		SetMeta(credo.MetaAccessLog, false)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/silent", nil))
	if buf.Len() != 0 {
		t.Errorf("expected no log for silenced route with custom logger, got: %s", buf.String())
	}
}

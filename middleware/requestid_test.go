package middleware_test

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func TestRequestID_EnrichesLogger(t *testing.T) {
	logger, buf := newTestLogger(t)

	// Built-in request ID disabled — middleware.RequestID provides the ID
	// and must enrich the request-scoped logger like the built-in tier does.
	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID(), credo.WithoutAccessLog())
	app.GET("/", func(ctx *credo.Context) error {
		ctx.Logger().Info("inside handler")
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.RequestID())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse log: %v\nraw: %s", err, buf.String())
	}
	id := w.Header().Get("X-Request-Id")
	if id == "" {
		t.Fatal("expected X-Request-Id response header")
	}
	if entry["request_id"] != id {
		t.Errorf("handler log request_id = %v, want %q (logger should be enriched)", entry["request_id"], id)
	}
}

func TestRequestID_BuiltinAccessLogCarriesRequestID(t *testing.T) {
	logger, buf := newTestLogger(t)

	// Built-in request ID OFF, built-in access log ON: the ID injected by
	// middleware.RequestID must reach the built-in access log via the
	// enriched logger — exactly once.
	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GlobalMiddleware(middleware.RequestID())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", "trace-xyz")
	app.ServeHTTP(w, r)

	if got := strings.Count(buf.String(), `"request_id"`); got != 1 {
		t.Fatalf("request_id key count = %d, want exactly 1\nraw: %s", got, buf.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse log: %v", err)
	}
	if entry["msg"] != "request completed" {
		t.Errorf("msg = %v, want 'request completed'", entry["msg"])
	}
	if entry["request_id"] != "trace-xyz" {
		t.Errorf("request_id = %v, want trace-xyz", entry["request_id"])
	}
}

func TestRequestID_GeneratesID(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.RequestID())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	id := w.Header().Get("X-Request-Id")
	if id == "" {
		t.Fatal("expected X-Request-Id response header to be set")
	}
}

func TestRequestID_GeneratedIDFormat(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.RequestID())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	id := w.Header().Get("X-Request-Id")
	if matched, _ := regexp.MatchString(`^[A-Z2-7]{26}$`, id); !matched {
		t.Errorf("id = %q, want 26-char base32 string (crypto/rand.Text format)", id)
	}
}

func TestRequestID_UsesExisting(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.RequestID())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", "my-trace-id-123")
	app.ServeHTTP(w, r)

	id := w.Header().Get("X-Request-Id")
	if id != "my-trace-id-123" {
		t.Errorf("id = %q, want 'my-trace-id-123'", id)
	}
}

func TestRequestID_LimitExceeded(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.RequestID(middleware.RequestIDConfig{
		Limit: 10,
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", "this-id-is-too-long-for-the-limit")
	app.ServeHTTP(w, r)

	id := w.Header().Get("X-Request-Id")
	if id == "this-id-is-too-long-for-the-limit" {
		t.Error("expected oversized ID to be replaced with a generated one")
	}
	if id == "" {
		t.Error("expected a generated ID, got empty string")
	}
}

func TestRequestID_GetRequestID(t *testing.T) {
	var captured string
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		captured = middleware.GetRequestID(ctx)
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.RequestID())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if captured == "" {
		t.Error("GetRequestID returned empty string inside handler")
	}
	// Should match the response header
	if captured != w.Header().Get("X-Request-Id") {
		t.Errorf("GetRequestID = %q, response header = %q", captured, w.Header().Get("X-Request-Id"))
	}
}

func TestRequestID_GetRequestID_NoMiddleware(t *testing.T) {
	// Without route-level middleware AND built-in disabled, GetRequestID should return "".
	var captured string
	app := mustNew(t, credo.WithoutRequestID())
	app.GET("/", func(ctx *credo.Context) error {
		captured = middleware.GetRequestID(ctx)
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if captured != "" {
		t.Errorf("GetRequestID = %q, want empty string without middleware", captured)
	}
}

func TestRequestID_CustomGenerator(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.RequestID(middleware.RequestIDConfig{
		Generator: func() string { return "custom-id-42" },
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	id := w.Header().Get("X-Request-Id")
	if id != "custom-id-42" {
		t.Errorf("id = %q, want 'custom-id-42'", id)
	}
}

func TestRequestID_CustomHeader(t *testing.T) {
	// Disable built-in RequestID which uses X-Request-Id by default.
	app := mustNew(t, credo.WithoutRequestID())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.RequestID(middleware.RequestIDConfig{
		Header: "X-Trace-Id",
	}))

	// Set on custom header
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Trace-Id", "trace-abc")
	app.ServeHTTP(w, r)

	if w.Header().Get("X-Trace-Id") != "trace-abc" {
		t.Errorf("X-Trace-Id = %q, want 'trace-abc'", w.Header().Get("X-Trace-Id"))
	}
	// Default header should NOT be set
	if w.Header().Get("X-Request-Id") != "" {
		t.Error("X-Request-Id should not be set when custom header is used")
	}
}

func TestRequestID_UniquePerRequest(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.RequestID())

	w1 := httptest.NewRecorder()
	r1 := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w1, r1)

	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w2, r2)

	id1 := w1.Header().Get("X-Request-Id")
	id2 := w2.Header().Get("X-Request-Id")
	if id1 == id2 {
		t.Errorf("two requests got the same ID: %q", id1)
	}
}

func TestRequestID_RejectsControlChars(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"newline", "abc\ndef"},
		{"null byte", "abc\x00def"},
		{"tab", "abc\tdef"},
		{"space", "abc def"},
		{"angle brackets", "abc<script>"},
		{"unicode", "abc\u00e9def"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.GET("/", func(ctx *credo.Context) error {
				return ctx.Response().NoContent(200)
			}).Middleware(middleware.RequestID())

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("X-Request-Id", tt.id)
			app.ServeHTTP(w, r)

			id := w.Header().Get("X-Request-Id")
			if id == tt.id {
				t.Errorf("expected invalid ID %q to be replaced", tt.id)
			}
			if id == "" {
				t.Error("expected a generated replacement ID, got empty")
			}
		})
	}
}

func TestRequestID_AcceptsValidChars(t *testing.T) {
	validID := "abc-DEF_012.xyz"

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(middleware.RequestID())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", validID)
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-Request-Id"); got != validID {
		t.Errorf("X-Request-Id = %q, want %q (valid ID should be preserved)", got, validID)
	}
}

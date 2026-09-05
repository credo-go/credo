package credo_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

func TestRequestID_GeneratesID(t *testing.T) {
	app := mustNew(t)
	app.UseRequestID()
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	id := w.Header().Get("X-Request-Id")
	if id == "" {
		t.Fatal("expected X-Request-Id response header to be set")
	}
	// crypto/rand.Text produces 26-char base32 strings.
	if matched, _ := regexp.MatchString(`^[A-Z2-7]{26}$`, id); !matched {
		t.Errorf("id = %q, want 26-char base32 string", id)
	}
}

func TestRequestID_PreservesExisting(t *testing.T) {
	app := mustNew(t)
	app.UseRequestID()
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", "my-trace-123")
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-Request-Id"); got != "my-trace-123" {
		t.Errorf("X-Request-Id = %q, want 'my-trace-123'", got)
	}
}

func TestRequestID_RejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"control chars", "abc\ndef"},
		{"space", "abc def"},
		{"angle brackets", "abc<script>"},
		{"too long", strings.Repeat("a", 65)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.UseRequestID()
			app.GET("/", func(ctx *credo.Context) error {
				return ctx.Response().NoContent(200)
			})

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

func TestRequestID_EnrichesLogger(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger))
	app.UseRequestID()
	app.GET("/", func(ctx *credo.Context) error {
		ctx.Logger().Info("handler called")
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
		t.Error("expected request_id attribute in handler's logger")
	}
	// Should match the response header.
	if got := w.Header().Get("X-Request-Id"); got != reqID {
		t.Errorf("header X-Request-Id = %q, log request_id = %q", got, reqID)
	}
}

func TestRequestID_CustomConfig(t *testing.T) {
	app := mustNew(t)
	app.UseRequestID(credo.RequestIDConfig{
		Header:    "X-Trace-Id",
		Generator: func() string { return "generated-trace" },
		Limit:     8,
	})
	var captured string
	app.GET("/", func(ctx *credo.Context) error {
		captured = ctx.RequestID()
		return ctx.Response().NoContent(200)
	})

	// Incoming value within the limit is preserved on the custom header.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Trace-Id", "abc123")
	app.ServeHTTP(w, r)
	if got := w.Header().Get("X-Trace-Id"); got != "abc123" || captured != "abc123" {
		t.Errorf("X-Trace-Id = %q, RequestID = %q, want abc123", got, captured)
	}
	if got := w.Header().Get("X-Request-Id"); got != "" {
		t.Errorf("X-Request-Id = %q, want empty with a custom header", got)
	}

	// Over the limit → the custom generator replaces it.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Trace-Id", "123456789")
	app.ServeHTTP(w, r)
	if got := w.Header().Get("X-Trace-Id"); got != "generated-trace" {
		t.Errorf("X-Trace-Id = %q, want generated-trace", got)
	}
}

func TestUseRequestID_Misuse(t *testing.T) {
	t.Run("twice", func(t *testing.T) {
		app := mustNew(t)
		app.UseRequestID()
		defer func() {
			if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "called twice") {
				t.Fatalf("panic = %v, want called twice", r)
			}
		}()
		app.UseRequestID()
	})
	t.Run("two configs", func(t *testing.T) {
		app := mustNew(t)
		defer func() {
			if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "at most one config") {
				t.Fatalf("panic = %v, want at most one config", r)
			}
		}()
		app.UseRequestID(credo.RequestIDConfig{}, credo.RequestIDConfig{})
	})
	t.Run("after prepare", func(t *testing.T) {
		app := mustNew(t)
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic from UseRequestID after preparation")
			}
		}()
		app.UseRequestID()
	})
}

func TestContext_RequestID(t *testing.T) {
	var captured string

	app := mustNew(t)
	app.UseRequestID()
	app.GET("/", func(ctx *credo.Context) error {
		captured = ctx.RequestID()
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if captured == "" {
		t.Fatal("Context.RequestID returned empty string")
	}
	if got := w.Header().Get("X-Request-Id"); got != captured {
		t.Errorf("Context.RequestID = %q, response header = %q", captured, got)
	}
}

func TestRequestID_OffByDefault(t *testing.T) {
	var captured string
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		captured = ctx.RequestID()
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", "incoming")
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-Request-Id"); got != "" {
		t.Errorf("X-Request-Id = %q, want empty without UseRequestID", got)
	}
	if captured != "" {
		t.Errorf("Context.RequestID = %q, want empty without UseRequestID", captured)
	}
}

func TestRequestID_UniquePerRequest(t *testing.T) {
	app := mustNew(t)
	app.UseRequestID()
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

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

package credo_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func TestBuiltinRecover_CatchesPanic(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		panic("boom")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if body["success"] != false {
		t.Errorf("success = %v, want false", body["success"])
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %#v, want a JSON object", body["error"])
	}
	if errObj["code"] != "internal_server_error" {
		t.Errorf("error.code = %v, want internal_server_error", errObj["code"])
	}
	if errObj["message"] != "Internal Server Error" {
		t.Errorf("error.message = %v, want Internal Server Error", errObj["message"])
	}
}

func TestBuiltinRecover_UpgradeHeaderBeforeHijackWritesErrorResponse(t *testing.T) {
	tests := []struct {
		name       string
		connHeader string
	}{
		{"standard", "Upgrade"},
		{"lowercase", "upgrade"},
		{"multi-token", "keep-alive, Upgrade"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.GET("/ws", func(ctx *credo.Context) error {
				panic("websocket panic")
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/ws", nil)
			r.Header.Set("Connection", tt.connHeader)
			app.ServeHTTP(w, r)

			// Upgrade request headers are not proof that the transport was
			// hijacked. A pre-hijack panic must use the normal HTTP pipeline.
			if w.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", w.Code)
			}
			if w.Body.Len() == 0 {
				t.Error("body is empty, want Problem Details")
			}
		})
	}
}

func TestBuiltinRecover_ActualHijackDoesNotWriteErrorResponse(t *testing.T) {
	app := mustNew(t)
	app.GET("/ws", func(ctx *credo.Context) error {
		if _, _, err := ctx.Response().Hijack(); err != nil {
			t.Fatalf("Hijack() error = %v", err)
		}
		panic("post-hijack panic")
	})

	w := newHijackResponseWriter()
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	app.ServeHTTP(w, r)

	if w.writeHeaderCalls != 0 || w.Body.Len() != 0 {
		t.Fatalf("post-hijack HTTP writes = %d/%q, want none", w.writeHeaderCalls, w.Body.String())
	}
}

func TestBuiltinRecover_RepanicAbortHandler(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		panic(http.ErrAbortHandler)
	})

	defer func() {
		rvr := recover()
		err, ok := rvr.(error)
		if !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("expected http.ErrAbortHandler re-panic, got %v", rvr)
		}
	}()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)
	t.Fatal("expected panic to propagate")
}

func TestBuiltinRecover_WithoutRecover(t *testing.T) {
	app := mustNew(t, credo.WithoutRecover())
	app.GET("/", func(ctx *credo.Context) error {
		panic("should propagate")
	})

	defer func() {
		rvr := recover()
		if rvr == nil {
			t.Fatal("expected panic to propagate with WithoutRecover")
		}
		if rvr != "should propagate" {
			t.Errorf("panic = %v, want 'should propagate'", rvr)
		}
	}()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)
	t.Fatal("expected panic to propagate")
}

func TestBuiltinRecover_NoPanic(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("body = %q, want 'ok'", w.Body.String())
	}
}

func TestBuiltinRecover_IncludesRequestID(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutAccessLog())
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

	reqID, ok := panicEntry["request_id"].(string)
	if !ok || reqID == "" {
		t.Error("expected request_id in panic log entry")
	}
}

func TestBuiltinRecover_FallbackRequestID(t *testing.T) {
	// When built-in RequestID is disabled but middleware.RequestID() is used,
	// the panic log should still include request_id via context store fallback.
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID(), credo.WithoutAccessLog())
	app.GlobalMiddleware(middleware.RequestID())
	app.GET("/", func(ctx *credo.Context) error {
		panic("boom")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	entries := parseJSONLines(t, buf.Bytes())
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

	reqID, ok := panicEntry["request_id"].(string)
	if !ok || reqID == "" {
		t.Error("expected request_id in panic log via context store fallback")
	}
}

func TestBuiltinRecover_CatchesMiddlewarePanic(t *testing.T) {
	app := mustNew(t)

	panicMW := func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			panic("middleware panic")
		}
	}

	app.GlobalMiddleware(panicMW)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

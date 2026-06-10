package credo_test

import (
	"encoding/json"
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

	var pd map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("failed to parse problem details: %v", err)
	}
	if pd["title"] != "Internal Server Error" {
		t.Errorf("title = %v, want Internal Server Error", pd["title"])
	}
}

func TestBuiltinRecover_WebSocketUpgrade_NoErrorResponse(t *testing.T) {
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

			// The writer may be hijacked on upgraded connections — the
			// built-in recovery must not write a 500 response.
			if w.Code != 200 {
				t.Errorf("status = %d, want 200 (no status written for websocket)", w.Code)
			}
			if w.Body.Len() != 0 {
				t.Errorf("body = %q, want empty (no error response for websocket)", w.Body.String())
			}
		})
	}
}

func TestBuiltinRecover_RepanicAbortHandler(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		panic(http.ErrAbortHandler)
	})

	defer func() {
		rvr := recover()
		if rvr != http.ErrAbortHandler {
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

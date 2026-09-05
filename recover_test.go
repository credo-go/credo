package credo_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

func TestRecover_CatchesPanic(t *testing.T) {
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

func TestRecover_UpgradeHeaderBeforeHijackWritesErrorResponse(t *testing.T) {
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

func TestRecover_ActualHijackDoesNotWriteErrorResponse(t *testing.T) {
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

func TestRecover_RepanicAbortHandler(t *testing.T) {
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

func TestRecover_WithoutRecover(t *testing.T) {
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

func TestRecover_NoPanic(t *testing.T) {
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

func TestRecover_IncludesRequestID(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger))
	app.UseRequestID()
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

func TestRecover_DedicatedLoggerGetsExplicitRequestID(t *testing.T) {
	// A dedicated recovery logger never carries request-scoped enrichment,
	// so the panic record gets the request ID as an explicit attribute.
	appLogger, appBuf := newTestLogger(t)
	panicLogger, panicBuf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(appLogger), credo.WithRecoverConfig(credo.RecoverConfig{
		Logger:            panicLogger,
		DisableStackTrace: true,
	}))
	app.UseRequestID()
	app.GET("/", func(ctx *credo.Context) error {
		panic("boom")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", "trace-42")
	app.ServeHTTP(w, r)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if strings.Contains(appBuf.String(), "panic recovered") {
		t.Errorf("panic record leaked to the app logger: %s", appBuf.String())
	}
	entries := parseJSONLines(t, panicBuf.Bytes())
	if len(entries) != 1 || entries[0]["msg"] != "panic recovered" {
		t.Fatalf("dedicated logger entries = %v, want one panic record", entries)
	}
	if got := entries[0]["request_id"]; got != "trace-42" {
		t.Errorf("request_id = %v, want trace-42", got)
	}
	if _, ok := entries[0]["stack"]; ok {
		t.Errorf("stack present with DisableStackTrace: %v", entries[0])
	}
}

func TestRecover_StackSizeTruncates(t *testing.T) {
	logger, buf := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger), credo.WithRecoverConfig(credo.RecoverConfig{StackSize: 64}))
	app.GET("/", func(ctx *credo.Context) error {
		panic("boom")
	})

	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	entries := parseJSONLines(t, buf.Bytes())
	var stack string
	for _, e := range entries {
		if e["msg"] == "panic recovered" {
			stack, _ = e["stack"].(string)
		}
	}
	if stack == "" || len(stack) > 64 {
		t.Fatalf("stack len = %d, want 1..64", len(stack))
	}
}

func TestRecover_WithoutRecoverWinsOverConfig(t *testing.T) {
	for _, order := range [][]credo.Option{
		{credo.WithoutRecover(), credo.WithRecoverConfig(credo.RecoverConfig{})},
		{credo.WithRecoverConfig(credo.RecoverConfig{}), credo.WithoutRecover()},
	} {
		app := mustNew(t, order...)
		app.GET("/", func(ctx *credo.Context) error {
			panic("boom")
		})
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected the panic to propagate with WithoutRecover")
				}
			}()
			app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		}()
	}
}

func TestRecover_CatchesMiddlewarePanic(t *testing.T) {
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

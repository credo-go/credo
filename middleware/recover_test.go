package middleware_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

type recoverHijackWriter struct {
	*httptest.ResponseRecorder
	hijackCalls      int
	writeHeaderCalls int
}

func newRecoverHijackWriter() *recoverHijackWriter {
	return &recoverHijackWriter{ResponseRecorder: httptest.NewRecorder()}
}

func (w *recoverHijackWriter) WriteHeader(code int) {
	w.writeHeaderCalls++
	w.ResponseRecorder.WriteHeader(code)
}

func (w *recoverHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijackCalls++
	return nil, nil, nil
}

func TestRecover_CatchesStringPanic(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		panic("something went wrong")
	}).Middleware(middleware.Recover())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestRecover_CatchesErrorPanic(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		panic(http.ErrServerClosed)
	}).Middleware(middleware.Recover())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestRecover_RepanicAbortHandler(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		panic(http.ErrAbortHandler)
	}).Middleware(middleware.Recover())

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
	t.Fatal("expected panic to propagate, handler returned normally")
}

func TestRecover_NoPanic(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	}).Middleware(middleware.Recover())

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

func TestRecover_LogsPanic(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t)
	app.POST("/api/test", func(ctx *credo.Context) error {
		panic("test panic")
	}).Middleware(middleware.Recover(middleware.RecoverConfig{
		Logger: logger,
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/test", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}

	if entry["panic"] != "test panic" {
		t.Errorf("panic = %v, want 'test panic'", entry["panic"])
	}
	if entry["method"] != "POST" {
		t.Errorf("method = %v, want POST", entry["method"])
	}
	if entry["path"] != "/api/test" {
		t.Errorf("path = %v, want /api/test", entry["path"])
	}
	if entry["msg"] != "panic recovered" {
		t.Errorf("msg = %v, want 'panic recovered'", entry["msg"])
	}
}

func TestRecover_StackTrace_Enabled(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		panic("stack test")
	}).Middleware(middleware.Recover(middleware.RecoverConfig{
		Logger: logger,
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}

	stack, ok := entry["stack"].(string)
	if !ok || stack == "" {
		t.Error("expected stack trace in log entry")
	}
	if !strings.Contains(stack, "goroutine") {
		t.Errorf("stack trace does not contain 'goroutine': %s", stack[:min(100, len(stack))])
	}
}

func TestRecover_StackTrace_Disabled(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		panic("no stack test")
	}).Middleware(middleware.Recover(middleware.RecoverConfig{
		Logger:            logger,
		DisableStackTrace: true,
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}

	if _, exists := entry["stack"]; exists {
		t.Error("stack trace should not be present when DisableStackTrace is true")
	}
}

func TestRecover_RequestID_InLog(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		panic("with request id")
	}).Middleware(
		middleware.RequestID(),
		middleware.Recover(middleware.RecoverConfig{
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
		t.Error("expected request_id in panic log entry")
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
			}).Middleware(middleware.Recover())

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/ws", nil)
			r.Header.Set("Connection", tt.connHeader)
			app.ServeHTTP(w, r)

			if w.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500 before actual Hijack", w.Code)
			}
		})
	}
}

func TestRecover_ActualHijackDoesNotPropagateHTTPError(t *testing.T) {
	app := mustNew(t)
	app.GET("/ws", func(ctx *credo.Context) error {
		if _, _, err := ctx.Response().Hijack(); err != nil {
			t.Fatalf("Hijack() error = %v", err)
		}
		panic("post-hijack panic")
	}).Middleware(middleware.Recover())

	w := newRecoverHijackWriter()
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	app.ServeHTTP(w, r)

	if w.hijackCalls != 1 {
		t.Fatalf("Hijack calls = %d, want 1", w.hijackCalls)
	}
	if w.writeHeaderCalls != 0 || w.Body.Len() != 0 {
		t.Fatalf("post-hijack HTTP writes = %d/%q, want none", w.writeHeaderCalls, w.Body.String())
	}
}

func TestRecover_ResponseBody(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		panic("body test")
	}).Middleware(middleware.Recover())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}

	if body["success"] != false || body["code"] != "internal_server_error" {
		t.Errorf("identity = %#v, want success=false/code=internal_server_error", body)
	}
	if body["message"] != "Internal Server Error" {
		t.Errorf("message = %v, want Internal Server Error", body["message"])
	}
}

func TestRecover_CommittedResponse_NoPanic500(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		ctx.Response().WriteHeader(200)
		ctx.Response().Write([]byte("partial"))
		panic("after commit")
	}).Middleware(middleware.Recover())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	// Status should remain 200 (already committed), not overwritten to 500
	if w.Code != 200 {
		t.Errorf("status = %d, want 200 (committed before panic)", w.Code)
	}
}

func TestRecover_StackSize_Truncation(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		panic("stack truncation test")
	}).Middleware(middleware.Recover(middleware.RecoverConfig{
		Logger:    logger,
		StackSize: 100, // Very small — force truncation
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}

	stack, ok := entry["stack"].(string)
	if !ok {
		t.Fatal("expected stack trace in log entry")
	}
	if len(stack) > 100 {
		t.Errorf("stack length = %d, want <= 100 (StackSize limit)", len(stack))
	}
}

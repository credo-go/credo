package credo_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// freePort finds a free TCP port and returns host, port, and the combined addr.
func freePort(t *testing.T) (string, int, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	return host, port, addr
}

// mustNew creates an App or fails the test.
func mustNew(t *testing.T, opts ...credo.Option) *credo.App {
	t.Helper()
	app, err := credo.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// --- State transition tests ---

func TestApp_InitialState(t *testing.T) {
	app := mustNew(t)
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q, want %q", got, "building")
	}
	if app.IsRunning() {
		t.Error("IsRunning() = true, want false")
	}
}

func TestApp_ServeHTTP_DoesNotChangeState(t *testing.T) {
	app := mustNew(t)
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ping", nil)
	app.ServeHTTP(w, r)

	if got := app.State(); got != "building" {
		t.Errorf("State() after ServeHTTP = %q, want %q", got, "building")
	}
	if app.IsRunning() {
		t.Error("IsRunning() = true after ServeHTTP, want false")
	}
}

func TestApp_Run_TransitionsToRunning(t *testing.T) {
	host, port, _ := freePort(t)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	// Wait for the server to start.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !app.IsRunning() {
		t.Fatal("server did not reach running state")
	}
	if got := app.State(); got != "running" {
		t.Errorf("State() = %q, want %q", got, "running")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
}

func TestApp_Run_Double(t *testing.T) {
	host, port, _ := freePort(t)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Second Run should return an error (not panic).
	err := app.Run()
	if err == nil {
		t.Fatal("second Run() should return error")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_ = app.Shutdown(ctx)
	<-errCh
}

func TestApp_Shutdown_TransitionsToStopped(t *testing.T) {
	host, port, _ := freePort(t)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}

	if got := app.State(); got != "stopped" {
		t.Errorf("State() after Shutdown = %q, want %q", got, "stopped")
	}
	<-errCh
}

func TestApp_Shutdown_NotRunning(t *testing.T) {
	app := mustNew(t)
	ctx := t.Context()
	err := app.Shutdown(ctx)
	if err == nil {
		t.Fatal("Shutdown() on building app should return error")
	}
}

func TestApp_Shutdown_Double(t *testing.T) {
	host, port, _ := freePort(t)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown() error: %v", err)
	}

	// Second shutdown should return error.
	err := app.Shutdown(ctx)
	if err == nil {
		t.Fatal("second Shutdown() should return error")
	}
	<-errCh
}

// --- Shutdown behavior tests ---

func TestApp_Shutdown_GracefulDrain(t *testing.T) {
	host, port, addr := freePort(t)

	app := mustNew(t, credo.WithAddr(host, port))

	var requestCompleted atomic.Bool
	started := make(chan struct{})

	app.GET("/slow", func(ctx *credo.Context) error {
		close(started)
		time.Sleep(200 * time.Millisecond)
		requestCompleted.Store(true)
		return ctx.Response().Text(200, "done")
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Start an in-flight request.
	var reqErr error
	done := make(chan struct{})
	go func() {
		resp, err := http.Get("http://" + addr + "/slow")
		reqErr = err
		if resp != nil {
			defer resp.Body.Close()
		}
		close(done)
	}()

	// Wait for the handler to start.
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: handler did not start within 3s")
	}

	// Shutdown while request is in flight.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}

	// The in-flight request should have completed.
	<-done
	if reqErr != nil {
		t.Fatalf("in-flight request error: %v", reqErr)
	}
	if !requestCompleted.Load() {
		t.Error("in-flight request was not completed before shutdown")
	}
	<-errCh
}

func TestApp_Shutdown_OnShutdownHooks(t *testing.T) {
	host, port, _ := freePort(t)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	var order []int
	var mu sync.Mutex
	record := func(n int) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			mu.Lock()
			order = append(order, n)
			mu.Unlock()
			return nil
		}
	}

	app.OnShutdown(record(1))
	app.OnShutdown(record(2))
	app.OnShutdown(record(3))

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	<-errCh

	// Hooks must execute in LIFO order: 3, 2, 1.
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 {
		t.Fatalf("expected 3 hooks called, got %d", len(order))
	}
	expected := []int{3, 2, 1}
	for i, v := range expected {
		if order[i] != v {
			t.Errorf("hook order[%d] = %d, want %d", i, order[i], v)
		}
	}
}

func TestApp_Shutdown_HookErrors(t *testing.T) {
	host, port, _ := freePort(t)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	errA := fmt.Errorf("hook-a failed")
	errB := fmt.Errorf("hook-b failed")

	app.OnShutdown(func(ctx context.Context) error { return errA })
	app.OnShutdown(func(ctx context.Context) error { return nil }) // succeeds
	app.OnShutdown(func(ctx context.Context) error { return errB })

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	shutdownErr := app.Shutdown(ctx)
	<-errCh

	if shutdownErr == nil {
		t.Fatal("Shutdown() should return joined errors")
	}
	if !errors.Is(shutdownErr, errA) {
		t.Errorf("Shutdown() error should contain errA: %v", shutdownErr)
	}
	if !errors.Is(shutdownErr, errB) {
		t.Errorf("Shutdown() error should contain errB: %v", shutdownErr)
	}
}

// --- Frozen guard tests ---

func TestApp_FrozenPanic_GlobalMiddleware(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(ctx *credo.Context) error { return nil })

	// Trigger compile via ServeHTTP.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from GlobalMiddleware after compile")
		}
	}()
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return next
	})
}

func TestApp_FrozenPanic_GroupMiddleware(t *testing.T) {
	app := mustNew(t)
	g := app.Group("/api")
	g.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from Group.Middleware after compile")
		}
	}()
	g.Middleware(func(next credo.Handler) credo.Handler {
		return next
	})
}

func TestApp_FrozenPanic_RouteMiddleware(t *testing.T) {
	app := mustNew(t)
	route := app.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from Route.Middleware after compile")
		}
	}()
	route.Middleware(func(next credo.Handler) credo.Handler {
		return next
	})
}

func TestApp_FrozenPanic_RouteRegistration(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from route registration after compile")
		}
	}()
	app.GET("/y", func(ctx *credo.Context) error { return nil })
}

func TestApp_FrozenPanic_Mount(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from Mount after compile")
		}
	}()
	app.Mount("/api", http.NewServeMux())
}

func TestApp_FrozenPanic_StatusHandler(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from StatusHandler after compile")
		}
	}()
	app.StatusHandler(404, func(ctx *credo.Context) error { return nil })
}

func TestApp_FrozenPanic_SetErrorRenderer(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from SetErrorRenderer after compile")
		}
	}()
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {})
}

func TestApp_FrozenPanic_SetMeta(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from SetMeta after compile")
		}
	}()
	app.SetMeta("key", "value")
}

func TestApp_FrozenPanic_OnShutdown(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from OnShutdown after compile")
		}
	}()
	app.OnShutdown(func(ctx context.Context) error { return nil })
}

func TestApp_FrozenPanic_RouteName(t *testing.T) {
	app := mustNew(t)
	route := app.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from Route.Name after compile")
		}
	}()
	route.Name("late-name")
}

func TestApp_FrozenPanic_RouteSetMeta(t *testing.T) {
	app := mustNew(t)
	route := app.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from Route.SetMeta after compile")
		}
	}()
	route.SetMeta("key", "value")
}

func TestApp_FrozenPanic_GroupSetMeta(t *testing.T) {
	app := mustNew(t)
	g := app.Group("/api")
	g.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from Group.SetMeta after compile")
		}
	}()
	g.SetMeta("key", "value")
}

func TestApp_FrozenPanic_GroupRemoveMeta(t *testing.T) {
	app := mustNew(t)
	g := app.Group("/api")
	g.SetMeta("key", "value")
	g.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from Group.RemoveMeta after compile")
		}
	}()
	g.RemoveMeta("key")
}

// --- P1-3: State rollback on run failure ---

func TestApp_Run_ListenFailure_RollsBackState(t *testing.T) {
	// Occupy a port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	// Run should fail because the port is occupied
	err = app.Run()
	if err == nil {
		t.Fatal("Run() should fail when port is occupied")
	}

	// State should be rolled back to "building"
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q after failed Run, want %q", got, "building")
	}
}

func TestApp_RunTLS_ListenFailure_RollsBackState(t *testing.T) {
	// Occupy a port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	// RunTLS should fail (bad cert files + occupied port)
	err = app.RunTLS("nonexistent.crt", "nonexistent.key")
	if err == nil {
		t.Fatal("RunTLS() should fail")
	}

	// State should be rolled back to "building"
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q after failed RunTLS, want %q", got, "building")
	}
}

// --- Container integration pattern ---

func TestApp_OnShutdown_IntegrationPattern(t *testing.T) {
	// Simulate a service that implements a Shutdown method,
	// registered via OnShutdown for lifecycle integration.
	type mockDB struct {
		closed atomic.Bool
	}

	host, port, _ := freePort(t)

	db := &mockDB{}
	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	app.OnShutdown(func(ctx context.Context) error {
		db.closed.Store(true)
		return nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	<-errCh

	if !db.closed.Load() {
		t.Error("mock DB was not closed during shutdown")
	}
}

// --- Context tests ---

func TestApp_Context_BeforeRun(t *testing.T) {
	app := mustNew(t)
	ctx := app.Context()
	if ctx == nil {
		t.Fatal("Context() returned nil before Run")
	}
	// Should return context.Background() before Run.
	select {
	case <-ctx.Done():
		t.Fatal("context should not be cancelled before Run")
	default:
	}
}

func TestApp_Context_DuringRunning(t *testing.T) {
	host, port, _ := freePort(t)
	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	appCtx := app.Context()
	select {
	case <-appCtx.Done():
		t.Fatal("app context should not be cancelled while running")
	default:
	}

	stopCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(stopCtx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	<-errCh
}

func TestApp_Context_CancelledOnShutdown(t *testing.T) {
	host, port, _ := freePort(t)
	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Capture the app context while running.
	appCtx := app.Context()

	stopCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(stopCtx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	<-errCh

	// The app context must be cancelled after Shutdown.
	select {
	case <-appCtx.Done():
		// good
	default:
		t.Fatal("app context should be cancelled after Shutdown")
	}
}

func TestApp_Shutdown_HookReceivesContext(t *testing.T) {
	host, port, _ := freePort(t)
	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	var hookCtx context.Context
	app.OnShutdown(func(ctx context.Context) error {
		hookCtx = ctx
		return nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	stopCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(stopCtx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	<-errCh

	if hookCtx == nil {
		t.Fatal("hook did not receive a context")
	}
	// The hook's context should have a deadline (from WithTimeout).
	if _, ok := hookCtx.Deadline(); !ok {
		t.Error("expected hook context to have a deadline")
	}
}

// --- OnStart hook tests ---

func TestApp_OnStart_FIFOOrder(t *testing.T) {
	host, port, _ := freePort(t)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	var order []int
	var mu sync.Mutex
	record := func(n int) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			mu.Lock()
			order = append(order, n)
			mu.Unlock()
			return nil
		}
	}

	app.OnStart(record(1))
	app.OnStart(record(2))
	app.OnStart(record(3))

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	<-errCh

	// Hooks must execute in FIFO order: 1, 2, 3.
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 {
		t.Fatalf("expected 3 hooks called, got %d", len(order))
	}
	expected := []int{1, 2, 3}
	for i, v := range expected {
		if order[i] != v {
			t.Errorf("hook order[%d] = %d, want %d", i, order[i], v)
		}
	}
}

func TestApp_OnStart_ErrorPreventsServing(t *testing.T) {
	host, port, _ := freePort(t)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	hookErr := fmt.Errorf("startup failed")
	app.OnStart(func(ctx context.Context) error {
		return hookErr
	})

	err := app.Run()
	if err == nil {
		t.Fatal("Run() should return error when OnStart hook fails")
	}
	if !errors.Is(err, hookErr) {
		t.Errorf("Run() error should wrap hookErr: got %v", err)
	}

	// State should be rolled back to "building".
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q after failed OnStart, want %q", got, "building")
	}
}

func TestApp_OnStart_ErrorStopsAtFirst(t *testing.T) {
	host, port, _ := freePort(t)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	var hook3Called atomic.Bool
	app.OnStart(func(ctx context.Context) error { return nil })                          // hook 0: ok
	app.OnStart(func(ctx context.Context) error { return fmt.Errorf("fail") })           // hook 1: fail
	app.OnStart(func(ctx context.Context) error { hook3Called.Store(true); return nil }) // hook 2: should not run

	_ = app.Run()

	if hook3Called.Load() {
		t.Error("hook 3 should not have been called after hook 2 failed")
	}
}

func TestApp_OnStart_ReceivesAppContext(t *testing.T) {
	host, port, _ := freePort(t)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	var hookCtx context.Context
	app.OnStart(func(ctx context.Context) error {
		hookCtx = ctx
		return nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if hookCtx == nil {
		t.Fatal("OnStart hook did not receive a context")
	}
	// The context should not be cancelled (app is running).
	select {
	case <-hookCtx.Done():
		t.Fatal("OnStart hook context should not be cancelled")
	default:
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	<-errCh
}

func TestApp_OnStart_AddrAvailable(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	var hookAddr net.Addr
	app.OnStart(func(ctx context.Context) error {
		hookAddr = app.Addr()
		return nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if hookAddr == nil {
		t.Fatal("app.Addr() returned nil inside OnStart hook")
	}
	_, portStr, err := net.SplitHostPort(hookAddr.String())
	if err != nil {
		t.Fatalf("failed to parse hook addr %q: %v", hookAddr, err)
	}
	p, _ := strconv.Atoi(portStr)
	if p == 0 {
		t.Error("expected non-zero port from app.Addr() inside OnStart hook")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	<-errCh
}

func TestApp_FrozenPanic_OnStart(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	app.ServeHTTP(w, r)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from OnStart after compile")
		}
	}()
	app.OnStart(func(ctx context.Context) error { return nil })
}

func TestApp_OnStart_IntegrationPattern(t *testing.T) {
	// Simulate service discovery: register on start, deregister on shutdown.
	type serviceRegistry struct {
		registered   atomic.Bool
		deregistered atomic.Bool
	}

	host, port, _ := freePort(t)
	reg := &serviceRegistry{}

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	app.OnStart(func(ctx context.Context) error {
		reg.registered.Store(true)
		return nil
	})
	app.OnShutdown(func(ctx context.Context) error {
		reg.deregistered.Store(true)
		return nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !reg.registered.Load() {
		t.Error("service was not registered during startup")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	<-errCh

	if !reg.deregistered.Load() {
		t.Error("service was not deregistered during shutdown")
	}
}

// --- Addr tests ---

func TestApp_Addr_BeforeRun(t *testing.T) {
	app := mustNew(t)
	if addr := app.Addr(); addr != nil {
		t.Errorf("Addr() = %v before Run, want nil", addr)
	}
}

func TestApp_Addr_DuringRun(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	addr := app.Addr()
	if addr == nil {
		t.Fatal("Addr() = nil during running, want non-nil")
	}
	_, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatalf("failed to parse addr %q: %v", addr, err)
	}
	p, _ := strconv.Atoi(portStr)
	if p == 0 {
		t.Error("expected non-zero port from Addr() during running")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	<-errCh
}

func TestApp_Addr_AfterShutdown(t *testing.T) {
	host, port, _ := freePort(t)

	app := mustNew(t, credo.WithAddr(host, port))
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "pong")
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	<-errCh

	if addr := app.Addr(); addr != nil {
		t.Errorf("Addr() = %v after shutdown, want nil", addr)
	}
}

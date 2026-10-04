package credo_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// callRecovering runs fn and fails the test — instead of crashing the test
// binary — when fn panics, which is what an unrecovered hook panic did.
func callRecovering(t *testing.T, name string, fn func() error) error {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", name, r)
		}
	}()
	return fn()
}

// hookPanicRecords returns the log records with the given message.
func hookPanicRecords(t *testing.T, logs *syncBuffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range parseJSONLines(t, []byte(logs.String())) {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// assertHookPanicRecord checks that exactly one record with msg was written
// and that it names the hook and carries the stack.
func assertHookPanicRecord(t *testing.T, logs *syncBuffer, msg string, index int, panicText string) {
	t.Helper()
	recs := hookPanicRecords(t, logs, msg)
	if len(recs) != 1 {
		t.Fatalf("got %d %q records, want 1:\n%s", len(recs), msg, logs.String())
	}
	rec := recs[0]
	if rec["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", rec["level"])
	}
	if got, _ := rec["hook_index"].(float64); int(got) != index {
		t.Errorf("hook_index = %v, want %d", rec["hook_index"], index)
	}
	if got, _ := rec["panic"].(string); !strings.Contains(got, panicText) {
		t.Errorf("panic = %v, want it to contain %q", rec["panic"], panicText)
	}
	if stack, _ := rec["stack"].(string); !strings.Contains(stack, "goroutine") {
		t.Errorf("stack = %q, want a goroutine stack", stack)
	}
}

// TestApp_OnStart_Panic_IsTheHooksError: a panic in an OnStart hook is that
// hook's error. Run returns it after the same teardown a returned error gets,
// instead of unwinding past the teardown with the listener still bound and
// the App stuck in starting.
func TestApp_OnStart_Panic_IsTheHooksError(t *testing.T) {
	host, port, addr := freePort(t)
	logs := &syncBuffer{}
	app := mustNew(t, credo.WithAddr(host, port),
		credo.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))

	var order []string
	app.MustProvideValue[*diShutdownTracker](&diShutdownTracker{order: &order, name: "di:svc"})

	var laterRan atomic.Bool
	app.OnStart(func(context.Context) error { return nil })
	app.OnStart(func(context.Context) error { panic("start boom") })
	app.OnStart(func(context.Context) error { laterRan.Store(true); return nil })
	app.OnShutdown(func(context.Context) error { order = append(order, "onShutdown"); return nil })

	err := callRecovering(t, "Run", app.Run)
	if err == nil {
		t.Fatal("Run() = nil, want the panicking hook's error")
	}
	for _, want := range []string{"OnStart hook [1]", "start boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run() error %q does not contain %q", err, want)
		}
	}
	if laterRan.Load() {
		t.Error("a later OnStart hook ran after the panic")
	}
	if want := []string{"di:svc", "onShutdown"}; !slices.Equal(order, want) {
		t.Errorf("teardown order = %v, want %v", order, want)
	}
	if got := app.State(); got != "stopped" {
		t.Errorf("State() = %q, want %q", got, "stopped")
	}
	ln, lerr := net.Listen("tcp", addr)
	if lerr != nil {
		t.Fatalf("the listener was not released: %v", lerr)
	}
	ln.Close()

	assertHookPanicRecord(t, logs, "credo: OnStart hook panic", 1, "start boom")
}

// TestApp_OnShutdown_Panic_DoesNotSkipTheRemainingHooks: a panic in an
// OnShutdown hook is that hook's error. The remaining hooks still run, the
// App reaches stopped and Shutdown returns the joined error.
func TestApp_OnShutdown_Panic_DoesNotSkipTheRemainingHooks(t *testing.T) {
	host, port, _ := freePort(t)
	logs := &syncBuffer{}
	app := mustNew(t, credo.WithAddr(host, port),
		credo.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))

	var (
		mu    sync.Mutex
		order []string
	)
	record := func(name string) func(context.Context) error {
		return func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
			return nil
		}
	}
	errBoom := errors.New("shutdown boom")
	app.OnShutdown(record("registered first"))
	app.OnShutdown(func(context.Context) error { panic(errBoom) })
	app.OnShutdown(record("registered last"))

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !app.IsRunning() {
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	shutdownErr := callRecovering(t, "Shutdown", func() error { return app.Shutdown(ctx) })
	<-errCh

	if !errors.Is(shutdownErr, errBoom) {
		t.Errorf("Shutdown() = %v, want it to wrap the panic value", shutdownErr)
	}
	mu.Lock()
	got := slices.Clone(order)
	mu.Unlock()
	// LIFO: the hook registered after the panicking one runs before it, the
	// one registered before it runs after it.
	if want := []string{"registered last", "registered first"}; !slices.Equal(got, want) {
		t.Errorf("hooks run = %v, want %v", got, want)
	}
	if state := app.State(); state != "stopped" {
		t.Errorf("State() = %q, want %q", state, "stopped")
	}

	assertHookPanicRecord(t, logs, "credo: OnShutdown hook panic", 1, "shutdown boom")
}

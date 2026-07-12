package credo_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

type drainTestResource struct {
	alive         atomic.Bool
	shutdownCalls atomic.Int32
}

func (r *drainTestResource) Shutdown(context.Context) error {
	r.shutdownCalls.Add(1)
	r.alive.Store(false)
	return nil
}

func TestApp_OnDrain_NilPanics(t *testing.T) {
	app := mustNew(t)
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("OnDrain(nil) did not panic")
		}
	}()
	app.OnDrain(nil)
}

func TestApp_FrozenPanic_OnDrain(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(*credo.Context) error { return nil })
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("OnDrain after compile did not panic")
		}
	}()
	app.OnDrain(func(context.Context) error { return nil })
}

func TestApp_OnDrain_OverlapsHTTPAndOtherHooks(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0), credo.WithoutAccessLog())
	handlerStarted := make(chan struct{})
	hookAStarted := make(chan struct{})
	hookBStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	releaseHooks := make(chan struct{})
	var handlerOnce sync.Once
	var releaseHandlerOnce, releaseHooksOnce sync.Once

	app.GET("/block", func(ctx *credo.Context) error {
		handlerOnce.Do(func() { close(handlerStarted) })
		<-releaseHandler
		return ctx.Response().NoContent(http.StatusNoContent)
	})
	app.OnDrain(func(context.Context) error {
		close(hookAStarted)
		<-releaseHooks
		return nil
	})
	app.OnDrain(func(context.Context) error {
		close(hookBStarted)
		<-releaseHooks
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitRunning(t, app)
	t.Cleanup(func() {
		releaseHooksOnce.Do(func() { close(releaseHooks) })
		releaseHandlerOnce.Do(func() { close(releaseHandler) })
		if app.IsRunning() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = app.Shutdown(ctx)
		}
	})

	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + app.Addr().String() + "/block")
		if resp != nil {
			resp.Body.Close()
		}
		requestDone <- err
	}()
	waitOnDrainSignal(t, handlerStarted, "HTTP handler")

	shutdownDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() { shutdownDone <- app.Shutdown(ctx) }()

	waitOnDrainSignal(t, hookAStarted, "first drain hook")
	waitOnDrainSignal(t, hookBStarted, "second drain hook")
	assertOnDrainPending(t, shutdownDone, "hooks and HTTP handler are blocked")

	releaseHooksOnce.Do(func() { close(releaseHooks) })
	assertOnDrainPending(t, shutdownDone, "HTTP handler remains blocked")
	releaseHandlerOnce.Do(func() { close(releaseHandler) })

	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	if err := <-requestDone; err != nil {
		t.Fatalf("request error: %v", err)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Run() error: %v", err)
	}
}

func TestApp_OnDrain_PanicIsIsolatedAndIdentified(t *testing.T) {
	logger, logs := newTestLogger(t)
	app := mustNew(t,
		credo.WithAddr("127.0.0.1", 0),
		credo.WithLogger(logger),
		credo.WithoutAccessLog(),
	)
	panicCause := errors.New("drain panic")
	var healthyHookCalled atomic.Bool
	app.OnDrain(func(context.Context) error {
		panic(panicCause)
	})
	app.OnDrain(func(context.Context) error {
		healthyHookCalled.Store(true)
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitRunning(t, app)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := app.Shutdown(ctx)
	if !errors.Is(err, panicCause) {
		t.Fatalf("Shutdown() error = %v, want panic cause", err)
	}
	if !healthyHookCalled.Load() {
		t.Fatal("panic in one drain hook prevented another hook from running")
	}
	if !strings.Contains(err.Error(), "OnDrain hook [0]") ||
		!strings.Contains(err.Error(), "lifecycle_on_drain_test.go") {
		t.Errorf("Shutdown() error lacks hook identity: %v", err)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	var panicLog map[string]any
	for _, entry := range parseJSONLines(t, logs.Bytes()) {
		if entry["msg"] == "credo: OnDrain hook panic" {
			panicLog = entry
			break
		}
	}
	if panicLog == nil {
		t.Fatal("missing structured drain panic log")
	}
	if panicLog["hook_index"] != float64(0) {
		t.Errorf("panic hook_index = %v, want 0", panicLog["hook_index"])
	}
	if source, _ := panicLog["hook_source"].(string); !strings.Contains(source, "lifecycle_on_drain_test.go") {
		t.Errorf("panic hook_source = %q, want registration source", source)
	}
	if stack, _ := panicLog["stack"].(string); stack == "" {
		t.Error("panic log has no stack")
	}
}

func TestApp_OnDrain_IgnoredCancellationIsReportedAndAbandoned(t *testing.T) {
	logger, logs := newTestLogger(t)
	app := mustNew(t,
		credo.WithAddr("127.0.0.1", 0),
		credo.WithLogger(logger),
		credo.WithoutAccessLog(),
	)
	hookStarted := make(chan struct{})
	hookReturned := make(chan struct{})
	releaseHook := make(chan struct{})
	var releaseOnce sync.Once
	var hookCalls atomic.Int32
	app.OnDrain(func(context.Context) error {
		hookCalls.Add(1)
		close(hookStarted)
		<-releaseHook
		close(hookReturned)
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitRunning(t, app)
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseHook) }) })

	shutdownCtx, cancel := context.WithCancel(t.Context())
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- app.Shutdown(shutdownCtx) }()
	waitOnDrainSignal(t, hookStarted, "cancellation-ignoring hook")
	cancel()

	var shutdownErr error
	select {
	case shutdownErr = <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown() waited indefinitely for a cancellation-ignoring hook")
	}
	if !errors.Is(shutdownErr, context.Canceled) {
		t.Fatalf("Shutdown() error = %v, want context.Canceled", shutdownErr)
	}
	if !strings.Contains(shutdownErr.Error(), "OnDrain hook [0]") ||
		!strings.Contains(shutdownErr.Error(), "lifecycle_on_drain_test.go") {
		t.Errorf("incomplete error lacks hook identity: %v", shutdownErr)
	}
	if got := app.State(); got != "stopped" {
		t.Errorf("State() = %q after degraded drain, want stopped", got)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	releaseOnce.Do(func() { close(releaseHook) })
	waitOnDrainSignal(t, hookReturned, "late hook return")
	if got := app.State(); got != "stopped" {
		t.Errorf("late hook mutated State() to %q", got)
	}
	if hookCalls.Load() != 1 {
		t.Errorf("drain hook calls = %d, want 1", hookCalls.Load())
	}
	if err := app.Shutdown(t.Context()); err == nil {
		t.Fatal("repeated Shutdown() after degraded drain returned nil")
	}

	var incompleteLog map[string]any
	for _, entry := range parseJSONLines(t, logs.Bytes()) {
		if entry["msg"] == "credo: drain task incomplete" && entry["hook_index"] == float64(0) {
			incompleteLog = entry
			break
		}
	}
	if incompleteLog == nil {
		t.Fatal("missing structured incomplete-hook log")
	}
	if source, _ := incompleteLog["hook_source"].(string); !strings.Contains(source, "lifecycle_on_drain_test.go") {
		t.Errorf("incomplete hook_source = %q, want registration source", source)
	}
}

func TestApp_OnDrain_CompletesBeforeDIShutdown(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0), credo.WithoutAccessLog())
	resource := &drainTestResource{}
	resource.alive.Store(true)
	app.MustProvideValue[*drainTestResource](resource)
	drainStarted := make(chan struct{})
	releaseDrain := make(chan struct{})
	var releaseOnce sync.Once
	var aliveDuringDrain atomic.Bool
	var lifecycleCancelledDuringDrain atomic.Bool
	var stoppedBeforeOnShutdown atomic.Bool
	var lifecycleCtx context.Context
	app.OnStart(func(ctx context.Context) error {
		lifecycleCtx = ctx
		return nil
	})
	app.OnDrain(func(context.Context) error {
		aliveDuringDrain.Store(resource.alive.Load())
		select {
		case <-lifecycleCtx.Done():
			lifecycleCancelledDuringDrain.Store(true)
		default:
		}
		close(drainStarted)
		<-releaseDrain
		return nil
	})
	app.OnShutdown(func(context.Context) error {
		stoppedBeforeOnShutdown.Store(!resource.alive.Load())
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitRunning(t, app)
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseDrain) }) })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- app.Shutdown(ctx) }()
	waitOnDrainSignal(t, drainStarted, "DI-order drain hook")
	if !resource.alive.Load() || !aliveDuringDrain.Load() {
		t.Fatal("DI resource shut down before drain hook completed")
	}
	if !lifecycleCancelledDuringDrain.Load() {
		t.Fatal("application lifecycle context was live when drain hook began")
	}
	if resource.shutdownCalls.Load() != 0 {
		t.Fatalf("DI Shutdown calls during drain = %d, want 0", resource.shutdownCalls.Load())
	}

	releaseOnce.Do(func() { close(releaseDrain) })
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	if resource.alive.Load() {
		t.Fatal("DI resource remained alive after successful shutdown")
	}
	if resource.shutdownCalls.Load() != 1 {
		t.Errorf("DI Shutdown calls = %d, want 1", resource.shutdownCalls.Load())
	}
	if !stoppedBeforeOnShutdown.Load() {
		t.Fatal("OnShutdown ran before DI shutdown")
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Run() error: %v", err)
	}
}

func TestApp_OnDrain_RunsAfterFailedStartup(t *testing.T) {
	for _, tc := range []struct {
		name                string
		failBeforeSubsystem bool
	}{
		{name: "before subsystem start", failBeforeSubsystem: true},
		{name: "after subsystem start", failBeforeSubsystem: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := mustNew(t, credo.WithAddr("127.0.0.1", 0), credo.WithoutAccessLog())
			startupErr := errors.New("startup failed")
			var subsystemStarted atomic.Bool
			var drainCalls atomic.Int32
			var observedStarted atomic.Bool
			app.OnDrain(func(context.Context) error {
				drainCalls.Add(1)
				observedStarted.Store(subsystemStarted.Load())
				return nil
			})
			if tc.failBeforeSubsystem {
				app.OnStart(func(context.Context) error { return startupErr })
				app.OnStart(func(context.Context) error {
					subsystemStarted.Store(true)
					return nil
				})
			} else {
				app.OnStart(func(context.Context) error {
					subsystemStarted.Store(true)
					return nil
				})
				app.OnStart(func(context.Context) error { return startupErr })
			}

			err := app.Run()
			if !errors.Is(err, startupErr) {
				t.Fatalf("Run() error = %v, want startup cause", err)
			}
			if drainCalls.Load() != 1 {
				t.Errorf("OnDrain calls = %d, want 1", drainCalls.Load())
			}
			if observedStarted.Load() != !tc.failBeforeSubsystem {
				t.Errorf("OnDrain observed subsystemStarted = %v, want %v",
					observedStarted.Load(), !tc.failBeforeSubsystem)
			}
			if got := app.State(); got != "stopped" {
				t.Errorf("State() = %q after failed startup, want stopped", got)
			}
			if err := app.Shutdown(t.Context()); err == nil {
				t.Fatal("Shutdown() after failed-startup teardown returned nil")
			}
			if drainCalls.Load() != 1 {
				t.Errorf("repeated Shutdown changed OnDrain calls to %d", drainCalls.Load())
			}
		})
	}
}

func TestApp_OnDrain_ConcurrentShutdownRunsExactlyOnce(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0), credo.WithoutAccessLog())
	drainStarted := make(chan struct{})
	releaseDrain := make(chan struct{})
	var releaseOnce sync.Once
	var drainCalls atomic.Int32
	app.OnDrain(func(context.Context) error {
		drainCalls.Add(1)
		close(drainStarted)
		<-releaseDrain
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitRunning(t, app)
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseDrain) }) })

	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range callers {
		go func() {
			<-start
			results <- app.Shutdown(ctx)
		}()
	}
	close(start)
	waitOnDrainSignal(t, drainStarted, "concurrent shutdown owner")
	releaseOnce.Do(func() { close(releaseDrain) })

	var successes int
	for range callers {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Errorf("successful Shutdown callers = %d, want 1", successes)
	}
	if drainCalls.Load() != 1 {
		t.Errorf("OnDrain calls = %d, want 1", drainCalls.Load())
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Run() error: %v", err)
	}
}

func waitOnDrainSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for %s", label)
	}
}

func assertOnDrainPending(t *testing.T, ch <-chan error, reason string) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("Shutdown() returned early while %s: %v", reason, err)
	case <-time.After(50 * time.Millisecond):
	}
}

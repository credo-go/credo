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
	"github.com/credo-go/credo/worker"
)

func TestApp_OnPreDrain_NilPanics(t *testing.T) {
	app := mustNew(t)
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("OnPreDrain(nil) did not panic")
		}
	}()
	app.OnPreDrain(nil)
}

func TestApp_FrozenPanic_OnPreDrain(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(*credo.Context) error { return nil })
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("OnPreDrain after compile did not panic")
		}
	}()
	app.OnPreDrain(func(context.Context) error { return nil })
}

func TestApp_OnPreDrainPrecedesLifecycleCancellationAndDI(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0), credo.WithoutAccessLog())
	resource := &drainTestResource{}
	resource.alive.Store(true)
	app.MustProvideValue[*drainTestResource](resource)
	app.UseHealth()

	workerContext := make(chan context.Context, 1)
	if err := worker.Register(app, worker.Func("pre-drain-order", func(ctx context.Context) error {
		workerContext <- ctx
		<-ctx.Done()
		return ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}

	preDrainStarted := make(chan struct{})
	releasePreDrain := make(chan struct{})
	onDrainStarted := make(chan struct{})
	var releaseOnce sync.Once
	var runningWorkerContext context.Context
	app.OnPreDrain(func(context.Context) error {
		if runningWorkerContext.Err() != nil {
			return errors.New("worker context was cancelled before OnPreDrain")
		}
		if !resource.alive.Load() {
			return errors.New("DI resource was shut down before OnPreDrain")
		}
		close(preDrainStarted)
		<-releasePreDrain
		return nil
	})
	app.OnDrain(func(context.Context) error {
		select {
		case <-runningWorkerContext.Done():
		default:
			return errors.New("worker context remained live during OnDrain")
		}
		if !resource.alive.Load() {
			return errors.New("DI resource was shut down before OnDrain")
		}
		close(onDrainStarted)
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitRunning(t, app)
	select {
	case runningWorkerContext = <-workerContext:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	t.Cleanup(func() { releaseOnce.Do(func() { close(releasePreDrain) }) })

	shutdownDone := make(chan error, 1)
	shutdownCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() { shutdownDone <- app.Shutdown(shutdownCtx) }()
	waitOnDrainSignal(t, preDrainStarted, "pre-drain hook")
	if app.State() != "stopping" {
		t.Fatalf("State() = %q while OnPreDrain is blocked, want stopping", app.State())
	}
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + app.Addr().String() + "/ready")
	if err != nil {
		t.Fatalf("GET /ready during OnPreDrain: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /ready status during OnPreDrain = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	select {
	case <-runningWorkerContext.Done():
		t.Fatal("worker context was cancelled while OnPreDrain was blocked")
	default:
	}
	if !resource.alive.Load() || resource.shutdownCalls.Load() != 0 {
		t.Fatal("DI resource shut down while OnPreDrain was blocked")
	}
	select {
	case <-onDrainStarted:
		t.Fatal("OnDrain started before OnPreDrain completed")
	default:
	}

	releaseOnce.Do(func() { close(releasePreDrain) })
	waitOnDrainSignal(t, onDrainStarted, "OnDrain hook")
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	if resource.alive.Load() || resource.shutdownCalls.Load() != 1 {
		t.Fatalf("DI resource alive/calls = %t/%d, want false/1", resource.alive.Load(), resource.shutdownCalls.Load())
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Run() error: %v", err)
	}
}

func TestApp_OnPreDrainHooksOverlap(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0), credo.WithoutAccessLog())
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	app.OnPreDrain(func(context.Context) error {
		close(firstStarted)
		<-release
		return nil
	})
	app.OnPreDrain(func(context.Context) error {
		close(secondStarted)
		<-release
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitRunning(t, app)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	shutdownDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() { shutdownDone <- app.Shutdown(ctx) }()
	waitOnDrainSignal(t, firstStarted, "first pre-drain hook")
	waitOnDrainSignal(t, secondStarted, "second pre-drain hook")
	releaseOnce.Do(func() { close(release) })
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if err := <-runErr; err != nil {
		t.Fatal(err)
	}
}

func TestApp_OnPreDrainRunsDuringFailedStartupBeforeCancellation(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0), credo.WithoutAccessLog())
	resource := &drainTestResource{}
	resource.alive.Store(true)
	app.MustProvideValue[*drainTestResource](resource)
	workerContext := make(chan context.Context, 1)
	if err := worker.Register(app, worker.Func("startup-failure-worker", func(ctx context.Context) error {
		workerContext <- ctx
		<-ctx.Done()
		return ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	startupErr := errors.New("injected startup failure")
	app.OnStart(func(context.Context) error { return startupErr })
	var calls atomic.Int32
	app.OnPreDrain(func(ctx context.Context) error {
		calls.Add(1)
		if ctx.Err() != nil {
			return errors.New("shutdown deadline ended before startup-failure pre-drain")
		}
		var running context.Context
		select {
		case running = <-workerContext:
		case <-ctx.Done():
			return ctx.Err()
		}
		if running.Err() != nil {
			return errors.New("worker context was cancelled before startup-failure pre-drain")
		}
		if !resource.alive.Load() {
			return errors.New("DI resource was shut down before startup-failure pre-drain")
		}
		return nil
	})

	err := app.RunContext(t.Context())
	if !errors.Is(err, startupErr) {
		t.Fatalf("RunContext() error = %v, want startup error", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("OnPreDrain calls = %d, want 1", calls.Load())
	}
	if resource.alive.Load() || resource.shutdownCalls.Load() != 1 {
		t.Fatalf("DI resource alive/calls = %t/%d, want false/1", resource.alive.Load(), resource.shutdownCalls.Load())
	}
	if app.State() != "stopped" {
		t.Fatalf("State() = %q, want stopped", app.State())
	}
}

func TestApp_OnPreDrainErrorsAndPanicsContinueTeardown(t *testing.T) {
	logger, logs := newTestLogger(t)
	app := mustNew(t,
		credo.WithAddr("127.0.0.1", 0),
		credo.WithLogger(logger),
		credo.WithoutAccessLog(),
	)
	resource := &drainTestResource{}
	resource.alive.Store(true)
	app.MustProvideValue[*drainTestResource](resource)
	var lifecycleCtx context.Context
	app.OnStart(func(ctx context.Context) error {
		lifecycleCtx = ctx
		return nil
	})
	hookErr := errors.New("pre-drain error")
	panicCause := errors.New("pre-drain panic")
	app.OnPreDrain(func(context.Context) error { return hookErr })
	app.OnPreDrain(func(context.Context) error { panic(panicCause) })
	var healthyHookCalls atomic.Int32
	app.OnPreDrain(func(context.Context) error {
		healthyHookCalls.Add(1)
		return nil
	})
	var onDrainCalls, onShutdownCalls atomic.Int32
	app.OnDrain(func(context.Context) error {
		onDrainCalls.Add(1)
		select {
		case <-lifecycleCtx.Done():
			return nil
		default:
			return errors.New("lifecycle was not cancelled before OnDrain")
		}
	})
	app.OnShutdown(func(context.Context) error {
		onShutdownCalls.Add(1)
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitRunning(t, app)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := app.Shutdown(ctx)
	if !errors.Is(err, hookErr) || !errors.Is(err, panicCause) {
		t.Fatalf("Shutdown() error = %v, want hook and panic causes", err)
	}
	if !strings.Contains(err.Error(), "OnPreDrain hook [0]") || !strings.Contains(err.Error(), "OnPreDrain hook [1]") {
		t.Fatalf("Shutdown() error lacks pre-drain identities: %v", err)
	}
	if healthyHookCalls.Load() != 1 || onDrainCalls.Load() != 1 || onShutdownCalls.Load() != 1 {
		t.Fatalf("healthy/pre-drain/shutdown calls = %d/%d/%d, want 1/1/1", healthyHookCalls.Load(), onDrainCalls.Load(), onShutdownCalls.Load())
	}
	if resource.alive.Load() || resource.shutdownCalls.Load() != 1 {
		t.Fatalf("DI resource alive/calls = %t/%d, want false/1", resource.alive.Load(), resource.shutdownCalls.Load())
	}
	if err := <-runErr; err != nil {
		t.Fatal(err)
	}

	foundPanicLog := false
	for _, entry := range parseJSONLines(t, logs.Bytes()) {
		if entry["msg"] == "credo: OnPreDrain hook panic" {
			foundPanicLog = true
			if entry["hook_index"] != float64(1) {
				t.Fatalf("panic hook index = %v, want 1", entry["hook_index"])
			}
		}
	}
	if !foundPanicLog {
		t.Fatal("missing structured OnPreDrain panic log")
	}
}

func TestApp_OnPreDrainDeadlineIsReportedButRemainsTeardownBarrier(t *testing.T) {
	logger, logs := newTestLogger(t)
	app := mustNew(t,
		credo.WithAddr("127.0.0.1", 0),
		credo.WithLogger(logger),
		credo.WithoutAccessLog(),
	)
	resource := &drainTestResource{}
	resource.alive.Store(true)
	app.MustProvideValue[*drainTestResource](resource)
	var lifecycleCtx context.Context
	app.OnStart(func(ctx context.Context) error {
		lifecycleCtx = ctx
		return nil
	})
	hookStarted := make(chan struct{})
	deadlineObserved := make(chan struct{})
	hookReturned := make(chan struct{})
	releaseHook := make(chan struct{})
	var releaseOnce sync.Once
	app.OnPreDrain(func(ctx context.Context) error {
		close(hookStarted)
		<-ctx.Done()
		close(deadlineObserved)
		<-releaseHook
		close(hookReturned)
		return nil
	})
	onDrainStarted := make(chan struct{})
	app.OnDrain(func(context.Context) error {
		close(onDrainStarted)
		return nil
	})
	var onShutdownCalls atomic.Int32
	app.OnShutdown(func(context.Context) error {
		onShutdownCalls.Add(1)
		return nil
	})

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()
	waitRunning(t, app)
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseHook) }) })
	shutdownCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- app.Shutdown(shutdownCtx) }()
	waitOnDrainSignal(t, hookStarted, "blocking pre-drain hook")
	waitOnDrainSignal(t, deadlineObserved, "pre-drain deadline")

	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown() returned before the pre-drain barrier completed: %v", err)
	default:
	}
	if app.State() != "stopping" {
		t.Fatalf("State() = %q past deadline with hook blocked, want stopping", app.State())
	}
	select {
	case <-lifecycleCtx.Done():
		t.Fatal("lifecycle context cancelled before over-deadline OnPreDrain returned")
	default:
	}
	if !resource.alive.Load() || resource.shutdownCalls.Load() != 0 {
		t.Fatal("DI resource shut down before over-deadline OnPreDrain returned")
	}
	select {
	case <-onDrainStarted:
		t.Fatal("OnDrain started before over-deadline OnPreDrain returned")
	default:
	}
	if onShutdownCalls.Load() != 0 {
		t.Fatal("OnShutdown ran before over-deadline OnPreDrain returned")
	}

	releaseOnce.Do(func() { close(releaseHook) })
	waitOnDrainSignal(t, hookReturned, "over-deadline pre-drain hook return")
	waitOnDrainSignal(t, onDrainStarted, "post-barrier OnDrain hook")

	shutdownErr := <-shutdownDone
	if !errors.Is(shutdownErr, context.DeadlineExceeded) || !strings.Contains(shutdownErr.Error(), "OnPreDrain hook [0]") {
		t.Fatalf("Shutdown() error = %v, want identified deadline", shutdownErr)
	}
	select {
	case <-lifecycleCtx.Done():
	default:
		t.Fatal("lifecycle context remained live after OnPreDrain returned")
	}
	if app.State() != "stopped" || onShutdownCalls.Load() != 1 {
		t.Fatalf("state/OnShutdown calls = %q/%d, want stopped/1", app.State(), onShutdownCalls.Load())
	}
	if err := <-runErr; err != nil {
		t.Fatal(err)
	}
	if app.State() != "stopped" {
		t.Fatalf("State() = %q after post-barrier teardown, want stopped", app.State())
	}

	foundIncompleteLog := false
	for _, entry := range parseJSONLines(t, logs.Bytes()) {
		if entry["msg"] == "credo: drain task incomplete" && entry["hook_index"] == float64(0) {
			if task, _ := entry["task"].(string); strings.Contains(task, "OnPreDrain hook [0]") {
				foundIncompleteLog = true
			}
		}
	}
	if !foundIncompleteLog {
		t.Fatal("missing structured incomplete OnPreDrain log")
	}
}

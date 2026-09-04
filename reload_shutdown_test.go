package credo_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// closingResource is a DI singleton that records, when it is shut down,
// whether the reload hook that uses it had already returned.
type closingResource struct {
	hookDone        *atomic.Bool
	hookDoneAtClose atomic.Bool
	closed          atomic.Bool
}

func (r *closingResource) Shutdown(context.Context) error {
	r.hookDoneAtClose.Store(r.hookDone.Load())
	r.closed.Store(true)
	return nil
}

// TestReload_CallbacksAreCancelledByShutdown: a reload hook that is running
// when Shutdown begins sees its context cancelled, and the DI resources it
// may be using are torn down only after it returns.
func TestReload_CallbacksAreCancelledByShutdown(t *testing.T) {
	f := newReloadFixture(t, "a: 1\n")
	var hookDone atomic.Bool
	r := &closingResource{hookDone: &hookDone}
	f.app.MustProvideValue(r)

	entered := make(chan struct{})
	f.app.OnReload(func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		if r.closed.Load() {
			return errors.New("resource was already closed while the hook was running")
		}
		hookDone.Store(true)
		return ctx.Err()
	})
	f.start(t)

	reloadErr := make(chan error, 1)
	go func() { reloadErr <- f.app.Reload(context.Background()) }()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-reloadErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Reload = %v, want the lifecycle cancellation reported", err)
		}
		if strings.Contains(err.Error(), "already closed") {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reload did not return after Shutdown")
	}
	if !r.closed.Load() {
		t.Fatal("resource was not shut down")
	}
	if !r.hookDoneAtClose.Load() {
		t.Fatal("resource was shut down before the reload hook returned")
	}
}

// TestShutdown_ReportsReloadStillInFlight: a hook that ignores cancellation
// past the shutdown deadline is reported, and teardown proceeds rather than
// hanging.
func TestShutdown_ReportsReloadStillInFlight(t *testing.T) {
	f := newReloadFixture(t, "a: 1\n")
	entered := make(chan struct{})
	release := make(chan struct{})
	f.app.OnReload(func(context.Context) error {
		close(entered)
		<-release
		return nil
	})
	f.start(t)

	reloadErr := make(chan error, 1)
	go func() { reloadErr <- f.app.Reload(context.Background()) }()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := f.app.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "reload still in flight") {
		t.Fatalf("Shutdown = %v, want the in-flight reload reported at the deadline", err)
	}
	if got := f.app.State(); got != "stopped" {
		t.Fatalf("State() = %q after Shutdown returned, want stopped", got)
	}

	close(release)
	select {
	case err := <-reloadErr:
		if err != nil {
			t.Fatalf("Reload = %v, want nil (the hook itself succeeded)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reload did not return after the hook was released")
	}
}

// TestReload_QueuedCallerAbortsOnShutdown: a Reload queued behind an in-flight
// one does not run once shutdown has begun.
func TestReload_QueuedCallerAbortsOnShutdown(t *testing.T) {
	f := newReloadFixture(t, "a: 1\n")
	var runs atomic.Int32
	entered := make(chan struct{}, 1)
	f.app.OnReload(func(ctx context.Context) error {
		runs.Add(1)
		entered <- struct{}{}
		<-ctx.Done()
		return nil
	})
	f.start(t)

	first := make(chan error, 1)
	go func() { first <- f.app.Reload(context.Background()) }()
	<-entered

	second := make(chan error, 1)
	go func() { second <- f.app.Reload(context.Background()) }()
	time.Sleep(30 * time.Millisecond) // let the second caller queue on the slot

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for name, ch := range map[string]chan error{"first": first, "second": second} {
		select {
		case err := <-ch:
			if name == "second" && (err == nil || !strings.Contains(err.Error(), `expected "running"`)) {
				t.Errorf("queued Reload = %v, want the not-running error", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s Reload did not return", name)
		}
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("hook ran %d times, want 1 (the queued reload must not run after shutdown)", got)
	}
}

// TestReload_WaitingCallerHonoursItsContext: a caller waiting for the
// in-flight reload gives up when its own context ends.
func TestReload_WaitingCallerHonoursItsContext(t *testing.T) {
	f := newReloadFixture(t, "a: 1\n")
	entered := make(chan struct{})
	release := make(chan struct{})
	f.app.OnReload(func(context.Context) error {
		close(entered)
		<-release
		return nil
	})
	f.start(t)

	first := make(chan error, 1)
	go func() { first <- f.app.Reload(context.Background()) }()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := f.app.Reload(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "waiting for the in-flight reload") {
		t.Fatalf("waiting Reload = %v, want its own deadline reported", err)
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Reload = %v", err)
	}
}

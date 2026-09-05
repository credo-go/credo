package worker

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// runApp serves app on an ephemeral listener until the returned shutdown
// function (or Cleanup) stops it, and fails the test if serving errors.
func runApp(t *testing.T, app *credo.App) (shutdown func() error) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- app.ServeContext(context.Background(), l) }()
	awaitCondition(t, "app to start", app.IsRunning)

	var once sync.Once
	var result error
	shutdown = func() error {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result = app.Shutdown(ctx)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("ServeContext: %v", err)
				}
			case <-ctx.Done():
				t.Error("ServeContext did not return after Shutdown")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = shutdown() })
	return shutdown
}

// resource is a DI singleton that records, at the moment it is shut down,
// whether the worker depending on it had already returned.
type resource struct {
	closed            atomic.Bool
	workerDoneAtClose atomic.Bool
	workerDone        *atomic.Bool
}

func (r *resource) Shutdown(context.Context) error {
	r.workerDoneAtClose.Store(r.workerDone.Load())
	r.closed.Store(true)
	return nil
}

// flushingWorker does a short, bounded cleanup after cancellation (the
// "write the last batch" pattern) and records whether the resource it uses
// was still open when it finished.
type flushingWorker struct {
	r                  *resource
	started            chan struct{}
	done               atomic.Bool
	resourceOpenAtExit atomic.Bool
}

func (*flushingWorker) Name() string { return "flusher" }

func (w *flushingWorker) Run(ctx context.Context) error {
	close(w.started)
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond) // bounded cooperative cleanup
	w.resourceOpenAtExit.Store(!w.r.closed.Load())
	w.done.Store(true)
	return nil
}

func TestLifecycle_WorkersFinishBeforeInfrastructureShutdown(t *testing.T) {
	for _, order := range []string{"worker-then-resource", "resource-then-worker"} {
		t.Run(order, func(t *testing.T) {
			app := newTestApp(t)
			w := &flushingWorker{started: make(chan struct{})}
			r := &resource{workerDone: &w.done}
			w.r = r

			register := func() {
				if err := Register(app, w); err != nil {
					t.Fatalf("Register: %v", err)
				}
			}
			if order == "worker-then-resource" {
				register()
				app.MustProvideValue(r)
			} else {
				app.MustProvideValue(r)
				register()
			}

			shutdown := runApp(t, app)
			<-w.started
			if err := shutdown(); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			if !r.closed.Load() {
				t.Fatal("resource was not shut down")
			}
			if !r.workerDoneAtClose.Load() {
				t.Error("resource was shut down before the worker returned")
			}
			if !w.resourceOpenAtExit.Load() {
				t.Error("worker observed a closed resource during its cleanup")
			}
		})
	}
}

func TestLifecycle_ShutdownReportsWorkerThatOutlivesTheDeadline(t *testing.T) {
	app := newTestApp(t)
	release := make(chan struct{})
	started := make(chan struct{})
	if err := Register(app, Func("stubborn", func(context.Context) error {
		close(started)
		<-release
		return nil
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	finalize(t, app)
	pool, err := app.Resolve[*Pool]()
	if err != nil {
		t.Fatal(err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- app.ServeContext(context.Background(), l) }()
	awaitCondition(t, "app to start", app.IsRunning)
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = app.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() = %v, want deadline exceeded", err)
	}
	if !strings.Contains(err.Error(), "OnDrain hook") {
		t.Errorf("Shutdown() = %v, want the worker reported as an incomplete OnDrain task", err)
	}
	if strings.Contains(err.Error(), "shutting down *worker.Pool") {
		t.Errorf("Shutdown() = %v, want no duplicate report from the DI pass", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Errorf("ServeContext: %v", err)
	}
	if err := pool.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown after workers returned = %v, want nil", err)
	}
}

func TestRegister_RejectedAfterFinalize(t *testing.T) {
	t.Run("first registration", func(t *testing.T) {
		app := newTestApp(t)
		if err := app.Finalize(); err != nil {
			t.Fatal(err)
		}
		err := Register(app, Func("late", func(context.Context) error { return nil }))
		requireErrContaining(t, err, "after app.Finalize")
	})
	t.Run("subsequent registration", func(t *testing.T) {
		app := newTestApp(t)
		if err := Register(app, Func("early", func(context.Context) error { return nil })); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if err := app.Finalize(); err != nil {
			t.Fatal(err)
		}
		err := Register(app, Func("late", func(context.Context) error { return nil }))
		requireErrContaining(t, err, "after app.Finalize")
		pool, err := app.Resolve[*Pool]()
		if err != nil {
			t.Fatal(err)
		}
		if n := len(pool.Workers()); n != 1 {
			t.Fatalf("pool has %d workers, want 1 (late worker must not be admitted)", n)
		}
	})
}

func TestRegister_PoolBindingIsProtected(t *testing.T) {
	app := newTestApp(t)
	if err := Register(app, Func("w", func(context.Context) error { return nil })); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, _, err := app.Replace[*Pool](newTestPool()); err == nil {
		t.Fatal("Replace[*Pool] succeeded; the wired pool and the DI binding may now diverge")
	}
}

func TestRegister_RejectsPoolProvidedOutsideRegister(t *testing.T) {
	app := newTestApp(t)
	app.MustProvideValue(&Pool{})
	err := Register(app, Func("w", func(context.Context) error { return nil }))
	requireErrContaining(t, err, "provided outside worker.Register")
}

func TestPoolShutdown_SharedResult(t *testing.T) {
	pool := newTestPool()
	release := make(chan struct{})
	started := make(chan struct{})
	if err := pool.addDefinition(&Definition{name: "w", worker: Func("w", func(context.Context) error {
		close(started)
		<-release
		return nil
	})}); err != nil {
		t.Fatal(err)
	}
	if err := pool.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-started

	// Two callers under an expired budget both see the deadline.
	expired, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	for i := range 2 {
		if err := pool.Shutdown(expired); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown #%d = %v, want deadline exceeded", i+1, err)
		}
	}
	if err := pool.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "already shut down") {
		t.Fatalf("Start after Shutdown = %v, want refusal", err)
	}

	// Once the worker returns, every caller (concurrent or later) gets nil.
	close(release)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if err := pool.Shutdown(context.Background()); err != nil {
				t.Errorf("Shutdown = %v, want nil", err)
			}
		})
	}
	wg.Wait()
}

func TestPoolShutdown_NeverStarted(t *testing.T) {
	if err := newTestPool().Shutdown(context.Background()); err != nil {
		t.Fatalf("managed pool: %v", err)
	}
	if err := (&Pool{}).Shutdown(context.Background()); err != nil {
		t.Fatalf("zero-value pool: %v", err)
	}
}

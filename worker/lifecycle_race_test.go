package worker

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPoolShutdown_CompletedResultIsStable: once every worker has returned,
// Shutdown reports nil regardless of the caller's context state.
func TestPoolShutdown_CompletedResultIsStable(t *testing.T) {
	pool := newTestPool()
	if err := pool.addDefinition(&definition{name: "w", resolve: instance(Func(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}))}); err != nil {
		t.Fatal(err)
	}
	if err := pool.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := pool.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown = %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 100 {
		if err := pool.Shutdown(cancelled); err != nil {
			t.Fatalf("Shutdown #%d with a cancelled ctx = %v, want nil after completion", i+1, err)
		}
	}
}

// TestPoolStart_ShutdownDuringResolutionWins: a Shutdown that arrives while
// Start is blocked inside a worker constructor completes at once — nothing
// has joined the WaitGroup yet — and the Start that resumes afterwards is
// refused and launches nothing.
func TestPoolStart_ShutdownDuringResolutionWins(t *testing.T) {
	app := newTestApp(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	app.MustProvide[*stubWorker[kindA]](func() *stubWorker[kindA] {
		close(entered)
		<-release
		return newStubWorker[kindA]()
	})
	MustRegisterProvided[*stubWorker[kindA]](app, "slow-ctor")
	var byValueRuns atomic.Int32
	MustRegister(app, "by-value", Func(func(ctx context.Context) error {
		byValueRuns.Add(1)
		<-ctx.Done()
		return nil
	}))
	finalize(t, app)
	pool, err := app.Resolve[*Pool]()
	if err != nil {
		t.Fatal(err)
	}

	startErr := make(chan error, 1)
	go func() { startErr <- pool.Start(context.Background()) }()
	<-entered

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err = pool.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown during resolution = %v, want nil (no worker was launched)", err)
	}

	close(release)
	select {
	case err = <-startErr:
		if err == nil || !strings.Contains(err.Error(), "already shut down") {
			t.Fatalf("Start after a concurrent Shutdown = %v, want refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after the constructor was released")
	}

	for _, info := range pool.Workers() {
		if info.Status != StatusIdle {
			t.Errorf("worker %q status = %q, want idle (never published)", info.Name, info.Status)
		}
	}
	pool.wg.Wait()
	if got := byValueRuns.Load(); got != 0 {
		t.Fatalf("by-value worker ran %d times, want 0", got)
	}
	w, err := app.Resolve[*stubWorker[kindA]]()
	if err != nil {
		t.Fatal(err)
	}
	if got := w.runs.Load(); got != 0 {
		t.Fatalf("provided worker ran %d times, want 0", got)
	}
}

// TestPoolStartShutdown_ConcurrentCallsAreOrdered: a Start racing a direct
// Shutdown either is refused or has every worker goroutine joined (and its
// context cancelled) before the wait begins, so a nil Shutdown never leaves a
// worker goroutine running on an uncancelled context.
func TestPoolStartShutdown_ConcurrentCallsAreOrdered(t *testing.T) {
	for range 200 {
		pool := newTestPool()
		if err := pool.addDefinition(&definition{name: "w", resolve: instance(Func(func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		}))}); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		var startErr, shutdownErr error
		wg.Go(func() { startErr = pool.Start(context.Background()) })
		wg.Go(func() { shutdownErr = pool.Shutdown(context.Background()) })
		wg.Wait()

		if shutdownErr != nil {
			t.Fatalf("Shutdown = %v", shutdownErr)
		}
		if startErr != nil {
			continue // refused: nothing was launched
		}
		// Start won the race: every goroutine it launched must already have
		// been waited for, i.e. none may be running past Shutdown's return.
		exited := make(chan struct{})
		go func() { pool.wg.Wait(); close(exited) }()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			t.Fatal("Start succeeded and Shutdown returned nil, but a worker goroutine is still running")
		}
	}
}

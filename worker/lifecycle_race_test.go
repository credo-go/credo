package worker

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestPoolShutdown_CompletedResultIsStable: once every worker has returned,
// Shutdown reports nil regardless of the caller's context state.
func TestPoolShutdown_CompletedResultIsStable(t *testing.T) {
	pool := newTestPool()
	if err := pool.addDefinition(&Definition{name: "w", worker: Func("w", func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})}); err != nil {
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

// TestPoolStartShutdown_ConcurrentCallsAreOrdered: a Start racing a direct
// Shutdown either is refused or has every worker goroutine joined (and its
// context cancelled) before the wait begins, so a nil Shutdown never leaves a
// worker goroutine running on an uncancelled context.
func TestPoolStartShutdown_ConcurrentCallsAreOrdered(t *testing.T) {
	for range 200 {
		pool := newTestPool()
		if err := pool.addDefinition(&Definition{name: "w", worker: Func("w", func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		})}); err != nil {
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

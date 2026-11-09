package worker

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// raceCountingWorker blocks until cancelled and counts its runs.
func raceCountingWorker(runs *atomic.Int32) workerFunc {
	return func(ctx context.Context) error {
		runs.Add(1)
		<-ctx.Done()
		return nil
	}
}

func TestComponent_ShutdownBeforeStart(t *testing.T) {
	var runs atomic.Int32
	c := addWorker(newTestEngine(), continuousDef("w"), raceCountingWorker(&runs))

	// A component that never started shuts down at once, whatever its context.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Shutdown(cancelled); err != nil {
		t.Fatalf("Shutdown before Start = %v, want nil", err)
	}
	err := c.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), `"w"`) || !strings.Contains(err.Error(), "Start after Shutdown") {
		t.Fatalf("Start after Shutdown = %v, want a refusal naming the worker", err)
	}
	if got := runs.Load(); got != 0 {
		t.Errorf("Run called %d times, want 0", got)
	}
	if got := c.r.status(); got != StatusPending {
		t.Errorf("status = %s, want pending", got)
	}
}

func TestComponent_StartTwiceRefused(t *testing.T) {
	var runs atomic.Int32
	c := addWorker(newTestEngine(), continuousDef("w"), raceCountingWorker(&runs))
	startWorker(t, c)
	err := c.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), `"w"`) || !strings.Contains(err.Error(), "Start called twice") {
		t.Fatalf("second Start = %v, want a refusal naming the worker", err)
	}
}

func TestComponent_ConcurrentShutdownsReturnAfterTheLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var returned atomic.Bool
		c := addWorker(newTestEngine(), continuousDef("w"), workerFunc(func(ctx context.Context) error {
			<-ctx.Done()
			<-release // a final flush that outlives the cancellation
			returned.Store(true)
			return nil
		}))
		if err := c.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		// The deadline passing does not make Shutdown return: it waits for Run.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		var done [2]atomic.Bool
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() {
				errs[i] = c.Shutdown(ctx)
				if !returned.Load() {
					t.Errorf("Shutdown #%d returned before Run", i+1)
				}
				done[i].Store(true)
			})
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		for i := range done {
			if done[i].Load() {
				t.Fatalf("Shutdown #%d returned while Run was still flushing", i+1)
			}
		}

		close(release)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("Shutdown #%d = %v, want nil", i+1, err)
			}
		}
		if got := c.r.status(); got != StatusStopped {
			t.Errorf("status = %s, want stopped", got)
		}
	})
}

func TestComponent_ShutdownAfterCompletionIsStable(t *testing.T) {
	var runs atomic.Int32
	c := addWorker(newTestEngine(), continuousDef("w"), raceCountingWorker(&runs))
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("first Shutdown = %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for i := range 100 {
		if err := c.Shutdown(cancelled); err != nil {
			t.Fatalf("Shutdown #%d with a cancelled ctx = %v, want nil after completion", i+2, err)
		}
	}
}

// TestComponent_StartShutdownRace: a Start racing a Shutdown is either
// refused — and launches nothing — or has its loop joined before Shutdown
// returns, so a nil Shutdown never leaves a loop running.
func TestComponent_StartShutdownRace(t *testing.T) {
	for range 200 {
		var runs atomic.Int32
		c := addWorker(newTestEngine(), continuousDef("w"), raceCountingWorker(&runs))

		var wg sync.WaitGroup
		var startErr, shutdownErr error
		wg.Go(func() { startErr = c.Start(context.Background()) })
		wg.Go(func() { shutdownErr = c.Shutdown(context.Background()) })
		wg.Wait()

		if shutdownErr != nil {
			t.Fatalf("Shutdown = %v", shutdownErr)
		}
		if startErr != nil {
			if !strings.Contains(startErr.Error(), "Start after Shutdown") {
				t.Fatalf("Start = %v, want the refusal", startErr)
			}
			if got := runs.Load(); got != 0 {
				t.Fatalf("refused Start ran the worker %d times", got)
			}
			continue
		}
		select {
		case <-c.done:
		default:
			t.Fatal("Start succeeded and Shutdown returned, but the loop is still running")
		}
		if got := c.r.status(); got != StatusStopped {
			t.Fatalf("status = %s, want stopped", got)
		}
	}
}

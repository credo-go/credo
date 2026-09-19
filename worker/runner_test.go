package worker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestSafeRun_RecoversPanics(t *testing.T) {
	err := safeRun(t.Context(), Func(func(context.Context) error {
		panic("boom")
	}))
	p, ok := errors.AsType[*panicError](err)
	if !ok {
		t.Fatalf("safeRun() error = %T %v, want *panicError", err, err)
	}
	if got := err.Error(); got != "worker: run panicked: boom" {
		t.Fatalf("Error() = %q, want the panic value without a stack", got)
	}
	if !strings.Contains(string(p.stack), "goroutine") {
		t.Fatalf("stack = %q, want the recovered goroutine stack", p.stack)
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("panicError must not unwrap")
	}
}

func TestRunContinuous_RestartsAndStopsGracefully(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := newTestPool()

		var calls atomic.Int64
		worker := Func(func(ctx context.Context) error {
			if calls.Add(1) == 1 {
				return errors.New("boom")
			}
			<-ctx.Done()
			return ctx.Err()
		})

		if err := pool.addDefinition(&definition{
			name:    "continuous",
			resolve: instance(worker),
			restartPolicy: restartPolicy{
				restartDelay: 5 * time.Second,
			},
		}); err != nil {
			t.Fatalf("addDefinition() = %v", err)
		}

		if err := pool.Start(t.Context()); err != nil {
			t.Fatalf("Start() = %v", err)
		}

		// First run fails; the runner sleeps on the restart timer. No
		// restart has happened yet.
		synctest.Wait()
		info := pool.Workers()[0]
		if info.Status != StatusWaiting || info.Restarts != 0 {
			t.Fatalf("after first failure: status = %q restarts = %d, want waiting/0", info.Status, info.Restarts)
		}

		// Virtual time passes the restart delay; the second run starts and
		// blocks on ctx.
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if info = pool.Workers()[0]; info.Status != StatusRunning || info.Restarts != 1 {
			t.Fatalf("after restart: status = %q restarts = %d, want running/1", info.Status, info.Restarts)
		}

		shutdownPool(t, pool)
		if got := pool.Workers()[0].Status; got != StatusStopped {
			t.Fatalf("after shutdown: status = %q, want %q", got, StatusStopped)
		}
		if calls.Load() != 2 {
			t.Fatalf("call count = %d, want 2", calls.Load())
		}
	})
}

func TestRunContinuous_MaxRestartsMarksFailed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := newTestPool()

		var calls atomic.Int64
		worker := Func(func(context.Context) error {
			calls.Add(1)
			return errors.New("boom")
		})

		if err := pool.addDefinition(&definition{
			name:    "continuous-fail",
			resolve: instance(worker),
			restartPolicy: restartPolicy{
				maxRestarts:  2,
				restartDelay: time.Minute,
			},
		}); err != nil {
			t.Fatalf("addDefinition() = %v", err)
		}

		if err := pool.Start(t.Context()); err != nil {
			t.Fatalf("Start() = %v", err)
		}

		// maxRestarts: 2 is the first run plus two restarts: three runs.
		synctest.Wait()
		info := pool.Workers()[0]
		if info.Status != StatusWaiting || info.Restarts != 0 || !strings.Contains(info.LastError, "boom") {
			t.Fatalf("after first failure: %+v, want waiting/0/boom", info)
		}

		time.Sleep(time.Minute)
		synctest.Wait()
		info = pool.Workers()[0]
		if info.Status != StatusWaiting || info.Restarts != 1 {
			t.Fatalf("after the first restart failed: %+v, want waiting/1", info)
		}

		time.Sleep(time.Minute)
		synctest.Wait()
		info = pool.Workers()[0]
		if info.Status != StatusFailed || info.Restarts != 2 || !strings.Contains(info.LastError, "boom") {
			t.Fatalf("after max restarts: %+v, want failed/2/boom", info)
		}
		if got := calls.Load(); got != 3 {
			t.Fatalf("runs = %d, want 3", got)
		}

		shutdownPool(t, pool)
	})
}

func TestRunContinuous_SubcontextDeadlineCountsAsFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := newTestPool()

		worker := Func(func(ctx context.Context) error {
			childCtx, cancel := context.WithTimeout(ctx, time.Nanosecond)
			defer cancel()
			<-childCtx.Done()
			return childCtx.Err()
		})

		if err := pool.addDefinition(&definition{
			name:          "deadline",
			resolve:       instance(worker),
			restartPolicy: restartPolicy{maxRestarts: 1},
		}); err != nil {
			t.Fatalf("addDefinition() = %v", err)
		}

		if err := pool.Start(t.Context()); err != nil {
			t.Fatalf("Start() = %v", err)
		}

		// Let the virtual clock advance past the 1ns child deadline
		// (synctest.Wait alone does not advance time).
		time.Sleep(time.Millisecond)
		synctest.Wait()
		// maxRestarts: 1 restarts once; the restarted run fails too.
		info := pool.Workers()[0]
		if info.Status != StatusFailed || info.Restarts != 1 {
			t.Fatalf("sub-context deadline: %+v, want failed/1 (real failure, restarted once)", info)
		}

		shutdownPool(t, pool)
	})
}

func TestPoolWorkers_SnapshotWhileRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := newTestPool()

		worker := Func(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})

		if err := pool.addDefinition(&definition{
			name:          "snapshot",
			resolve:       instance(worker),
			restartPolicy: restartPolicy{maxRestarts: 2, restartDelay: time.Second},
		}); err != nil {
			t.Fatalf("addDefinition() = %v", err)
		}

		if err := pool.Start(t.Context()); err != nil {
			t.Fatalf("Start() = %v", err)
		}

		synctest.Wait() // worker is now blocked inside Run

		info := pool.Workers()[0]
		if info.Name != "snapshot" {
			t.Fatalf("Name = %q, want snapshot", info.Name)
		}
		if info.Kind != KindContinuous {
			t.Fatalf("Kind = %q, want %q", info.Kind, KindContinuous)
		}
		if want := (Config{MaxRestarts: 2, RestartDelay: time.Second}); !reflect.DeepEqual(info.Config, want) {
			t.Fatalf("Config = %+v, want %+v", info.Config, want)
		}
		if info.Status != StatusRunning {
			t.Fatalf("Status = %q, want %q", info.Status, StatusRunning)
		}
		if info.LastRun.IsZero() {
			t.Fatal("LastRun = zero, want non-zero while worker is running")
		}
		if info.LastError != "" {
			t.Fatalf("LastError = %q, want empty", info.LastError)
		}

		shutdownPool(t, pool)
	})
}

func TestRunScheduled_SkipsOverlap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := newTestPool()

		var calls atomic.Int64
		release := make(chan struct{})
		worker := Func(func(ctx context.Context) error {
			calls.Add(1)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})

		if err := pool.addDefinition(&definition{
			name:     "scheduled",
			resolve:  instance(worker),
			schedule: mustSchedule(t, "@every 1m"),
		}); err != nil {
			t.Fatalf("addDefinition() = %v", err)
		}

		if err := pool.Start(t.Context()); err != nil {
			t.Fatalf("Start() = %v", err)
		}

		// First activation fires after one minute and blocks inside Run.
		time.Sleep(time.Minute)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("call count = %d, want 1 (first activation)", calls.Load())
		}

		// Two more activations elapse while the first run is still in
		// flight — the serial loop must skip them, not queue them.
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("call count = %d, want 1 (overlapping activations skipped)", calls.Load())
		}

		// Release the first run; the loop skips the missed activations and
		// arms the next future one, which then runs.
		release <- struct{}{}
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("call count = %d, want 2 (next activation after overlap)", calls.Load())
		}

		shutdownPool(t, pool)
	})
}

func TestRunScheduled_MaxConsecutiveFailuresMarksFailed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := newTestPool()

		worker := Func(func(context.Context) error {
			return errors.New("boom")
		})

		if err := pool.addDefinition(&definition{
			name:          "scheduled-fail",
			resolve:       instance(worker),
			schedule:      mustSchedule(t, "@every 1m"),
			failurePolicy: failurePolicy{maxConsecutiveFailures: 2},
		}); err != nil {
			t.Fatalf("addDefinition() = %v", err)
		}

		if err := pool.Start(t.Context()); err != nil {
			t.Fatalf("Start() = %v", err)
		}

		time.Sleep(time.Minute)
		synctest.Wait()
		info := pool.Workers()[0]
		if info.Status != StatusWaiting || info.ConsecutiveFailures != 1 || !strings.Contains(info.LastError, "boom") {
			t.Fatalf("after first failure: %+v, want waiting/1/boom", info)
		}

		time.Sleep(time.Minute)
		synctest.Wait()
		info = pool.Workers()[0]
		if info.Status != StatusFailed || info.ConsecutiveFailures != 2 || !strings.Contains(info.LastError, "boom") {
			t.Fatalf("after max consecutive failures: %+v, want failed/2/boom", info)
		}

		shutdownPool(t, pool)
	})
}

func TestRunScheduled_StartImmediatelySetsZeroScheduledAt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := newTestPool()

		scheduledAtCh := make(chan time.Time, 1)
		worker := Func(func(ctx context.Context) error {
			scheduledAtCh <- ScheduledAt(ctx)
			return nil
		})

		if err := pool.addDefinition(&definition{
			name:             "startup",
			resolve:          instance(worker),
			schedule:         mustSchedule(t, "@every 1h"),
			startImmediately: true,
		}); err != nil {
			t.Fatalf("addDefinition() = %v", err)
		}

		if err := pool.Start(t.Context()); err != nil {
			t.Fatalf("Start() = %v", err)
		}

		synctest.Wait()
		select {
		case scheduledAt := <-scheduledAtCh:
			if !scheduledAt.IsZero() {
				t.Fatalf("ScheduledAt() = %s, want zero", scheduledAt)
			}
		default:
			t.Fatal("startImmediately worker did not run")
		}

		shutdownPool(t, pool)
	})
}

func TestPoolShutdown_DeadlineExceeded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := newTestPool()

		release := make(chan struct{})
		worker := Func(func(context.Context) error {
			<-release
			return nil
		})

		if err := pool.addDefinition(&definition{
			name:    "stubborn",
			resolve: instance(worker),
		}); err != nil {
			t.Fatalf("addDefinition() = %v", err)
		}

		if err := pool.Start(t.Context()); err != nil {
			t.Fatalf("Start() = %v", err)
		}

		synctest.Wait() // worker is blocked, ignoring its context

		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		err := pool.Shutdown(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown() error = %v, want %v", err, context.DeadlineExceeded)
		}

		close(release)
		pool.wg.Wait()
	})
}

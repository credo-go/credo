package worker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/credo-go/credo"
)

// runnerStart adds w under def to a fresh test supervisor, starts its
// component and returns both; the component stops when the test ends.
func runnerStart(t *testing.T, def *definition, w Worker) (*Supervisor, *component) {
	t.Helper()
	s := newTestSupervisor()
	c := addWorker(s, def, w)
	startWorker(t, c)
	return s, c
}

// runnerBlocking returns a continuous worker that runs until its context is
// cancelled, the idiomatic way.
func runnerBlocking() Func {
	return func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
}

// runnerCall is one observed Run invocation.
type runnerCall struct {
	at  time.Duration // offset from the recorder's origin
	run RunInfo
}

// runnerRecorder wraps a run function and records every invocation.
type runnerRecorder struct {
	origin time.Time
	mu     sync.Mutex
	calls  []runnerCall
}

func newRunnerRecorder() *runnerRecorder { return &runnerRecorder{origin: time.Now()} }

// wrap returns a worker that records the call and then runs fn with the
// 1-based call number.
func (rec *runnerRecorder) wrap(fn func(ctx context.Context, n int) error) Func {
	return func(ctx context.Context) error {
		info, _ := CurrentRun(ctx)
		rec.mu.Lock()
		rec.calls = append(rec.calls, runnerCall{at: time.Since(rec.origin), run: info})
		n := len(rec.calls)
		rec.mu.Unlock()
		return fn(ctx, n)
	}
}

func (rec *runnerRecorder) all() []runnerCall {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return slices.Clone(rec.calls)
}

func (rec *runnerRecorder) offsets() []time.Duration {
	var out []time.Duration
	for _, c := range rec.all() {
		out = append(out, c.at)
	}
	return out
}

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

func TestSafeRun_ReturnsTheRunError(t *testing.T) {
	boom := errors.New("boom")
	if err := safeRun(t.Context(), Func(func(context.Context) error { return boom })); !errors.Is(err, boom) {
		t.Fatalf("safeRun() = %v, want the error Run returned", err)
	}
	if err := safeRun(t.Context(), Func(func(context.Context) error { return nil })); err != nil {
		t.Fatalf("safeRun() = %v, want nil", err)
	}
}

func TestRunContinuous_RestartsAndStopsGracefully(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		w := Func(func(ctx context.Context) error {
			if calls.Add(1) == 1 {
				return errors.New("boom")
			}
			<-ctx.Done()
			return ctx.Err()
		})
		s, c := runnerStart(t, continuousDef("continuous", ContinuousConfig{
			Restart: Restart{MinDelay: 5 * time.Second, MaxDelay: 5 * time.Second},
		}), w)

		// The first run fails; the loop waits to restart, and no restart has
		// happened yet.
		synctest.Wait()
		info := s.Snapshot()[0]
		if info.Status != StatusBackoff || info.Restarts != 0 || info.LastError != "boom" {
			t.Fatalf("after the first failure: %+v, want backoff, 0 restarts, LastError boom", info)
		}

		// The restart delay passes; the second run starts and blocks.
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if info = s.Snapshot()[0]; info.Status != StatusRunning || info.Restarts != 1 {
			t.Fatalf("after the restart: status = %q restarts = %d, want running/1", info.Status, info.Restarts)
		}

		stopWorker(t, c)
		info = s.Snapshot()[0]
		if info.Status != StatusStopped || info.Restarts != 1 || info.LastError != "boom" {
			t.Fatalf("after shutdown: %+v, want stopped, 1 restart, the last failure kept", info)
		}
		if calls.Load() != 2 {
			t.Fatalf("call count = %d, want 2", calls.Load())
		}
	})
}

func TestRunContinuous_RestartLimit(t *testing.T) {
	t.Run("limit 2 is the first run plus two restarts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var calls atomic.Int64
			s, _ := runnerStart(t, continuousDef("continuous-fail", ContinuousConfig{
				Restart: Restart{Limit: 2, MinDelay: time.Minute, MaxDelay: time.Minute}, // a fixed cadence
			}), Func(func(context.Context) error {
				calls.Add(1)
				return errors.New("boom")
			}))

			synctest.Wait()
			info := s.Snapshot()[0]
			if info.Status != StatusBackoff || info.Restarts != 0 || info.LastError != "boom" {
				t.Fatalf("after the first failure: %+v, want backoff/0/boom", info)
			}

			time.Sleep(time.Minute)
			synctest.Wait()
			if info = s.Snapshot()[0]; info.Status != StatusBackoff || info.Restarts != 1 {
				t.Fatalf("after the first restart failed: %+v, want backoff/1", info)
			}

			time.Sleep(time.Minute)
			synctest.Wait()
			info = s.Snapshot()[0]
			if info.Status != StatusFailed || info.Restarts != 2 || info.LastError != "boom" {
				t.Fatalf("after the limit: %+v, want failed/2/boom", info)
			}

			// failed is permanent: no further run, however long we wait.
			time.Sleep(time.Hour)
			synctest.Wait()
			if got := calls.Load(); got != 3 {
				t.Fatalf("runs = %d, want 3", got)
			}
		})
	})

	t.Run("limit 1 runs twice", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var runs atomic.Int32
			s, _ := runnerStart(t, continuousDef("consumer", ContinuousConfig{
				Restart: Restart{Limit: 1, MinDelay: time.Second},
			}), Func(func(context.Context) error {
				runs.Add(1)
				return errors.New("boom")
			}))
			time.Sleep(time.Minute)
			synctest.Wait()
			info := s.Snapshot()[0]
			if runs.Load() != 2 || info.Restarts != 1 || info.Status != StatusFailed {
				t.Fatalf("Limit 1: runs = %d, %+v, want 2 runs, 1 restart, failed", runs.Load(), info)
			}
		})
	})

	t.Run("zero is unlimited and shutdown during the delay counts nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var runs atomic.Int32
			s, c := runnerStart(t, continuousDef("consumer", ContinuousConfig{
				Restart: Restart{MinDelay: time.Second, MaxDelay: time.Second},
			}), Func(func(context.Context) error {
				runs.Add(1)
				return errors.New("boom")
			}))
			// A fixed one-second delay: runs at 0s, 1s, 2s, 3s and 4s; the loop
			// then waits for 5s.
			time.Sleep(4*time.Second + 500*time.Millisecond)
			synctest.Wait()
			info := s.Snapshot()[0]
			if runs.Load() != 5 || info.Restarts != 4 || info.Status != StatusBackoff {
				t.Fatalf("unlimited: runs = %d, %+v, want 5 runs, 4 restarts, backoff", runs.Load(), info)
			}
			stopWorker(t, c)
			info = s.Snapshot()[0]
			if info.Restarts != 4 || info.Status != StatusStopped || runs.Load() != 5 {
				t.Fatalf("after shutdown in the delay: runs = %d, %+v, want the restart never counted", runs.Load(), info)
			}
		})
	})
}

func TestRunContinuous_SubcontextDeadlineCountsAsFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := runnerStart(t, continuousDef("deadline", ContinuousConfig{
			Restart: Restart{Limit: 1, MinDelay: time.Second, MaxDelay: time.Second},
		}), Func(func(ctx context.Context) error {
			childCtx, cancel := context.WithTimeout(ctx, time.Nanosecond)
			defer cancel()
			<-childCtx.Done()
			return childCtx.Err()
		}))

		// Let the virtual clock pass the child deadlines and the restart delay
		// (synctest.Wait alone does not advance time).
		time.Sleep(2 * time.Second)
		synctest.Wait()
		// A context error while the worker is alive is a real failure: Limit 1
		// restarts once, and the restarted run fails too.
		info := s.Snapshot()[0]
		if info.Status != StatusFailed || info.Restarts != 1 || info.LastError != "context deadline exceeded" {
			t.Fatalf("sub-context deadline: %+v, want failed/1 with the deadline error", info)
		}
	})
}

func TestRunContinuous_RestartDisabled(t *testing.T) {
	cases := []struct {
		name string
		run  Func
		want string
	}{
		{"error", func(context.Context) error { return errors.New("boom") }, "boom"},
		{"panic", func(context.Context) error { panic("kaboom") }, "worker: run panicked: kaboom"},
		{"early nil return", func(context.Context) error { return nil }, errUnexpectedExit.Error()},
	}
	for _, tc := range cases {
		t.Run("first failure is terminal: "+tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var runs atomic.Int32
				s, _ := runnerStart(t, continuousDef("consumer", ContinuousConfig{
					Restart: Restart{Disabled: true},
				}), Func(func(ctx context.Context) error {
					runs.Add(1)
					return tc.run(ctx)
				}))
				time.Sleep(time.Hour)
				synctest.Wait()
				info := s.Snapshot()[0]
				if info.Status != StatusFailed || info.Restarts != 0 || info.LastError != tc.want || runs.Load() != 1 {
					t.Fatalf("runs = %d, %+v, want one run, failed, 0 restarts, LastError %q", runs.Load(), info, tc.want)
				}
			})
		})
	}

	t.Run("a failure during shutdown is terminal too", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, c := runnerStart(t, continuousDef("consumer", ContinuousConfig{
				Restart: Restart{Disabled: true},
			}), Func(func(ctx context.Context) error {
				<-ctx.Done()
				return errors.New("flush failed")
			}))
			synctest.Wait()
			stopWorker(t, c)
			if info := s.Snapshot()[0]; info.Status != StatusFailed || info.LastError != "flush failed" {
				t.Fatalf("%+v, want failed with the flush error: the failure ended the worker", info)
			}
		})
	})

	t.Run("a graceful stop is not a failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, c := runnerStart(t, continuousDef("consumer", ContinuousConfig{
				Restart: Restart{Disabled: true},
			}), runnerBlocking())
			synctest.Wait()
			stopWorker(t, c)
			if info := s.Snapshot()[0]; info.Status != StatusStopped || info.LastError != "" {
				t.Fatalf("%+v, want stopped without an error", info)
			}
		})
	})
}

func TestSnapshot_WhileRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		s, _ := runnerStart(t, continuousDef("snapshot", ContinuousConfig{
			Restart: Restart{Limit: 2, MinDelay: time.Second, MaxDelay: time.Second},
		}), runnerBlocking())

		synctest.Wait() // the worker is blocked inside Run

		info := s.Snapshot()[0]
		if info.Name != "snapshot" || info.Kind != KindContinuous || info.Schedule != "" || info.Scheduled != nil {
			t.Fatalf("identity = %+v, want a continuous worker named snapshot", info)
		}
		want := ContinuousConfig{
			Tier:    credo.TierInternal,
			Restart: Restart{Limit: 2, MinDelay: time.Second, MaxDelay: time.Second},
		}
		if info.Continuous == nil || *info.Continuous != want {
			t.Fatalf("Continuous = %+v, want %+v", info.Continuous, want)
		}
		if info.Status != StatusRunning {
			t.Fatalf("Status = %q, want %q", info.Status, StatusRunning)
		}
		if !info.LastStartedAt.Equal(start) {
			t.Fatalf("LastStartedAt = %v, want the admission time %v", info.LastStartedAt, start)
		}
		if info.LastError != "" || !info.LastSucceededAt.IsZero() {
			t.Fatalf("LastError = %q, LastSucceededAt = %v, want both empty", info.LastError, info.LastSucceededAt)
		}
	})
}

func TestRunScheduled_StatusBeforeTheFirstActivation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestSupervisor()
		c := addWorker(s, scheduledDef("report", "@every 1m"), Func(func(context.Context) error { return nil }))
		if got := s.Snapshot()[0].Status; got != StatusPending {
			t.Fatalf("before Start: status = %q, want pending", got)
		}
		startWorker(t, c)
		synctest.Wait()
		if info := s.Snapshot()[0]; info.Status != StatusWaiting || !info.LastStartedAt.IsZero() {
			t.Fatalf("after Start: %+v, want waiting and never started", info)
		}
	})
}

func TestRunScheduled_SkipsOverlap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newRunnerRecorder()
		release := make(chan struct{})
		s, _ := runnerStart(t, scheduledDef("scheduled", "@every 1m"), rec.wrap(func(ctx context.Context, _ int) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}))

		// The first activation fires after one minute and blocks inside Run.
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := len(rec.all()); n != 1 {
			t.Fatalf("call count = %d, want 1 (first activation)", n)
		}
		if got := s.Snapshot()[0].Status; got != StatusRunning {
			t.Fatalf("status = %q, want running", got)
		}

		// Two more activations elapse while the first run is in flight — the
		// serial loop must skip them, not queue them.
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if n := len(rec.all()); n != 1 {
			t.Fatalf("call count = %d, want 1 (overlapping activations skipped)", n)
		}

		// Release the first run; the loop skips the missed activations and arms
		// the next future one, which then runs.
		release <- struct{}{}
		synctest.Wait()
		if got := s.Snapshot()[0].Status; got != StatusWaiting {
			t.Fatalf("after the run: status = %q, want waiting", got)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		calls := rec.all()
		if len(calls) != 2 {
			t.Fatalf("call count = %d, want 2 (next activation after overlap)", len(calls))
		}
		if want := rec.origin.Add(4 * time.Minute); calls[1].at != 4*time.Minute || !calls[1].run.ScheduledAt.Equal(want) {
			t.Fatalf("second run at %s scheduled %v, want the 4m activation", calls[1].at, calls[1].run.ScheduledAt)
		}
	})
}

func TestRunScheduled_AnchoredGrid(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newRunnerRecorder()
		runnerStart(t, scheduledDef("report", "@every 1m"), rec.wrap(func(context.Context, int) error {
			time.Sleep(30 * time.Second) // a long run delays nothing on the grid
			return nil
		}))

		time.Sleep(3*time.Minute + 40*time.Second)
		synctest.Wait()
		calls := rec.all()
		want := []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute}
		if got := rec.offsets(); !slices.Equal(got, want) {
			t.Fatalf("runs started at %v, want %v: the next activation follows the last intended one", got, want)
		}
		for i, c := range calls {
			if want := rec.origin.Add(want[i]); !c.run.ScheduledAt.Equal(want) {
				t.Fatalf("run %d ScheduledAt = %v, want %v", i+1, c.run.ScheduledAt, want)
			}
		}
	})
}

func TestRunScheduled_RunOnStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newRunnerRecorder()
		s, _ := runnerStart(t, scheduledDef("startup", "@every 1m", ScheduledConfig{RunOnStart: true}),
			rec.wrap(func(_ context.Context, n int) error {
				if n == 1 {
					time.Sleep(25 * time.Second)
				}
				return nil
			}))

		synctest.Wait()
		calls := rec.all()
		if len(calls) != 1 || calls[0].at != 0 || !calls[0].run.ScheduledAt.IsZero() {
			t.Fatalf("calls = %+v, want one immediate run with a zero ScheduledAt", calls)
		}

		// The run on start finished at 25s; the first computed activation is
		// based on that time: 1m25s, then 2m25s.
		time.Sleep(2*time.Minute + 30*time.Second)
		synctest.Wait()
		calls = rec.all()
		want := []time.Duration{0, 85 * time.Second, 145 * time.Second}
		if got := rec.offsets(); !slices.Equal(got, want) {
			t.Fatalf("runs started at %v, want %v", got, want)
		}
		if at := rec.origin.Add(85 * time.Second); !calls[1].run.ScheduledAt.Equal(at) {
			t.Fatalf("first computed activation ScheduledAt = %v, want %v", calls[1].run.ScheduledAt, at)
		}
		if info := s.Snapshot()[0]; !info.LastSucceededAt.Equal(rec.origin.Add(145 * time.Second)) {
			t.Fatalf("LastSucceededAt = %v, want the completion time of the last run", info.LastSucceededAt)
		}
	})
}

func TestRunScheduled_MaxConsecutiveFailures(t *testing.T) {
	t.Run("reaching the limit marks failed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var runs atomic.Int32
			s, _ := runnerStart(t, scheduledDef("scheduled-fail", "@every 1m", ScheduledConfig{MaxConsecutiveFailures: 2}),
				Func(func(context.Context) error {
					runs.Add(1)
					return errors.New("boom")
				}))

			time.Sleep(time.Minute)
			synctest.Wait()
			info := s.Snapshot()[0]
			if info.Status != StatusWaiting || info.ConsecutiveFailures != 1 || info.LastError != "boom" {
				t.Fatalf("after the first failure: %+v, want waiting/1/boom", info)
			}

			time.Sleep(time.Minute)
			synctest.Wait()
			info = s.Snapshot()[0]
			if info.Status != StatusFailed || info.ConsecutiveFailures != 2 || info.LastError != "boom" {
				t.Fatalf("after the limit: %+v, want failed/2/boom", info)
			}
			if info.Restarts != 0 {
				t.Fatalf("Restarts = %d, want 0 for a scheduled worker", info.Restarts)
			}

			time.Sleep(time.Hour)
			synctest.Wait()
			if runs.Load() != 2 {
				t.Fatalf("runs = %d, want 2: failed is permanent", runs.Load())
			}
		})
	})

	t.Run("a success resets the streak", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := runnerStart(t, scheduledDef("report", "@every 1m", ScheduledConfig{MaxConsecutiveFailures: 2}),
				newRunnerRecorder().wrap(func(_ context.Context, n int) error {
					if n == 2 {
						return nil
					}
					return errors.New("boom")
				}))

			time.Sleep(2 * time.Minute)
			synctest.Wait()
			info := s.Snapshot()[0]
			if info.Status != StatusWaiting || info.ConsecutiveFailures != 0 || info.LastError != "" ||
				!info.LastSucceededAt.Equal(info.LastStartedAt) {
				t.Fatalf("after a success: %+v, want waiting, streak 0, LastError cleared, LastSucceededAt stamped", info)
			}

			time.Sleep(time.Minute)
			synctest.Wait()
			if info = s.Snapshot()[0]; info.Status != StatusWaiting || info.ConsecutiveFailures != 1 {
				t.Fatalf("after the next failure: %+v, want waiting/1: the success reset the streak", info)
			}
		})
	})
}

func TestRunScheduled_ScheduleExhausted(t *testing.T) {
	const never = "0 0 30 2 *" // February 30th
	cases := []struct {
		name     string
		cfg      ScheduledConfig
		wantRuns int32
	}{
		{"before the first activation", ScheduledConfig{}, 0},
		{"after the run on start", ScheduledConfig{RunOnStart: true}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var runs atomic.Int32
				s, _ := runnerStart(t, scheduledDef("never", never, tc.cfg), Func(func(context.Context) error {
					runs.Add(1)
					return nil
				}))
				synctest.Wait()
				info := s.Snapshot()[0]
				if info.Status != StatusFailed || info.LastError != "schedule has no future activation" {
					t.Fatalf("%+v, want failed with LastError %q", info, "schedule has no future activation")
				}
				if runs.Load() != tc.wantRuns {
					t.Fatalf("runs = %d, want %d", runs.Load(), tc.wantRuns)
				}
			})
		})
	}
}

func TestCurrentRun(t *testing.T) {
	t.Run("continuous runs carry the worker and a fresh ID", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rec := newRunnerRecorder()
			runnerStart(t, continuousDef("consumer", ContinuousConfig{
				Restart: Restart{MinDelay: time.Second, MaxDelay: time.Second},
			}), rec.wrap(func(ctx context.Context, n int) error {
				if n == 1 {
					return errors.New("boom")
				}
				<-ctx.Done()
				return nil
			}))
			time.Sleep(2 * time.Second)
			synctest.Wait()
			calls := rec.all()
			if len(calls) != 2 {
				t.Fatalf("%d runs, want 2", len(calls))
			}
			for _, c := range calls {
				if c.run.Worker != "consumer" || c.run.ID == "" || !c.run.ScheduledAt.IsZero() {
					t.Fatalf("RunInfo = %+v, want the worker name, an ID and a zero ScheduledAt", c.run)
				}
			}
			if calls[0].run.ID == calls[1].run.ID {
				t.Fatalf("both runs have ID %q, want one per run", calls[0].run.ID)
			}
		})
	})

	t.Run("scheduled runs carry the intended activation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rec := newRunnerRecorder()
			runnerStart(t, scheduledDef("report", "@every 1m"), rec.wrap(func(context.Context, int) error { return nil }))
			time.Sleep(time.Minute)
			synctest.Wait()
			calls := rec.all()
			if len(calls) != 1 {
				t.Fatalf("%d runs, want 1", len(calls))
			}
			run := calls[0].run
			if run.Worker != "report" || run.ID == "" || !run.ScheduledAt.Equal(rec.origin.Add(time.Minute)) {
				t.Fatalf("RunInfo = %+v, want the worker name, an ID and the 1m activation", run)
			}
		})
	})

	t.Run("a plain context is not a run context", func(t *testing.T) {
		if info, ok := CurrentRun(t.Context()); ok || info != (RunInfo{}) {
			t.Fatalf("CurrentRun(plain) = %+v, %v, want zero and false", info, ok)
		}
		var nilCtx context.Context
		if info, ok := CurrentRun(nilCtx); ok || info != (RunInfo{}) {
			t.Fatalf("CurrentRun(nil) = %+v, %v, want zero and false", info, ok)
		}
	})
}

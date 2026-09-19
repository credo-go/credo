package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestClassifyRun(t *testing.T) {
	boom := errors.New("boom")
	panicked := &panicError{value: context.Canceled, stack: []byte("goroutine 1 [running]:")}
	const budget = 15 * time.Second

	tests := []struct {
		name string
		in   runInput
		want runOutcome
		// wantErr is the recorded error's text; "" means no error.
		wantErr string
	}{
		// Row 1: a panic is a failure, whatever else happened.
		{"panic", runInput{kind: KindScheduled, err: panicked},
			runOutcome{verdict: runFailed, stack: panicked.stack}, "worker: run panicked: context canceled"},
		{"panic with a context-error value during shutdown", runInput{kind: KindContinuous, err: panicked, poolDone: true},
			runOutcome{verdict: runFailed, stack: panicked.stack}, "worker: run panicked: context canceled"},
		{"panic beats timeout", runInput{kind: KindScheduled, err: panicked, timedOut: true, timeout: budget},
			runOutcome{verdict: runFailed, stack: panicked.stack}, "worker: run panicked: context canceled"},

		// Row 2: the timeout cause makes a failure of any return value.
		{"timeout then nil", runInput{kind: KindScheduled, timedOut: true, timeout: budget},
			runOutcome{verdict: runFailed, timedOut: true}, "worker: run timed out after 15s"},
		{"timeout then error", runInput{kind: KindScheduled, err: boom, timedOut: true, timeout: budget},
			runOutcome{verdict: runFailed, timedOut: true}, "worker: run timed out after 15s: boom"},
		{"timeout then shutdown", runInput{kind: KindScheduled, err: context.DeadlineExceeded, timedOut: true, timeout: budget, poolDone: true},
			runOutcome{verdict: runFailed, timedOut: true}, "worker: run timed out after 15s: context deadline exceeded"},

		// Row 3: nil or a context error under pool cancellation is graceful.
		{"shutdown then nil (scheduled)", runInput{kind: KindScheduled, poolDone: true},
			runOutcome{verdict: runStopped}, ""},
		{"shutdown then nil (continuous)", runInput{kind: KindContinuous, poolDone: true},
			runOutcome{verdict: runStopped}, ""},
		{"shutdown then context.Canceled", runInput{kind: KindContinuous, err: context.Canceled, poolDone: true},
			runOutcome{verdict: runStopped}, ""},
		{"shutdown then wrapped deadline", runInput{kind: KindScheduled, err: fmt.Errorf("query: %w", context.DeadlineExceeded), poolDone: true},
			runOutcome{verdict: runStopped}, ""},
		{"shutdown then joined context errors", runInput{kind: KindContinuous, err: errors.Join(context.Canceled, context.Canceled), poolDone: true},
			runOutcome{verdict: runStopped}, ""},

		// Row 4: any other error is a failure, during shutdown too.
		{"error", runInput{kind: KindContinuous, err: boom},
			runOutcome{verdict: runFailed}, "boom"},
		{"shutdown then a non-context error", runInput{kind: KindScheduled, err: boom, poolDone: true},
			runOutcome{verdict: runFailed}, "boom"},
		{"shutdown then a context error joined with another error", runInput{kind: KindScheduled, err: errors.Join(context.Canceled, boom), poolDone: true},
			runOutcome{verdict: runFailed}, "context canceled\nboom"},
		{"context error while alive", runInput{kind: KindContinuous, err: context.DeadlineExceeded},
			runOutcome{verdict: runFailed}, "context deadline exceeded"},

		// Row 5: a scheduled nil return is a success.
		{"scheduled nil", runInput{kind: KindScheduled},
			runOutcome{verdict: runSucceeded}, ""},

		// Row 6: a continuous nil return while alive is an unexpected exit.
		{"continuous nil while alive", runInput{kind: KindContinuous},
			runOutcome{verdict: runFailed, unexpectedExit: true}, errUnexpectedExit.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyRun(tt.in)
			gotErr := errorText(got.err)
			got.err = nil
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("classifyRun() = %+v, want %+v", got, tt.want)
			}
			if gotErr != tt.wantErr {
				t.Errorf("recorded error = %q, want %q", gotErr, tt.wantErr)
			}
		})
	}

	t.Run("timeout errors wrap ErrRunTimeout and the returned error", func(t *testing.T) {
		out := classifyRun(runInput{kind: KindScheduled, err: boom, timedOut: true, timeout: budget})
		if !errors.Is(out.err, ErrRunTimeout) || !errors.Is(out.err, boom) {
			t.Fatalf("recorded error %v does not wrap both ErrRunTimeout and the returned error", out.err)
		}
	})
}

// isCanceledError reports itself as context.Canceled through an Is method and
// wraps nothing, the way the net package's cancelled-dial error does.
type isCanceledError struct{}

func (isCanceledError) Error() string { return "operation was canceled" }

func (isCanceledError) Is(target error) bool { return target == context.Canceled }

// vouchingError claims to be context.Canceled through its Is method while
// its children carry whatever they carry.
type vouchingError struct {
	child    error
	children []error
}

func (vouchingError) Error() string { return "vouching" }

func (vouchingError) Is(target error) bool { return target == context.Canceled }

type vouchingWrapError struct{ vouchingError }

func (e vouchingWrapError) Unwrap() error { return e.child }

type vouchingJoinError struct{ vouchingError }

func (e vouchingJoinError) Unwrap() []error { return e.children }

func TestIsContextError(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, true},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"other error", boom, false},
		{"wrap chain", fmt.Errorf("flush: %w", fmt.Errorf("query: %w", context.Canceled)), true},
		{"Is method", isCanceledError{}, true},
		{"wrapped Is method", fmt.Errorf("dial: %w", isCanceledError{}), true},
		{"join of context errors", errors.Join(context.Canceled, context.DeadlineExceeded), true},
		{"join with nested wrap chains", errors.Join(fmt.Errorf("a: %w", context.Canceled), isCanceledError{}), true},
		{"join with another error", errors.Join(context.Canceled, boom), false},
		{"join with another error first", errors.Join(boom, context.Canceled), false},
		{"nested join hiding another error", errors.Join(context.Canceled, errors.Join(context.Canceled, boom)), false},
		{"wrapped join with another error", fmt.Errorf("stop: %w", errors.Join(context.Canceled, boom)), false},
		{"two %w verbs, one not a context error", fmt.Errorf("%w: %w", boom, context.Canceled), false},
		{"Is method over a wrapped other error", vouchingWrapError{vouchingError{child: boom}}, false},
		{"Is method over a mixed join", vouchingJoinError{vouchingError{children: []error{context.Canceled, boom}}}, false},
		{"Is method over a wrapped context error", vouchingWrapError{vouchingError{child: context.Canceled}}, true},
		{"Is method with a nil child is a leaf", vouchingWrapError{}, true},
		{"Is method with no branches is a leaf", vouchingJoinError{}, true},
		{"two %w verbs, both context errors", fmt.Errorf("%w: %w", context.DeadlineExceeded, context.Canceled), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isContextError(tt.err); got != tt.want {
				t.Errorf("isContextError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestRun_ContextErrorJoinedWithAnotherErrorIsAFailure is the regression test
// for a final write that fails during shutdown: joining its error with
// ctx.Err() must not turn the run into a graceful stop and lose the error.
func TestRun_ContextErrorJoinedWithAnotherErrorIsAFailure(t *testing.T) {
	flushErr := errors.New("final batch write failed")
	run := Func(func(ctx context.Context) error {
		<-ctx.Done()
		return errors.Join(ctx.Err(), flushErr)
	})
	const want = "context canceled\nfinal batch write failed"

	t.Run("continuous", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newTestPool()
			startPool(t, p, mustDefinition(t, "consumer", run))
			synctest.Wait()
			shutdownPool(t, p)
			info := p.Workers()[0]
			if info.Status != StatusStopped || info.LastError != want {
				t.Fatalf("%+v, want stopped with LastError %q", info, want)
			}
		})
	})
	t.Run("scheduled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newTestPool()
			startPool(t, p, mustDefinition(t, "report", run, WithSchedule("@every 1h"), WithStartImmediately()))
			synctest.Wait()
			shutdownPool(t, p)
			info := p.Workers()[0]
			if info.Status != StatusStopped || info.ConsecutiveFailures != 1 || info.LastError != want {
				t.Fatalf("%+v, want stopped with one failure and LastError %q", info, want)
			}
		})
	})
}

// barrierPolicy is an in-package loopPolicy whose beforeRun blocks on its
// n-th call until the test releases it, so the test can complete a
// cancellation before driveLoop reaches its admission check. Every run fails
// and is recorded like a continuous failure.
type barrierPolicy struct {
	r       *runner
	wait    time.Duration // before every run after the first
	blockAt int           // 1-based beforeRun call that blocks
	calls   int
	ran     bool
	entered chan struct{}
	release chan struct{}
}

func newBarrierPolicy(r *runner, blockAt int, wait time.Duration) *barrierPolicy {
	return &barrierPolicy{r: r, wait: wait, blockAt: blockAt,
		entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *barrierPolicy) start(context.Context) (time.Duration, bool) { return waitNone, false }

func (b *barrierPolicy) startAttrs() []any { return nil }

func (b *barrierPolicy) beforeRun() (time.Time, bool) {
	b.calls++
	if b.calls == b.blockAt {
		close(b.entered)
		<-b.release
	}
	return time.Time{}, b.ran
}

func (b *barrierPolicy) afterRun(_ context.Context, res runResult) (time.Duration, bool) {
	b.ran = true
	b.r.setOutcome(StatusWaiting, res.outcome.err)
	return b.wait, false
}

// TestDriveLoop_CancellationObservedAtAdmission covers W11: a cancellation
// completed before the admission check prevents the next Run, and leaves
// LastRun and Restarts as they were.
func TestDriveLoop_CancellationObservedAtAdmission(t *testing.T) {
	cases := []struct {
		name    string
		blockAt int
		wait    time.Duration
	}{
		{"before the first run", 1, waitNone},
		{"before a restart", 2, waitNone},
		{"before a restart whose timer fired", 2, time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var runs atomic.Int32
				w := Func(func(context.Context) error {
					runs.Add(1)
					return errors.New("boom")
				})
				def := &definition{name: "barrier", resolve: instance(w)}
				r := newRunner(def, w)
				policy := newBarrierPolicy(r, tc.blockAt, tc.wait)
				p := newTestPool()

				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() {
					defer close(done)
					p.driveLoop(ctx, r, policy)
				}()

				var before Info
				if tc.blockAt > 1 {
					synctest.Wait() // the first run failed; the loop waits or blocks
					if tc.wait != waitNone {
						time.Sleep(tc.wait) // let the restart timer win its select
					}
				}
				<-policy.entered
				before = r.snapshot()
				cancel()
				close(policy.release)
				<-done

				after := r.snapshot()
				wantRuns := int32(tc.blockAt - 1)
				if got := runs.Load(); got != wantRuns {
					t.Fatalf("Run invocations = %d, want %d (no Run after the cancellation was observed)", got, wantRuns)
				}
				if after.Status != StatusStopped {
					t.Fatalf("status = %q, want stopped", after.Status)
				}
				if !after.LastRun.Equal(before.LastRun) || after.Restarts != before.Restarts {
					t.Fatalf("LastRun/Restarts moved from %v/%d to %v/%d", before.LastRun, before.Restarts, after.LastRun, after.Restarts)
				}
				if tc.blockAt == 1 && !after.LastRun.IsZero() {
					t.Fatalf("LastRun = %v, want zero before the first run", after.LastRun)
				}
				if tc.blockAt > 1 && (after.LastRun.IsZero() || after.Restarts != 0) {
					t.Fatalf("after one failed run: LastRun = %v, Restarts = %d, want the first run's time and 0", after.LastRun, after.Restarts)
				}
			})
		})
	}
}

func TestRunContinuous_NilWhileAliveIsAnUnexpectedExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := newLogCapture()
		p := newPool(logs.logger(), DefaultRestartDelay)
		var runs atomic.Int32
		def := mustDefinition(t, "consumer", Func(func(context.Context) error {
			runs.Add(1)
			return nil // e.g. a range loop over a channel that was closed
		}), WithMaxRestarts(1), WithRestartDelay(time.Minute), WithReadiness(ReadinessPolicy{FailWhenFailed: true}))
		startPool(t, p, def)

		synctest.Wait()
		info := p.Workers()[0]
		if info.Status != StatusWaiting || info.Restarts != 0 || info.LastError != errUnexpectedExit.Error() {
			t.Fatalf("after the first nil return: %+v, want waiting, 0 restarts, the unexpected-exit error", info)
		}
		if err := p.evaluateReadiness(def); err != nil {
			t.Fatalf("readiness below the limit = %v, want ready", err)
		}

		time.Sleep(time.Minute)
		synctest.Wait()
		info = p.Workers()[0]
		if info.Status != StatusFailed || info.Restarts != 1 || runs.Load() != 2 {
			t.Fatalf("after the restarted run returned nil: %+v (runs %d), want failed after 2 runs, 1 restart", info, runs.Load())
		}
		if !info.LastSuccess.IsZero() {
			t.Fatalf("LastSuccess = %v, want zero for a continuous worker", info.LastSuccess)
		}
		if err := p.evaluateReadiness(def); err == nil || !strings.Contains(err.Error(), "failed permanently") {
			t.Fatalf("readiness = %v, want FailWhenFailed to report the dead worker", err)
		}
		if n := len(logs.withMessage("worker run failed")); n != 2 {
			t.Fatalf("%d 'worker run failed' lines, want 2 (one per unexpected exit)", n)
		}
		shutdownPool(t, p)
	})
}

func TestRunContinuous_FailuresKeepTheirDiagnostics(t *testing.T) {
	cases := []struct {
		name string
		run  Func
		want string
	}{
		{"error", func(context.Context) error { return errors.New("broker unreachable") }, "broker unreachable"},
		{"panic", func(context.Context) error { panic("kaboom") }, "worker: run panicked: kaboom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newTestPool()
				startPool(t, p, mustDefinition(t, "consumer", tc.run, WithRestartDelay(time.Minute)))
				synctest.Wait()
				info := p.Workers()[0]
				if info.Status != StatusWaiting || info.LastError != tc.want {
					t.Fatalf("%+v, want waiting with LastError %q", info, tc.want)
				}
				shutdownPool(t, p)
			})
		})
	}
}

func TestRunContinuous_NilAfterCancellationIsGraceful(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newTestPool()
		startPool(t, p, mustDefinition(t, "consumer", blockingFunc()))
		synctest.Wait()
		if info := p.Workers()[0]; info.Status != StatusRunning {
			t.Fatalf("the <-ctx.Done() idiom: status = %q, want running", info.Status)
		}
		shutdownPool(t, p)
		info := p.Workers()[0]
		if info.Status != StatusStopped || info.LastError != "" || info.Restarts != 0 {
			t.Fatalf("after shutdown: %+v, want stopped without an error", info)
		}
	})
}

func TestRunContinuous_MaxRestarts(t *testing.T) {
	t.Run("one restart", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newTestPool()
			var runs atomic.Int32
			startPool(t, p, mustDefinition(t, "consumer", Func(func(context.Context) error {
				runs.Add(1)
				return errors.New("boom")
			}), WithMaxRestarts(1), WithRestartDelay(time.Second)))
			time.Sleep(time.Minute)
			synctest.Wait()
			info := p.Workers()[0]
			if runs.Load() != 2 || info.Restarts != 1 || info.Status != StatusFailed {
				t.Fatalf("WithMaxRestarts(1): runs = %d, %+v, want 2 runs, 1 restart, failed", runs.Load(), info)
			}
			shutdownPool(t, p)
		})
	})
	t.Run("zero is unlimited and shutdown during the delay counts nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newTestPool()
			var runs atomic.Int32
			startPool(t, p, mustDefinition(t, "consumer", Func(func(context.Context) error {
				runs.Add(1)
				return errors.New("boom")
			}), WithRestartDelay(time.Second)))
			// Runs at 0s, 1s, 2s, 3s and 4s; the loop then waits for 5s.
			time.Sleep(4*time.Second + 500*time.Millisecond)
			synctest.Wait()
			info := p.Workers()[0]
			if runs.Load() != 5 || info.Restarts != 4 || info.Status != StatusWaiting {
				t.Fatalf("unlimited: runs = %d, %+v, want 5 runs, 4 restarts, waiting", runs.Load(), info)
			}
			shutdownPool(t, p)
			info = p.Workers()[0]
			if info.Restarts != 4 || info.Status != StatusStopped || runs.Load() != 5 {
				t.Fatalf("after shutdown in the delay: runs = %d, %+v, want the restart never counted", runs.Load(), info)
			}
		})
	})
}

func TestRunScheduled_NilDuringShutdownIsGraceful(t *testing.T) {
	for _, ret := range []string{"nil", "context error"} {
		t.Run(ret, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newTestPool()
				var runs atomic.Int32
				startPool(t, p, mustDefinition(t, "report", Func(func(ctx context.Context) error {
					if runs.Add(1) == 1 {
						return errors.New("boom")
					}
					<-ctx.Done()
					if ret == "nil" {
						return nil
					}
					return ctx.Err()
				}), WithSchedule("@every 1m")))

				time.Sleep(2 * time.Minute)
				synctest.Wait() // the second run is in flight
				shutdownPool(t, p)

				info := p.Workers()[0]
				if info.Status != StatusStopped || info.ConsecutiveFailures != 1 || !info.LastSuccess.IsZero() || info.LastError != "boom" {
					t.Fatalf("%+v, want stopped with the failure streak, LastError and LastSuccess unchanged", info)
				}
			})
		})
	}
}

func TestRunScheduled_RunTimeout(t *testing.T) {
	t.Run("timeout then nil or a context error is a failure", func(t *testing.T) {
		for _, ret := range []string{"nil", "context error"} {
			t.Run(ret, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					p := newTestPool()
					var sawTimeoutCause atomic.Bool
					def := mustDefinition(t, "report", Func(func(ctx context.Context) error {
						<-ctx.Done()
						sawTimeoutCause.Store(errors.Is(context.Cause(ctx), ErrRunTimeout))
						if ret == "nil" {
							return nil
						}
						return ctx.Err()
					}), WithSchedule("@every 1m"), WithRunTimeout(10*time.Second),
						WithReadiness(ReadinessPolicy{RequireFirstSuccess: true}))
					startPool(t, p, def)

					time.Sleep(time.Minute + 10*time.Second)
					synctest.Wait()
					info := p.Workers()[0]
					want := "worker: run timed out after 10s"
					if ret != "nil" {
						want += ": context deadline exceeded"
					}
					if info.Status != StatusWaiting || info.ConsecutiveFailures != 1 || info.LastError != want {
						t.Fatalf("%+v, want waiting, 1 failure, LastError %q", info, want)
					}
					if !info.LastSuccess.IsZero() {
						t.Fatal("a timed-out run stamped LastSuccess")
					}
					if err := p.evaluateReadiness(def); err == nil || !strings.Contains(err.Error(), "no successful run yet") {
						t.Fatalf("RequireFirstSuccess = %v, want still closed", err)
					}
					if !sawTimeoutCause.Load() {
						t.Fatal("Run did not see ErrRunTimeout as its context's cause")
					}
					shutdownPool(t, p)
				})
			})
		}
	})

	// blockUntilReleased times out and then holds the run until release is
	// closed, so a test can shut the pool down while the run is in flight.
	blockUntilReleased := func(release chan struct{}) Func {
		return func(ctx context.Context) error {
			<-ctx.Done()
			<-release
			return nil
		}
	}
	shutdownWhileBlocked := func(t *testing.T, p *Pool, release chan struct{}) {
		t.Helper()
		result := make(chan error, 1)
		go func() { result <- p.Shutdown(context.Background()) }()
		synctest.Wait() // the pool context is cancelled; Shutdown waits
		close(release)
		if err := <-result; err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
	}

	t.Run("timeout then shutdown below the limit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newTestPool()
			release := make(chan struct{})
			startPool(t, p, mustDefinition(t, "report", blockUntilReleased(release),
				WithSchedule("@every 1m"), WithRunTimeout(10*time.Second), WithMaxConsecutiveFailures(3)))
			time.Sleep(time.Minute + 20*time.Second)
			synctest.Wait()
			shutdownWhileBlocked(t, p, release)
			info := p.Workers()[0]
			if info.Status != StatusStopped || info.ConsecutiveFailures != 1 || info.LastError != "worker: run timed out after 10s" {
				t.Fatalf("%+v, want the timed-out failure recorded and a shutdown exit", info)
			}
		})
	})

	t.Run("timeout as the last allowed failure during shutdown", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newTestPool()
			release := make(chan struct{})
			startPool(t, p, mustDefinition(t, "report", blockUntilReleased(release),
				WithSchedule("@every 1m"), WithRunTimeout(10*time.Second), WithMaxConsecutiveFailures(1)))
			time.Sleep(time.Minute + 20*time.Second)
			synctest.Wait()
			shutdownWhileBlocked(t, p, release)
			if info := p.Workers()[0]; info.Status != StatusFailed || info.ConsecutiveFailures != 1 {
				t.Fatalf("%+v, want failed: it really was the last allowed failure", info)
			}
		})
	})

	t.Run("shutdown then deadline", func(t *testing.T) {
		for _, ret := range []string{"nil", "context error", "non-context error"} {
			t.Run(ret, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					p := newTestPool()
					startPool(t, p, mustDefinition(t, "report", Func(func(ctx context.Context) error {
						<-ctx.Done()
						time.Sleep(20 * time.Second) // outlives the deadline
						switch ret {
						case "nil":
							return nil
						case "context error":
							return ctx.Err()
						default:
							return errors.New("final batch write failed")
						}
					}), WithSchedule("@every 1m"), WithRunTimeout(10*time.Second)))
					time.Sleep(time.Minute + 5*time.Second)
					synctest.Wait()
					// No deadline on Shutdown: the run outlives the budget
					// on purpose.
					if err := p.Shutdown(context.Background()); err != nil {
						t.Fatalf("Shutdown() = %v", err)
					}

					info := p.Workers()[0]
					if ret == "non-context error" {
						if info.Status != StatusStopped || info.ConsecutiveFailures != 1 || info.LastError != "final batch write failed" {
							t.Fatalf("%+v, want the original diagnostic recorded as a failure", info)
						}
						return
					}
					if info.Status != StatusStopped || info.ConsecutiveFailures != 0 || info.LastError != "" {
						t.Fatalf("%+v, want a graceful stop: the pool was cancelled before the deadline", info)
					}
				})
			})
		}
	})

	t.Run("a run that ignores its context skips activations without overlap", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := newLogCapture()
			p := newPool(logs.logger(), DefaultRestartDelay)
			var runs, running atomic.Int32
			startPool(t, p, mustDefinition(t, "report", Func(func(context.Context) error {
				if running.Add(1) > 1 {
					t.Error("two runs overlapped")
				}
				defer running.Add(-1)
				if runs.Add(1) == 1 {
					time.Sleep(150 * time.Second) // ignores the 10s budget
				}
				return nil
			}), WithSchedule("@every 1m"), WithRunTimeout(10*time.Second)))

			time.Sleep(3*time.Minute + 50*time.Second) // first run: 1:00–3:30
			synctest.Wait()
			info := p.Workers()[0]
			if runs.Load() != 1 || info.LastError != "worker: run timed out after 10s" {
				t.Fatalf("runs = %d, %+v, want one timed-out run", runs.Load(), info)
			}
			if skips := logs.withMessage("worker ticks skipped"); len(skips) != 1 || skips[0].Attrs["skipped"] != int64(2) {
				t.Fatalf("skip lines = %+v, want one line for the 2:00 and 3:00 activations", skips)
			}
			time.Sleep(10 * time.Second) // 4:00
			synctest.Wait()
			if runs.Load() != 2 {
				t.Fatalf("runs = %d, want the 4:00 activation to run", runs.Load())
			}
			shutdownPool(t, p)
		})
	})

	t.Run("the startup run is bounded and the grid is kept", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			p := newTestPool()
			type call struct{ at, scheduledAt time.Time }
			calls := make(chan call, 4)
			startPool(t, p, mustDefinition(t, "report", Func(func(ctx context.Context) error {
				calls <- call{time.Now(), ScheduledAt(ctx)}
				<-ctx.Done()
				return ctx.Err()
			}), WithSchedule("@every 1m"), WithStartImmediately(), WithRunTimeout(5*time.Second)))

			synctest.Wait()
			first := <-calls
			if !first.scheduledAt.IsZero() || !first.at.Equal(start) {
				t.Fatalf("startup run at %v scheduled %v, want immediately with a zero ScheduledAt", first.at, first.scheduledAt)
			}
			time.Sleep(5 * time.Second)
			synctest.Wait()
			if info := p.Workers()[0]; info.ConsecutiveFailures != 1 || info.LastError != "worker: run timed out after 5s: context deadline exceeded" {
				t.Fatalf("startup run: %+v, want a timed-out failure after 5s", info)
			}

			time.Sleep(time.Minute) // the 1:05 activation; its run times out at 1:10
			synctest.Wait()
			second := <-calls
			time.Sleep(time.Minute)
			synctest.Wait()
			third := <-calls
			if got := third.at.Sub(second.at); got != time.Minute {
				t.Fatalf("activations %v and %v are %s apart, want 1m: a timeout must not shift the grid", second.at, third.at, got)
			}
			if !third.scheduledAt.Equal(third.at) {
				t.Fatalf("ScheduledAt = %v, want the activation time %v", third.scheduledAt, third.at)
			}
			shutdownPool(t, p)
		})
	})
}

func TestRunScheduled_PanicsAreFailures(t *testing.T) {
	t.Run("no stack in LastError or readiness", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newTestPool()
			def := mustDefinition(t, "report", Func(func(context.Context) error { panic("kaboom") }),
				WithSchedule("@every 1m"), WithMaxConsecutiveFailures(1),
				WithReadiness(ReadinessPolicy{FailWhenFailed: true}))
			startPool(t, p, def)
			time.Sleep(time.Minute)
			synctest.Wait()
			info := p.Workers()[0]
			if info.Status != StatusFailed || info.LastError != "worker: run panicked: kaboom" {
				t.Fatalf("%+v, want failed with the panic value only", info)
			}
			err := p.evaluateReadiness(def)
			if err == nil || strings.Contains(err.Error(), "goroutine") || !strings.Contains(err.Error(), "kaboom") {
				t.Fatalf("readiness = %v, want the panic value without a stack", err)
			}
			shutdownPool(t, p)
		})
	})

	t.Run("panic(ctx.Err()) during shutdown is not a graceful stop", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newTestPool()
			startPool(t, p, mustDefinition(t, "consumer", Func(func(ctx context.Context) error {
				<-ctx.Done()
				panic(ctx.Err())
			}), WithMaxRestarts(1)))
			synctest.Wait()
			shutdownPool(t, p)
			info := p.Workers()[0]
			if info.Status != StatusStopped || info.LastError != "worker: run panicked: context canceled" {
				t.Fatalf("%+v, want the panic recorded as a failure", info)
			}
		})
	})
}

func TestRegister_RunTimeoutValidation(t *testing.T) {
	app := newTestApp(t)
	requireErrContaining(t, Register(app, "consumer", blockingFunc(), WithRunTimeout(time.Second)),
		"WithRunTimeout is for scheduled workers")
	requireErrContaining(t, Register(app, "report", scheduledOnce(), WithSchedule("@every 1m"), WithRunTimeout(-time.Second)),
		"run timeout must be >= 0")
	if err := Register(app, "report", scheduledOnce(), WithSchedule("@every 1m"), WithRunTimeout(0)); err != nil {
		t.Fatalf("WithRunTimeout(0) = %v, want accepted (no timeout)", err)
	}
}

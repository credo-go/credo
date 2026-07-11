package health

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

const probeTestTimeout = 5 * time.Second

type panicTextError struct{}

func (*panicTextError) Error() string { panic("error text exploded") }

type blockingTextError struct {
	release <-chan struct{}
}

func (e *blockingTextError) Error() string {
	<-e.release
	return "released cause"
}

func TestProbeRun_NonCooperativeCheckTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, releaseCheck := probeRelease(t)
		started := make(chan struct{})
		probe := NewProbe(func(context.Context) Result {
			close(started)
			<-release
			return Result{Status: "up"}
		})

		resultCh := make(chan Result, 1)
		start := time.Now()
		go func() {
			resultCh <- probe.Run(t.Context(), probeTestTimeout)
		}()

		<-started
		synctest.Sleep(probeTestTimeout)
		result := requireProbeResult(t, resultCh)
		requireProbeFailure(t, result, context.DeadlineExceeded)
		if elapsed := time.Since(start); elapsed != probeTestTimeout {
			t.Fatalf("Run elapsed = %s, want %s", elapsed, probeTestTimeout)
		}

		releaseCheck()
		synctest.Wait()
	})
}

func TestProbeRun_ConcurrentAndRepeatedWaitersShareFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, releaseCheck := probeRelease(t)
		started := make(chan struct{})
		var calls atomic.Int32
		var active atomic.Int32
		probe := NewProbe(func(context.Context) Result {
			if calls.Add(1) == 1 {
				close(started)
			}
			active.Add(1)
			defer active.Add(-1)
			<-release
			return Result{Status: "up"}
		})

		const waiterCount = 16
		firstWave := runProbeWaiters(t.Context(), probe, waiterCount, probeTestTimeout)
		<-started
		synctest.Sleep(probeTestTimeout)
		requireProbeTimeouts(t, firstWave, waiterCount)

		if got := calls.Load(); got != 1 {
			t.Fatalf("check invocations after concurrent waiters = %d, want 1", got)
		}
		if got := active.Load(); got != 1 {
			t.Fatalf("active checks after waiter timeouts = %d, want 1", got)
		}

		// The first wave timed out, but its non-cooperative check is still
		// running. Later probes must join that physical flight rather than start
		// one new goroutine per request.
		secondWave := runProbeWaiters(t.Context(), probe, waiterCount, probeTestTimeout)
		synctest.Sleep(probeTestTimeout)
		requireProbeTimeouts(t, secondWave, waiterCount)

		if got := calls.Load(); got != 1 {
			t.Fatalf("check invocations while timed-out flight is still running = %d, want 1", got)
		}
		if got := active.Load(); got != 1 {
			t.Fatalf("active checks while timed-out flight is still running = %d, want 1", got)
		}

		releaseCheck()
		synctest.Wait()
		if got := active.Load(); got != 0 {
			t.Fatalf("active checks after release = %d, want 0", got)
		}
	})
}

func TestProbeRun_LateSuccessDoesNotReplaceTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, releaseCheck := probeRelease(t)
		started := make(chan struct{})
		secondCause := errors.New("second flight failed")
		var calls atomic.Int32
		probe := NewProbe(func(context.Context) Result {
			switch calls.Add(1) {
			case 1:
				close(started)
				<-release
				return Result{Status: "up"}
			default:
				return Result{Status: "down", Cause: secondCause}
			}
		})

		firstCh := make(chan Result, 1)
		go func() {
			firstCh <- probe.Run(t.Context(), probeTestTimeout)
		}()
		<-started
		synctest.Sleep(probeTestTimeout)
		first := requireProbeResult(t, firstCh)
		requireProbeFailure(t, first, context.DeadlineExceeded)

		// The abandoned worker eventually reports success. That late result must
		// neither mutate the already-published timeout nor become a cached result
		// for the next waiter.
		releaseCheck()
		synctest.Wait()
		requireProbeFailure(t, first, context.DeadlineExceeded)

		second := probe.Run(t.Context(), probeTestTimeout)
		requireProbeFailure(t, second, secondCause)
		if got := calls.Load(); got != 2 {
			t.Fatalf("check invocations = %d, want 2", got)
		}
	})
}

func TestProbeRun_ReleaseAllowsNewFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, releaseCheck := probeRelease(t)
		started := make(chan struct{})
		var calls atomic.Int32
		probe := NewProbe(func(context.Context) Result {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}
			return Result{Status: "up"}
		})

		firstCh := make(chan Result, 1)
		go func() {
			firstCh <- probe.Run(t.Context(), probeTestTimeout)
		}()
		<-started
		synctest.Sleep(probeTestTimeout)
		requireProbeFailure(t, requireProbeResult(t, firstCh), context.DeadlineExceeded)

		releaseCheck()
		synctest.Wait()

		second := probe.Run(t.Context(), probeTestTimeout)
		if second.Status != "up" {
			t.Fatalf("second flight status = %q, want up (cause: %v)", second.Status, second.Cause)
		}
		if second.Cause != nil {
			t.Fatalf("second flight cause = %v, want nil", second.Cause)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("check invocations = %d, want 2", got)
		}
	})
}

func TestProbeRun_PanicIsolatedAndReleasesFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		probe := NewProbe(func(context.Context) Result {
			if calls.Add(1) == 1 {
				panic("probe exploded")
			}
			return Result{Status: "up"}
		})

		first := probe.Run(t.Context(), probeTestTimeout)
		if first.Status != "down" {
			t.Fatalf("panic result status = %q, want down", first.Status)
		}
		if first.Cause == nil {
			t.Fatal("panic result cause = nil, want recovered panic error")
		}
		if !strings.Contains(first.Cause.Error(), "probe exploded") {
			t.Fatalf("panic result cause = %q, want panic value", first.Cause)
		}

		// A completed flight is not a cache: once Run returns, the next call
		// must start a fresh execution without needing scheduler settlement.
		second := probe.Run(t.Context(), probeTestTimeout)
		if second.Status != "up" {
			t.Fatalf("second flight status = %q, want up (cause: %v)", second.Status, second.Cause)
		}
		if second.Cause != nil {
			t.Fatalf("second flight cause = %v, want nil", second.Cause)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("check invocations = %d, want 2", got)
		}
	})
}

func TestProbeRun_ParentCancellationBoundsWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, releaseCheck := probeRelease(t)
		started := make(chan struct{})
		probe := NewProbe(func(context.Context) Result {
			close(started)
			<-release
			return Result{Status: "up"}
		})

		waiterCtx, cancel := context.WithCancel(t.Context())
		resultCh := make(chan Result, 1)
		start := time.Now()
		go func() {
			resultCh <- probe.Run(waiterCtx, time.Hour)
		}()

		<-started
		cancel()
		synctest.Wait()
		result := requireProbeResult(t, resultCh)
		requireProbeFailure(t, result, context.Canceled)
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("Run elapsed after parent cancellation = %s, want 0", elapsed)
		}

		releaseCheck()
		synctest.Wait()
	})
}

func TestProbeRun_CanceledWaiterDoesNotCancelSharedFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, releaseCheck := probeRelease(t)
		started := make(chan struct{})
		var calls atomic.Int32
		probe := NewProbe(func(ctx context.Context) Result {
			calls.Add(1)
			close(started)
			select {
			case <-release:
				return Result{Status: "up"}
			case <-ctx.Done():
				return Result{Status: "down", Cause: ctx.Err()}
			}
		})

		canceledCtx, cancel := context.WithCancel(t.Context())
		firstResult := make(chan Result, 1)
		secondResult := make(chan Result, 1)
		go func() { firstResult <- probe.Run(canceledCtx, probeTestTimeout) }()
		<-started
		go func() { secondResult <- probe.Run(t.Context(), probeTestTimeout) }()
		synctest.Wait()

		cancel()
		synctest.Wait()
		requireProbeFailure(t, requireProbeResult(t, firstResult), context.Canceled)

		releaseCheck()
		synctest.Wait()
		second := requireProbeResult(t, secondResult)
		if second.Status != "up" || second.Cause != nil {
			t.Fatalf("shared waiter result = %#v, want up", second)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("check invocations = %d, want one shared flight", got)
		}
	})
}

func TestProbeRun_JoiningWaiterUsesItsOwnShorterTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, releaseCheck := probeRelease(t)
		started := make(chan struct{})
		probe := NewProbe(func(context.Context) Result {
			close(started)
			<-release
			return Result{Status: "up"}
		})

		firstResult := make(chan Result, 1)
		secondResult := make(chan Result, 1)
		go func() { firstResult <- probe.Run(t.Context(), 10*time.Second) }()
		<-started
		go func() { secondResult <- probe.Run(t.Context(), 2*time.Second) }()

		synctest.Sleep(2 * time.Second)
		requireProbeFailure(t, requireProbeResult(t, secondResult), context.DeadlineExceeded)

		releaseCheck()
		synctest.Wait()
		first := requireProbeResult(t, firstResult)
		if first.Status != "up" {
			t.Fatalf("first waiter result = %#v, want up", first)
		}
	})
}

func TestProbeRun_CauseTextPanicIsIsolated(t *testing.T) {
	probe := NewProbe(func(context.Context) Result {
		return Result{Status: "down", Cause: &panicTextError{}}
	})

	result := probe.Run(t.Context(), probeTestTimeout)
	if result.Status != "down" || !strings.Contains(result.Error, "error text exploded") {
		t.Fatalf("result = %#v, want recovered Cause.Error panic", result)
	}
}

func TestProbeRun_BlockingCauseTextIsBoundedAndShared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, releaseCheck := probeRelease(t)
		var calls atomic.Int32
		probe := NewProbe(func(context.Context) Result {
			calls.Add(1)
			return Result{Status: "down", Cause: &blockingTextError{release: release}}
		})

		firstResult := make(chan Result, 1)
		go func() { firstResult <- probe.Run(t.Context(), probeTestTimeout) }()
		synctest.Sleep(probeTestTimeout)
		requireProbeFailure(t, requireProbeResult(t, firstResult), context.DeadlineExceeded)

		second := probe.Run(t.Context(), probeTestTimeout)
		requireProbeFailure(t, second, context.DeadlineExceeded)
		if got := calls.Load(); got != 1 {
			t.Fatalf("check invocations = %d, want one flight blocked in Cause.Error", got)
		}

		releaseCheck()
		synctest.Wait()
	})
}

func probeRelease(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	releaseCheck := func() {
		once.Do(func() { close(release) })
	}
	t.Cleanup(releaseCheck)
	return release, releaseCheck
}

func runProbeWaiters(ctx context.Context, probe *Probe, count int, timeout time.Duration) <-chan Result {
	results := make(chan Result, count)
	for range count {
		go func() {
			results <- probe.Run(ctx, timeout)
		}()
	}
	return results
}

func requireProbeTimeouts(t *testing.T, results <-chan Result, count int) {
	t.Helper()
	for range count {
		requireProbeFailure(t, requireProbeResult(t, results), context.DeadlineExceeded)
	}
}

func requireProbeResult(t *testing.T, results <-chan Result) Result {
	t.Helper()
	select {
	case result := <-results:
		return result
	default:
		t.Fatal("Probe.Run did not return within the expected bound")
		return Result{}
	}
}

func requireProbeFailure(t *testing.T, result Result, want error) {
	t.Helper()
	if result.Status != "down" {
		t.Fatalf("result status = %q, want down (cause: %v)", result.Status, result.Cause)
	}
	if !errors.Is(result.Cause, want) {
		t.Fatalf("result cause = %v, want errors.Is(_, %v)", result.Cause, want)
	}
}

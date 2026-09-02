package health

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var errProbeDidNotReturn = errors.New("health: probe exited without returning")

// Result is the transport-neutral outcome of one health probe execution.
// Cause is internal diagnostic state and must never be serialized implicitly.
type Result struct {
	Status  string
	Latency time.Duration
	Cause   error `json:"-"`
	// Error is the Cause text captured inside the bounded probe worker. Root
	// logging and rendering use this immutable string and never invoke an
	// arbitrary Cause.Error method outside the timeout/panic boundary.
	Error string `json:"-"`
}

// Probe runs one health callback at a time. Concurrent callers join the same
// immutable flight. A callback that ignores cancellation can outlive its
// deadline, but it cannot create an unbounded goroutine per readiness request:
// the timed-out flight remains attached until that callback actually exits.
type Probe struct {
	check func(context.Context) Result

	mu     sync.Mutex
	flight *probeFlight
}

type probeFlight struct {
	started time.Time
	done    chan struct{}
	result  Result
}

// NewProbe creates a stable probe for check.
func NewProbe(check func(context.Context) Result) *Probe {
	return &Probe{check: check}
}

// Run joins or starts the probe's current flight and waits for either its
// immutable result or this waiter's timeout/cancellation. The first caller's
// timeout bounds the shared callback execution; every joining caller also
// applies its own timeout as a maximum wait. The callback context is detached
// from the first waiter's cancellation, so one canceled HTTP request cannot
// invalidate a flight already shared by another readiness request.
func (p *Probe) Run(ctx context.Context, timeout time.Duration) Result {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return failureResult(err, 0)
	}
	if p == nil {
		return failureResult(errors.New("health: nil probe"), 0)
	}
	if timeout <= 0 {
		return failureResult(context.DeadlineExceeded, 0)
	}

	waiterCtx, cancelWait := context.WithTimeout(ctx, timeout)
	defer cancelWait()

	flight := p.joinOrStart(ctx, timeout)
	select {
	case <-flight.done:
		return flight.result
	case <-waiterCtx.Done():
		return failureResult(waiterCtx.Err(), time.Since(flight.started))
	}
}

func (p *Probe) joinOrStart(ctx context.Context, timeout time.Duration) *probeFlight {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.flight != nil {
		return p.flight
	}

	flight := &probeFlight{
		started: time.Now(),
		done:    make(chan struct{}),
	}
	p.flight = flight

	executionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	go p.execute(flight, executionCtx, cancel)
	return flight
}

func (p *Probe) execute(flight *probeFlight, ctx context.Context, cancel context.CancelFunc) {
	workerResult := make(chan Result, 1)
	go p.call(workerResult, ctx)

	select {
	case result := <-workerResult:
		if err := ctx.Err(); err != nil {
			result = failureResult(err, time.Since(flight.started))
		}
		cancel()
		p.complete(flight, result)
	case <-ctx.Done():
		cancel()
		p.publishRetained(flight, failureResult(ctx.Err(), time.Since(flight.started)))

		// Do not clear the flight at the deadline. A non-cooperative callback
		// is still running, and clearing here would let every new probe start
		// another permanently blocked goroutine. Its late result is discarded.
		<-workerResult
		p.clear(flight)
	}
}

func (p *Probe) call(result chan<- Result, ctx context.Context) {
	completed := false
	probeResult := Result{}
	defer func() {
		if recovered := recover(); recovered != nil {
			probeResult = failureResult(fmt.Errorf("panic: %v", recovered), 0)
		} else if !completed {
			probeResult = failureResult(errProbeDidNotReturn, 0)
		}
		result <- probeResult
	}()

	if p.check == nil {
		probeResult = failureResult(errors.New("health: nil probe callback"), 0)
		completed = true
		return
	}
	probeResult = p.check(ctx)
	if probeResult.Cause != nil {
		// This call is deliberately inside the worker. A Cause.Error method
		// that panics is recovered above; one that blocks is bounded by the
		// coordinator deadline like the rest of the callback.
		probeResult.Error = probeResult.Cause.Error()
	}
	completed = true
}

// SuccessResult is the "up" result a contributed probe returns when its
// condition holds.
func SuccessResult() Result { return Result{Status: "up"} }

// FailureResult is the "down" result for cause, with the cause text captured
// eagerly the way the bounded runner does for named checks.
func FailureResult(cause error) Result { return failureResult(cause, 0) }

func failureResult(cause error, latency time.Duration) Result {
	return Result{
		Status:  "down",
		Latency: latency,
		Cause:   cause,
		Error:   cause.Error(),
	}
}

// complete publishes a normal/panic result and detaches the finished flight in
// one mutex critical section. Existing waiters observe the channel close;
// callers arriving after completion cannot join a cached result and instead
// start a new flight after the mutex is released.
func (p *Probe) complete(flight *probeFlight, result Result) {
	p.mu.Lock()
	flight.result = result
	close(flight.done)
	if p.flight == flight {
		p.flight = nil
	}
	p.mu.Unlock()
}

// publishRetained publishes a deadline result while retaining the physical
// flight. New callers observe the immutable timeout immediately but cannot
// launch another callback until the non-cooperative worker actually exits.
func (p *Probe) publishRetained(flight *probeFlight, result Result) {
	p.mu.Lock()
	flight.result = result
	close(flight.done)
	p.mu.Unlock()
}

func (p *Probe) clear(flight *probeFlight) {
	p.mu.Lock()
	if p.flight == flight {
		p.flight = nil
	}
	p.mu.Unlock()
}

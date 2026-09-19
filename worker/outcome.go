package worker

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"
)

// errUnexpectedExit is recorded when a continuous worker's Run returns nil
// while the pool is still running. The message states the contract because
// the failure log line may be the only place an upgrader learns about it.
var errUnexpectedExit = errors.New(
	"worker: Run returned nil before shutdown; a continuous worker must run until its context is cancelled")

// panicError is a recovered panic of a worker run. Its message carries the
// panic value only; the stack travels separately, as a log attribute, so it
// never reaches Info.LastError or a readiness failure.
//
// It deliberately has no Unwrap: a panic is a failure whatever its value
// wraps, and panic(ctx.Err()) during shutdown must not pass for a graceful
// stop.
type panicError struct {
	value any
	stack []byte
}

func (e *panicError) Error() string {
	return fmt.Sprintf("worker: run panicked: %v", e.value)
}

// safeRun runs w, turning a panic into a *panicError.
func safeRun(ctx context.Context, w Worker) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &panicError{value: r, stack: debug.Stack()}
		}
	}()
	return w.Run(ctx)
}

// runVerdict is what one run amounts to for the worker's counters.
type runVerdict int

const (
	// runFailed counts toward the restart or failure limit.
	runFailed runVerdict = iota
	// runSucceeded is a scheduled run that returned nil while the pool ran.
	runSucceeded
	// runStopped is a graceful stop: neither a success nor a failure.
	runStopped
)

// runInput is everything classifyRun needs to know about one finished run.
type runInput struct {
	kind Kind
	// err is what Run returned, or the *panicError safeRun recovered.
	err error
	// timedOut reports that the run context's cancellation cause was
	// ErrRunTimeout, read before the run context was cancelled.
	timedOut bool
	// timeout is the WithRunTimeout budget, for the recorded error.
	timeout time.Duration
	// poolDone reports that the pool context was done when Run returned.
	poolDone bool
}

// runOutcome is the classified result of one run.
type runOutcome struct {
	verdict runVerdict
	// err is the error to record for a failure; nil otherwise.
	err error
	// timedOut marks a failure caused by the run timeout.
	timedOut bool
	// unexpectedExit marks a continuous worker's nil return while alive.
	unexpectedExit bool
	// stack is the panic stack of a recovered panic.
	stack []byte
}

// classifyRun decides what one run amounts to. The rules are evaluated in
// order, and the first that matches wins:
//
//  1. Run panicked → failure, whatever the panic value wraps.
//  2. The run context was cancelled by the run timeout → timed-out failure,
//     whatever Run returned, nil included.
//  3. The pool context is done and Run returned nil or nothing but a context
//     error → graceful stop.
//  4. Run returned an error → failure.
//  5. A scheduled run returned nil → success.
//  6. A continuous run returned nil while the pool is alive → unexpected-exit
//     failure.
//
// Why the loop ends is decided separately, after the outcome is recorded.
func classifyRun(in runInput) runOutcome {
	if p, ok := errors.AsType[*panicError](in.err); ok {
		return runOutcome{verdict: runFailed, err: p, stack: p.stack}
	}
	if in.timedOut {
		err := fmt.Errorf("%w after %s", ErrRunTimeout, in.timeout)
		if in.err != nil {
			err = fmt.Errorf("%w after %s: %w", ErrRunTimeout, in.timeout, in.err)
		}
		return runOutcome{verdict: runFailed, err: err, timedOut: true}
	}
	if in.poolDone && (in.err == nil || isContextError(in.err)) {
		return runOutcome{verdict: runStopped}
	}
	if in.err != nil {
		return runOutcome{verdict: runFailed, err: in.err}
	}
	if in.kind == KindScheduled {
		return runOutcome{verdict: runSucceeded}
	}
	return runOutcome{verdict: runFailed, err: errUnexpectedExit, unexpectedExit: true}
}

// isContextError reports whether err is a context error and nothing else:
// every branch of its unwrap/join tree must end in context.Canceled or
// context.DeadlineExceeded. A single wrap chain qualifies
// (fmt.Errorf("query: %w", ctx.Err()), a *url.Error around a cancelled dial);
// a joined error qualifies only when each of its branches does. errors.Is
// would accept errors.Join(ctx.Err(), flushErr) on the strength of one branch
// and lose the flush error, which alone would have been a failure.
func isContextError(err error) bool {
	if err == nil {
		return false
	}
	// A node with children is judged by its children alone: its own Is
	// method could vouch for a tree that also carries another error.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		branches := 0
		for _, child := range joined.Unwrap() {
			if child == nil {
				continue
			}
			if !isContextError(child) {
				return false
			}
			branches++
		}
		if branches > 0 {
			return true
		}
	} else if child := errors.Unwrap(err); child != nil {
		return isContextError(child)
	}
	return isContextErrorNode(err)
}

// isContextErrorNode inspects a leaf, an error that wraps nothing: the
// explicit traversal in isContextError must see every branch, so errors.Is
// cannot be used here. An Is method is honored the way errors.Is honors it,
// which is how the net package reports a cancelled dial.
func isContextErrorNode(err error) bool {
	if err == context.Canceled || err == context.DeadlineExceeded { //nolint:errorlint // This leaf only; see above.
		return true
	}
	if x, ok := err.(interface{ Is(error) bool }); ok { //nolint:errorlint // This leaf only; see above.
		return x.Is(context.Canceled) || x.Is(context.DeadlineExceeded)
	}
	return false
}

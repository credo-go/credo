package worker

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

type runner struct {
	def *Definition

	mu        sync.Mutex
	status    Status
	attempts  int64
	lastRun   time.Time
	lastError string
}

func newRunner(def *Definition) *runner {
	return &runner{def: def, status: StatusIdle}
}

func (r *runner) setStatus(status Status) {
	r.mu.Lock()
	r.status = status
	r.mu.Unlock()
}

func (r *runner) setOutcome(status Status, err error) {
	r.update(func(r *runner) {
		r.status = status
		if err != nil {
			r.lastError = err.Error()
			return
		}
		r.lastError = ""
	})
}

func (r *runner) setAttemptOutcome(status Status, attempts int64, err error) {
	r.update(func(r *runner) {
		r.attempts = attempts
		r.status = status
		if err != nil {
			r.lastError = err.Error()
			return
		}
		r.lastError = ""
	})
}

func (r *runner) update(fn func(*runner)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r)
}

func (r *runner) stopIfNotFailed(clearStaleError bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == StatusFailed {
		return false
	}
	if clearStaleError {
		r.lastError = ""
	}
	r.status = StatusStopped
	return true
}

func (r *runner) startRun(startedAt time.Time) {
	r.update(func(r *runner) {
		r.status = StatusRunning
		r.lastRun = startedAt
	})
}

func (r *runner) snapshot() Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Info{
		Name:      r.def.name,
		Kind:      r.def.Kind(),
		Schedule:  r.def.scheduleExpr(),
		Status:    r.status,
		Attempts:  r.attempts,
		LastRun:   r.lastRun,
		LastError: r.lastError,
	}
}

func (p *Pool) runContinuous(ctx context.Context, r *runner) {
	p.driveLoop(ctx, r, &continuousPolicy{p: p, r: r})
}

// runScheduled drives a scheduled worker with a single goroutine:
// sleep until the next activation, run it synchronously, recompute.
// Activations that pass while a run is still in flight are skipped (and
// logged), never queued — the same skip-if-still-running semantics the
// earlier two-goroutine model provided via a non-blocking tick handoff.
func (p *Pool) runScheduled(ctx context.Context, r *runner) {
	p.driveLoop(ctx, r, &scheduledPolicy{p: p, r: r})
}

// waitNone tells driveLoop to start the next attempt without a timer.
const waitNone = time.Duration(-1)

// loopPolicy is the kind-specific half of the worker loop. driveLoop owns the
// shared skeleton — the timer wait, cancellation while waiting, run
// bookkeeping, and panic-safe execution — and the policy decides when the
// next attempt runs, how it is numbered, and what its outcome means.
type loopPolicy interface {
	// start returns the wait before the first attempt (waitNone for none) or
	// stop=true when the loop must not run at all.
	start(ctx context.Context) (wait time.Duration, stop bool)

	// beforeRun runs once the wait has elapsed and returns the attempt number
	// and the activation time recorded in the run context; ok=false stops the
	// loop after the policy recorded the terminal status.
	beforeRun(ctx context.Context) (attempt int, scheduledAt time.Time, ok bool)

	// afterRun classifies the outcome of one attempt and returns the wait
	// before the next one (waitNone for none) or stop=true.
	afterRun(ctx context.Context, err error) (wait time.Duration, stop bool)
}

// driveLoop runs attempts under policy until it stops. Cancellation while
// waiting preserves the last execution snapshot: the worker is stopped only
// if it has not already transitioned to Failed.
func (p *Pool) driveLoop(ctx context.Context, r *runner, policy loopPolicy) {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	wait, stop := policy.start(ctx)
	for !stop {
		if wait != waitNone {
			timer.Reset(wait)
			select {
			case <-ctx.Done():
				r.stopIfNotFailed(false)
				return
			case <-timer.C:
			}
		}

		attempt, scheduledAt, ok := policy.beforeRun(ctx)
		if !ok {
			return
		}
		r.startRun(time.Now())
		runCtx := enrichContext(ctx, r.def.name, attempt, scheduledAt, newRunID())
		wait, stop = policy.afterRun(ctx, safeRun(runCtx, r.def.worker))
	}
}

// continuousPolicy restarts a continuous worker after each failure, with the
// configured delay and restart limit; a clean return stops it.
type continuousPolicy struct {
	p        *Pool
	r        *runner
	restarts int64
}

func (c *continuousPolicy) start(context.Context) (time.Duration, bool) {
	return waitNone, false
}

func (c *continuousPolicy) beforeRun(ctx context.Context) (int, time.Time, bool) {
	if ctx.Err() != nil {
		c.r.setOutcome(StatusStopped, nil)
		return 0, time.Time{}, false
	}
	return int(c.restarts) + 1, time.Time{}, true
}

func (c *continuousPolicy) afterRun(ctx context.Context, err error) (time.Duration, bool) {
	r, p := c.r, c.p
	if err == nil {
		r.setOutcome(StatusStopped, nil)
		return waitNone, true
	}
	if isGracefulStop(err, ctx) {
		r.setOutcome(StatusStopped, nil)
		p.logger.InfoContext(ctx, "worker stopped", "worker", r.def.name, "kind", r.def.Kind())
		return waitNone, true
	}

	c.restarts++
	p.logger.ErrorContext(ctx,
		"worker run failed",
		"worker", r.def.name,
		"kind", r.def.Kind(),
		"restart", c.restarts,
		"error", err,
	)

	if max := r.def.restartPolicy.maxRestarts; max > 0 && c.restarts >= int64(max) {
		r.setAttemptOutcome(StatusFailed, c.restarts, err)
		p.logger.ErrorContext(ctx,
			"worker exceeded max restarts",
			"worker", r.def.name,
			"kind", r.def.Kind(),
			"max_restarts", max,
		)
		return waitNone, true
	}

	r.setAttemptOutcome(StatusWaiting, c.restarts, err)
	return r.def.restartPolicy.restartDelay, false
}

// scheduledPolicy runs one activation at a time and computes the next one
// from the last intended activation (the anchor), so a long run delays the
// next fire but does not shift the grid.
type scheduledPolicy struct {
	p                   *Pool
	r                   *runner
	consecutiveFailures int64
	anchor              time.Time
	next                time.Time
	synthetic           bool // the pending attempt is the startup run (WithStartImmediately)
}

func (s *scheduledPolicy) start(ctx context.Context) (time.Duration, bool) {
	if s.r.def.startImmediately {
		s.synthetic = true
		return waitNone, false
	}
	s.anchor = time.Now()
	return s.scheduleNext(ctx)
}

func (s *scheduledPolicy) beforeRun(ctx context.Context) (int, time.Time, bool) {
	if s.synthetic {
		if ctx.Err() != nil {
			s.r.stopIfNotFailed(false)
			return 0, time.Time{}, false
		}
		// Synthetic startup run: ScheduledAt is the zero time.
		return 1, time.Time{}, true
	}
	return 1, s.next, true
}

func (s *scheduledPolicy) afterRun(ctx context.Context, err error) (time.Duration, bool) {
	intendedTime := s.next
	if s.synthetic {
		intendedTime = time.Time{}
	}
	if s.finishRun(ctx, intendedTime, err) {
		return waitNone, true
	}
	if s.synthetic {
		s.synthetic = false
		s.anchor = time.Now()
	} else {
		s.anchor = s.next
	}
	return s.scheduleNext(ctx)
}

// finishRun records one activation's outcome and reports whether the
// scheduling loop should stop (graceful stop or permanent failure).
func (s *scheduledPolicy) finishRun(ctx context.Context, intendedTime time.Time, err error) (stop bool) {
	r, p := s.r, s.p
	if isGracefulStop(err, ctx) {
		r.setOutcome(StatusStopped, nil)
		p.logger.InfoContext(ctx,
			"worker stopped during scheduled run",
			"worker", r.def.name,
		)
		return true
	}

	if err == nil {
		s.consecutiveFailures = 0
		status := StatusWaiting
		if ctx.Err() != nil {
			status = StatusStopped
		}
		r.setAttemptOutcome(status, 0, nil)
		return false
	}

	s.consecutiveFailures++
	p.logger.ErrorContext(ctx,
		"scheduled worker run failed",
		"worker", r.def.name,
		"scheduled_at", intendedTime,
		"consecutive_failures", s.consecutiveFailures,
		"error", err,
	)

	if max := r.def.failurePolicy.maxConsecutiveFailures; max > 0 && s.consecutiveFailures >= int64(max) {
		r.setAttemptOutcome(StatusFailed, s.consecutiveFailures, err)
		p.logger.ErrorContext(ctx,
			"worker exceeded max consecutive failures",
			"worker", r.def.name,
			"max_consecutive_failures", max,
		)
		return true
	}

	if ctx.Err() != nil {
		r.setAttemptOutcome(StatusStopped, s.consecutiveFailures, err)
		return true
	}

	r.setAttemptOutcome(StatusWaiting, s.consecutiveFailures, err)
	return false
}

// scheduleNext computes the next activation from the anchor, skipping (and
// logging) activations the previous run outlasted, and returns the wait until
// it. A schedule with no future activation marks the worker Failed and stops.
func (s *scheduledPolicy) scheduleNext(ctx context.Context) (time.Duration, bool) {
	r, p := s.r, s.p
	next := r.def.schedule.Next(s.anchor)
	for !next.IsZero() && !next.After(time.Now()) {
		// The previous run outlasted this activation — skip it, exactly
		// like a busy executor skipped ticks in the two-goroutine model.
		p.logger.WarnContext(ctx,
			"worker tick skipped",
			"worker", r.def.name,
			"scheduled_at", next,
		)
		next = r.def.schedule.Next(next)
	}
	if next.IsZero() {
		r.update(func(r *runner) {
			r.lastError = "schedule has no future activation"
			r.status = StatusFailed
		})
		p.logger.ErrorContext(ctx,
			"worker schedule has no future activation",
			"worker", r.def.name,
			"schedule", r.def.scheduleExpr(),
		)
		return waitNone, true
	}
	s.next = next
	return time.Until(next), false
}

func safeRun(ctx context.Context, w Worker) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("worker %q panicked: %v\n%s", w.Name(), r, debug.Stack())
		}
	}()
	return w.Run(ctx)
}

func isGracefulStop(err error, parentCtx context.Context) bool {
	if err == nil || parentCtx == nil || parentCtx.Err() == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

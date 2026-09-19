package worker

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

type runner struct {
	def    *definition
	worker Worker // resolved once by Pool.Start

	mu sync.Mutex
	st runState
}

// runState is the live half of Info; runner.mu guards it.
type runState struct {
	status              Status
	restarts            int64
	consecutiveFailures int64
	lastRun             time.Time
	lastSuccess         time.Time
	lastError           string
}

func newRunner(def *definition, w Worker) *runner {
	return &runner{def: def, worker: w, st: runState{status: StatusIdle}}
}

func (r *runner) setStatus(status Status) {
	r.update(func(st *runState) { st.status = status })
}

func (r *runner) setOutcome(status Status, err error) {
	r.update(func(st *runState) {
		st.status = status
		st.lastError = errorText(err)
	})
}

// setFailureOutcome records a scheduled run's outcome with the failure streak.
func (r *runner) setFailureOutcome(status Status, consecutiveFailures int64, err error) {
	r.update(func(st *runState) {
		st.consecutiveFailures = consecutiveFailures
		st.status = status
		st.lastError = errorText(err)
	})
}

// recordSuccess records a successful scheduled run: it stamps LastSuccess,
// resets the failure streak and clears LastError.
func (r *runner) recordSuccess(at time.Time) {
	r.update(func(st *runState) {
		st.status = StatusWaiting
		st.consecutiveFailures = 0
		st.lastSuccess = at
		st.lastError = ""
	})
}

func (r *runner) update(fn func(*runState)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.st)
}

// stopIfNotFailed ends the worker as Stopped unless it already reached
// Failed. It records nothing else: a graceful stop is neither a success nor a
// failure, so counters, LastSuccess and LastError keep their values.
func (r *runner) stopIfNotFailed() {
	r.update(func(st *runState) {
		if st.status != StatusFailed {
			st.status = StatusStopped
		}
	})
}

// admitRun commits "a run started" — the only writer of StatusRunning. A
// continuous restart is counted here, when the restarted run really begins.
func (r *runner) admitRun(startedAt time.Time, restart bool) {
	r.update(func(st *runState) {
		st.status = StatusRunning
		st.lastRun = startedAt
		if restart {
			st.restarts++
		}
	})
}

func (r *runner) status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st.status
}

func (r *runner) restartCount() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st.restarts
}

func (r *runner) snapshot() Info {
	r.mu.Lock()
	st := r.st
	r.mu.Unlock()
	return r.def.info(st)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (p *Pool) runContinuous(ctx context.Context, r *runner) {
	p.driveLoop(ctx, r, &continuousPolicy{p: p, r: r})
}

// runScheduled drives a scheduled worker with a single goroutine:
// sleep until the next activation, run it synchronously, recompute.
// Activations that pass while a run is still in flight are skipped (and
// logged), never queued.
func (p *Pool) runScheduled(ctx context.Context, r *runner) {
	p.driveLoop(ctx, r, &scheduledPolicy{p: p, r: r})
}

// waitNone tells driveLoop to start the next run without a timer.
const waitNone = time.Duration(-1)

// loopPolicy is the kind-specific half of the worker loop. driveLoop owns the
// shared skeleton — the timer wait, cancellation, run admission, the run
// timeout, panic-safe execution and outcome classification — and the policy
// decides when the next run happens and what a classified outcome means for
// the worker's counters and status.
type loopPolicy interface {
	// start returns the wait before the first run (waitNone for none) or
	// stop=true when the loop must not run at all.
	start(ctx context.Context) (wait time.Duration, stop bool)

	// startAttrs returns the kind-specific attributes of the "worker
	// started" line; it is called once, right after start.
	startAttrs() []any

	// beforeRun runs once the wait has elapsed. It must be side-effect-free:
	// driveLoop checks for cancellation after it and may then exit without
	// running. It returns the activation time recorded in the run context
	// and whether the run is a restart of a continuous worker.
	beforeRun() (scheduledAt time.Time, restart bool)

	// afterRun records one classified run and returns the wait before the
	// next one (waitNone for none) or stop=true.
	afterRun(ctx context.Context, res runResult) (wait time.Duration, stop bool)
}

// runResult is what driveLoop hands a policy after one run.
type runResult struct {
	outcome     runOutcome
	runID       string
	scheduledAt time.Time
	duration    time.Duration
}

// logAttrs returns the attributes every failed-run line carries: run_id
// (equal to RunID inside Run) and duration, plus timed_out, unexpected_exit
// and stack only when they apply.
func (res runResult) logAttrs() []any {
	attrs := []any{"run_id", res.runID, "duration", res.duration}
	if res.outcome.timedOut {
		attrs = append(attrs, "timed_out", true)
	}
	if res.outcome.unexpectedExit {
		attrs = append(attrs, "unexpected_exit", true)
	}
	if res.outcome.stack != nil {
		attrs = append(attrs, "stack", string(res.outcome.stack))
	}
	return attrs
}

// driveLoop runs the worker under policy until it stops. Cancellation while
// waiting, or observed at admission, preserves the last execution snapshot:
// the worker is stopped only if it has not already transitioned to Failed.
//
// Admitting a run is one step, in a fixed order: the side-effect-free
// policy.beforeRun, then the cancellation check, then a single commit of
// "a run started" (the only writer of StatusRunning), then Run. When the
// check observes cancellation, no new Run is invoked and neither LastRun nor
// Restarts changes.
func (p *Pool) driveLoop(ctx context.Context, r *runner, policy loopPolicy) {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	name, kind := r.def.name, r.def.kind()
	timeout := r.def.runTimeout

	// driveLoop owns both lifecycle lines, so every exit path — however the
	// loop ends — logs exactly one start and one stop.
	wait, stop := policy.start(ctx)
	p.logger.InfoContext(ctx, "worker started",
		append([]any{"worker", name, "kind", string(kind)}, policy.startAttrs()...)...)
	defer func() {
		status := r.status()
		reason := "shutdown"
		if status == StatusFailed {
			reason = "failed"
		}
		p.logger.InfoContext(ctx, "worker stopped",
			"worker", name, "kind", string(kind), "status", string(status), "reason", reason)
	}()

	for !stop {
		if wait != waitNone {
			timer.Reset(wait)
			select {
			case <-ctx.Done():
				r.stopIfNotFailed()
				return
			case <-timer.C:
			}
		}

		scheduledAt, restart := policy.beforeRun()
		if ctx.Err() != nil {
			r.stopIfNotFailed()
			return
		}
		startedAt := time.Now()
		r.admitRun(startedAt, restart)

		runID := newRunID()
		runCtx := enrichContext(ctx, name, scheduledAt, runID)
		cancel := context.CancelFunc(func() {})
		if timeout > 0 {
			runCtx, cancel = context.WithTimeoutCause(runCtx, timeout, ErrRunTimeout)
		}
		err := safeRun(runCtx, r.worker)
		// Read the cause before cancel, which would otherwise make every run
		// look cancelled. context.Cause keeps the first reason, so a timeout
		// that fired before shutdown stays a timeout.
		timedOut := errors.Is(context.Cause(runCtx), ErrRunTimeout)
		cancel()

		wait, stop = policy.afterRun(ctx, runResult{
			outcome: classifyRun(runInput{
				kind:     kind,
				err:      err,
				timedOut: timedOut,
				timeout:  timeout,
				poolDone: ctx.Err() != nil,
			}),
			runID:       runID,
			scheduledAt: scheduledAt,
			duration:    time.Since(startedAt),
		})
	}
}

// continuousPolicy restarts a continuous worker after every failed run —
// including an early nil return — with a capped, jittered exponential delay
// and the restart limit. WithMaxRestarts(N) allows the first run plus N
// restarts.
type continuousPolicy struct {
	p   *Pool
	r   *runner
	ran bool // a run has completed, so the next one is a restart
	// ceiling is the upper bound of the last delay window; zero starts a new
	// backoff sequence.
	ceiling time.Duration
}

func (c *continuousPolicy) start(context.Context) (time.Duration, bool) {
	return waitNone, false
}

func (c *continuousPolicy) startAttrs() []any { return nil }

func (c *continuousPolicy) beforeRun() (time.Time, bool) {
	return time.Time{}, c.ran
}

func (c *continuousPolicy) afterRun(ctx context.Context, res runResult) (time.Duration, bool) {
	r, p := c.r, c.p
	c.ran = true
	out := res.outcome
	if out.verdict != runFailed {
		// A continuous run never succeeds: the only other outcome is a
		// graceful stop.
		r.stopIfNotFailed()
		return waitNone, true
	}

	// Decide before logging: the failure line announces the next delay only
	// when a restart is planned.
	restarts := r.restartCount()
	maxRestarts := r.def.restartPolicy.maxRestarts
	exhausted := maxRestarts > 0 && restarts >= int64(maxRestarts)
	stopping := ctx.Err() != nil
	attrs := []any{
		"worker", r.def.name,
		"kind", string(KindContinuous),
		"restarts", restarts,
	}
	var delay time.Duration
	if !exhausted && !stopping {
		delay = c.nextDelay(res.duration)
		attrs = append(attrs, "next_restart_in", delay)
	}
	attrs = append(attrs, "error", out.err)
	p.logger.ErrorContext(ctx, "worker run failed", append(attrs, res.logAttrs()...)...)

	if exhausted {
		r.setOutcome(StatusFailed, out.err)
		p.logger.ErrorContext(ctx,
			"worker exceeded max restarts",
			"worker", r.def.name,
			"kind", string(KindContinuous),
			"max_restarts", maxRestarts,
		)
		return waitNone, true
	}
	if stopping {
		// A failure during shutdown is recorded, and the loop ends.
		r.setOutcome(StatusStopped, out.err)
		return waitNone, true
	}

	r.setOutcome(StatusWaiting, out.err)
	return delay, false
}

// nextDelay advances the backoff sequence after a failed run that lasted
// runDuration and returns the wait before the restart: uniform in
// [max(base, ceiling/2), ceiling], where the ceiling starts at base and
// doubles up to the cap. A run that lasted at least the cap starts a new
// sequence, so its restart waits base again.
func (c *continuousPolicy) nextDelay(runDuration time.Duration) time.Duration {
	base := c.r.def.restartPolicy.restartDelay
	maxDelay := c.r.def.restartPolicy.maxRestartDelay
	if runDuration >= maxDelay {
		c.ceiling = 0
	}
	c.ceiling = nextCeiling(c.ceiling, base, maxDelay)
	return c.p.jitter(max(base, c.ceiling/2), c.ceiling)
}

// nextCeiling returns the upper bound of the next delay window: base for the
// first failure of a sequence (prev == 0), then twice the previous ceiling,
// saturating at maxDelay. It never overflows: doubling happens only while the
// result stays within maxDelay. base <= maxDelay holds by construction.
func nextCeiling(prev, base, maxDelay time.Duration) time.Duration {
	switch {
	case prev == 0:
		return base
	case prev > maxDelay/2:
		return maxDelay
	default:
		return prev * 2
	}
}

// uniformJitter returns a duration drawn uniformly from [lo, hi]; lo when the
// window is empty. lo is positive, so hi-lo+1 cannot overflow.
func uniformJitter(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(rand.Int64N(int64(hi-lo)+1))
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
	synthetic           bool // the pending run is the startup run (WithStartImmediately)
}

func (s *scheduledPolicy) start(ctx context.Context) (time.Duration, bool) {
	if s.r.def.startImmediately {
		s.synthetic = true
		return waitNone, false
	}
	s.anchor = time.Now()
	return s.scheduleNext(ctx)
}

func (s *scheduledPolicy) startAttrs() []any {
	attrs := []any{"schedule", s.r.def.scheduleExpr()}
	if s.synthetic {
		return append(attrs, "start_immediately", true)
	}
	if !s.next.IsZero() {
		attrs = append(attrs, "next_run", s.next)
	}
	return attrs
}

func (s *scheduledPolicy) beforeRun() (time.Time, bool) {
	if s.synthetic {
		// Synthetic startup run: ScheduledAt is the zero time.
		return time.Time{}, false
	}
	return s.next, false
}

func (s *scheduledPolicy) afterRun(ctx context.Context, res runResult) (time.Duration, bool) {
	if s.finishRun(ctx, res) {
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
func (s *scheduledPolicy) finishRun(ctx context.Context, res runResult) (stop bool) {
	r, p := s.r, s.p
	out := res.outcome
	switch out.verdict {
	case runStopped:
		r.stopIfNotFailed()
		return true
	case runSucceeded:
		s.consecutiveFailures = 0
		r.recordSuccess(time.Now())
		p.logger.DebugContext(ctx,
			"scheduled worker run completed",
			"worker", r.def.name,
			"run_id", res.runID,
			"scheduled_at", res.scheduledAt,
			"duration", res.duration,
		)
		return false
	}

	s.consecutiveFailures++
	p.logger.ErrorContext(ctx,
		"scheduled worker run failed",
		append([]any{
			"worker", r.def.name,
			"scheduled_at", res.scheduledAt,
			"consecutive_failures", s.consecutiveFailures,
			"error", out.err,
		}, res.logAttrs()...)...,
	)

	if max := r.def.failurePolicy.maxConsecutiveFailures; max > 0 && s.consecutiveFailures >= int64(max) {
		r.setFailureOutcome(StatusFailed, s.consecutiveFailures, out.err)
		p.logger.ErrorContext(ctx,
			"worker exceeded max consecutive failures",
			"worker", r.def.name,
			"max_consecutive_failures", max,
		)
		return true
	}
	if ctx.Err() != nil {
		// A failure during shutdown is recorded, and the loop ends.
		r.setFailureOutcome(StatusStopped, s.consecutiveFailures, out.err)
		return true
	}

	r.setFailureOutcome(StatusWaiting, s.consecutiveFailures, out.err)
	return false
}

// scheduleNext computes the next activation from the anchor, skipping (and
// logging) activations the previous run outlasted, and returns the wait until
// it. A schedule with no future activation marks the worker Failed and stops.
func (s *scheduledPolicy) scheduleNext(ctx context.Context) (time.Duration, bool) {
	r, p := s.r, s.p
	next := r.def.schedule.Next(s.anchor)
	var skipped int
	var firstSkipped, lastSkipped time.Time
	for !next.IsZero() && !next.After(time.Now()) {
		// The previous run outlasted this activation — skip it.
		if skipped == 0 {
			firstSkipped = next
		}
		lastSkipped = next
		skipped++
		next = r.def.schedule.Next(next)
	}
	if skipped > 0 {
		// One line per resumption, not one per activation: a one-second
		// schedule behind a ten-minute run would otherwise log 600 lines.
		p.logger.WarnContext(ctx,
			"worker ticks skipped",
			"worker", r.def.name,
			"skipped", skipped,
			"first_scheduled_at", firstSkipped,
			"last_scheduled_at", lastSkipped,
		)
	}
	if next.IsZero() {
		r.update(func(st *runState) {
			st.lastError = "schedule has no future activation"
			st.status = StatusFailed
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

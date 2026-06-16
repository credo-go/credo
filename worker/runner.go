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
	restarts := int64(0)
	for {
		if ctx.Err() != nil {
			r.setOutcome(StatusStopped, nil)
			return
		}

		r.startRun(time.Now())

		runCtx := enrichContext(ctx, r.def.name, int(restarts)+1, time.Time{}, newRunID())
		err := safeRun(runCtx, r.def.worker)

		if err == nil {
			r.setOutcome(StatusStopped, nil)
			return
		}
		if isGracefulStop(err, ctx) {
			r.setOutcome(StatusStopped, nil)
			p.logger.InfoContext(ctx, "worker stopped", "worker", r.def.name, "kind", r.def.Kind())
			return
		}

		restarts++
		p.logger.ErrorContext(ctx,
			"worker run failed",
			"worker", r.def.name,
			"kind", r.def.Kind(),
			"restart", restarts,
			"error", err,
		)

		if max := r.def.restartPolicy.maxRestarts; max > 0 && restarts >= int64(max) {
			r.setAttemptOutcome(StatusFailed, restarts, err)
			p.logger.ErrorContext(ctx,
				"worker exceeded max restarts",
				"worker", r.def.name,
				"kind", r.def.Kind(),
				"max_restarts", max,
			)
			return
		}

		r.setAttemptOutcome(StatusWaiting, restarts, err)
		restartTimer := time.NewTimer(r.def.restartPolicy.restartDelay)
		select {
		case <-ctx.Done():
			restartTimer.Stop()
			r.stopIfNotFailed(false)
			return
		case <-restartTimer.C:
		}
	}
}

// runScheduled drives a scheduled worker with a single goroutine:
// sleep until the next activation, run it synchronously, recompute.
// Activations that pass while a run is still in flight are skipped (and
// logged), never queued — the same skip-if-still-running semantics the
// earlier two-goroutine model provided via a non-blocking tick handoff.
func (p *Pool) runScheduled(ctx context.Context, r *runner) {
	consecutiveFailures := int64(0)

	// runOnce executes one activation synchronously and reports whether the
	// scheduling loop should stop (graceful stop or permanent failure).
	runOnce := func(intendedTime time.Time) (stop bool) {
		r.startRun(time.Now())

		runCtx := enrichContext(ctx, r.def.name, 1, intendedTime, newRunID())
		err := safeRun(runCtx, r.def.worker)

		if isGracefulStop(err, ctx) {
			r.setOutcome(StatusStopped, nil)
			p.logger.InfoContext(ctx,
				"worker stopped during scheduled run",
				"worker", r.def.name,
			)
			return true
		}

		if err == nil {
			consecutiveFailures = 0
			status := StatusWaiting
			if ctx.Err() != nil {
				status = StatusStopped
			}
			r.setAttemptOutcome(status, 0, nil)
			return false
		}

		consecutiveFailures++
		p.logger.ErrorContext(ctx,
			"scheduled worker run failed",
			"worker", r.def.name,
			"scheduled_at", intendedTime,
			"consecutive_failures", consecutiveFailures,
			"error", err,
		)

		if max := r.def.failurePolicy.maxConsecutiveFailures; max > 0 && consecutiveFailures >= int64(max) {
			r.setAttemptOutcome(StatusFailed, consecutiveFailures, err)
			p.logger.ErrorContext(ctx,
				"worker exceeded max consecutive failures",
				"worker", r.def.name,
				"max_consecutive_failures", max,
			)
			return true
		}

		if ctx.Err() != nil {
			r.setAttemptOutcome(StatusStopped, consecutiveFailures, err)
			return true
		}

		r.setAttemptOutcome(StatusWaiting, consecutiveFailures, err)
		return false
	}

	if r.def.startImmediately {
		if ctx.Err() != nil {
			r.stopIfNotFailed(false)
			return
		}
		// Synthetic startup run: ScheduledAt is the zero time.
		if runOnce(time.Time{}) {
			return
		}
	}

	// anchor is the last intended activation. Computing the next activation
	// from it (instead of from time.Now()) keeps @every periods anchored:
	// a long run delays the next fire but does not shift the grid.
	anchor := time.Now()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		next := r.def.schedule.Next(anchor)
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
			return
		}

		timer.Reset(time.Until(next))
		select {
		case <-ctx.Done():
			// Preserve the last execution snapshot on shutdown; only stop the
			// worker if it has not already transitioned to Failed.
			r.stopIfNotFailed(false)
			return
		case <-timer.C:
		}

		if runOnce(next) {
			return
		}
		anchor = next
	}
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

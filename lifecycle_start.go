package credo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/credo-go/credo/internal/di"
	internalhealth "github.com/credo-go/credo/internal/health"
	internalobserve "github.com/credo-go/credo/internal/observe"
)

// lifecycleStackSize bounds the stack captured when a Start or hook panics.
const lifecycleStackSize = 8192

// errShutdownWhileStarting is the cause of a start phase that Shutdown
// interrupted.
var errShutdownWhileStarting = errors.New("credo: the App was shut down while starting")

// startOutcome is how a start walk ended.
type startOutcome uint8

const (
	startOK startOutcome = iota
	// startFailed: a Start, start hook or constructor failed.
	startFailed
	// startInterrupted: a shutdown was requested during the start phase.
	startInterrupted
)

// startRun is one start phase. A shutdown requested while it runs —
// Shutdown, the context of the entry point — interrupts it: the running
// Start's context is cancelled, nothing further starts, and the rollback runs
// under the drain context the interrupter supplied.
type startRun struct {
	// ctx is the parent of every Start and start-hook context; an interrupt
	// cancels it.
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	interrupted bool
	finished    bool
	// drainCtx bounds the rollback of an interrupted start; drainCancel
	// releases it, when the interrupter derived it.
	drainCtx    context.Context
	drainCancel context.CancelFunc
	// cause is why the start was interrupted: the entry point's context
	// error, or errShutdownWhileStarting.
	cause error

	// done is closed once the start phase has stored its final state; err is
	// then the rollback's lifecycle error, for a Shutdown waiting on it.
	done chan struct{}
	err  error

	// Written by the walk's goroutine only.
	report   lifecycleReport
	excluded []*di.Unit
	stuck    []*di.Unit
}

func newStartRun() *startRun {
	ctx, cancel := context.WithCancel(context.Background())
	return &startRun{ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// interrupt requests a shutdown of the start phase, with drainCtx bounding
// the rollback. It reports false when the start phase has already ended or
// was already interrupted; drainCancel is then the caller's to call.
func (r *startRun) interrupt(drainCtx context.Context, drainCancel context.CancelFunc, cause error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || r.interrupted {
		return false
	}
	r.interrupted = true
	r.drainCtx, r.drainCancel, r.cause = drainCtx, drainCancel, cause
	r.cancel()
	return true
}

func (r *startRun) isInterrupted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interrupted
}

// rollbackContext returns the drain context of an interrupted start.
func (r *startRun) rollbackContext() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drainCtx
}

// enter marks the start phase as succeeded unless it was interrupted first.
func (r *startRun) enter() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.interrupted {
		return false
	}
	r.finished = true
	return true
}

// finish ends the start phase and wakes whoever waits on it. Once only.
func (r *startRun) finish(err error) {
	r.mu.Lock()
	r.finished = true
	r.err = err
	drainCancel := r.drainCancel
	r.mu.Unlock()
	r.cancel()
	if drainCancel != nil {
		drainCancel()
	}
	close(r.done)
}

// watch interrupts the start phase when ctx ends before it does. The
// rollback then runs under ctx's values with the shutdown timeout, counted
// from the request.
func (r *startRun) watch(ctx context.Context, timeout time.Duration) {
	if ctx.Done() == nil {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			if !r.interrupt(drainCtx, cancel, ctx.Err()) {
				cancel()
			}
		case <-r.done:
		}
	}()
}

// call runs fn with a context that ends when fn returns or the start is
// interrupted. Once interrupted it waits for fn until the rollback's
// deadline; returned is false when fn had not returned by then, and its
// later return is logged.
func (lm *lifecycleManager) call(
	r *startRun, name string, fn func(ctx context.Context) error,
) (elapsed time.Duration, returned bool, err error) {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	results := make(chan error, 1)
	began := time.Now()
	go func() { results <- fn(ctx) }()
	select {
	case err := <-results:
		return time.Since(began), true, err
	case <-r.ctx.Done():
	}
	select {
	case err := <-results:
		return time.Since(began), true, err
	case <-r.rollbackContext().Done():
	}
	go lm.logLateReturn(name, PhaseStart, began, results)
	return time.Since(began), false, nil
}

// logLateReturn logs the result of a call abandoned at the deadline when it
// eventually returns; it is never written back into a report.
func (lm *lifecycleManager) logLateReturn(name string, phase LifecyclePhase, began time.Time, results <-chan error) {
	err := <-results
	attrs := []slog.Attr{
		slog.String("component", name),
		slog.String("phase", string(phase)),
		slog.Duration("duration", time.Since(began)),
	}
	level := slog.LevelWarn
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
		level = slog.LevelError
	}
	lm.app.logger.LogAttrs(context.Background(), level,
		"credo: call returned after the deadline; result not reported", attrs...)
}

// build returns a planned unit's value, building it on a helper goroutine.
// ok is false when the start was interrupted before the build completed; the
// teardown then waits for the pending construction.
func (lm *lifecycleManager) build(r *startRun, u *di.Unit) (value any, ok bool, err error) {
	c := lm.app.container
	if v, built := c.UnitValue(u); built {
		return v, true, nil
	}
	type result struct {
		v   any
		err error
	}
	results := make(chan result, 1)
	go func() {
		v, err := c.BuildUnit(u)
		results <- result{v, err}
	}()
	select {
	case res := <-results:
		return res.v, true, res.err
	case <-r.ctx.Done():
		return nil, false, nil
	}
}

// startWalk runs the start walk: per tier, internal then ingress, it builds
// each planned component in dependency order and calls its Start, then runs
// the tier's start hooks FIFO. It stops at the first failure or at an
// interrupt; on success it stores running, unless an interrupt came first.
func (lm *lifecycleManager) startWalk(r *startRun) startOutcome {
	c := lm.app.container
	for _, s := range lm.frameworkSteps {
		if r.isInterrupted() {
			return startInterrupted
		}
		elapsed, returned, err := lm.call(r, s.name, s.fn)
		if outcome := lm.recordCall(r, s.name, TierInternal, elapsed, returned, err); outcome != startOK {
			return outcome
		}
	}
	for _, tier := range []Tier{TierInternal, TierIngress} {
		for _, u := range c.StartPlan(di.Tier(tier)) {
			if r.isInterrupted() {
				return startInterrupted
			}
			v, ok, err := lm.build(r, u)
			switch {
			case !ok:
				return startInterrupted
			case err != nil && r.isInterrupted():
				lm.logInterruptedStart(u.Name(), err)
				return startInterrupted
			case err != nil:
				r.report.add(LifecycleEntry{Name: u.Name(), Tier: tier, Phase: PhaseStart,
					Outcome: outcomeOf(err), Err: err})
				return startFailed
			}
			for _, step := range u.Steps() {
				run := func(ctx context.Context) error { return step.Run(ctx, v) }
				if outcome := lm.startUnit(r, u, tier, run, false); outcome != startOK {
					return outcome
				}
			}
			starter, ok := v.(Starter)
			if !ok || !c.ClaimStart(u) {
				continue
			}
			if outcome := lm.startUnit(r, u, tier, starter.Start, true); outcome != startOK {
				return outcome
			}
		}
		for _, h := range lm.onStart {
			if h.tier != tier {
				continue
			}
			if r.isInterrupted() {
				return startInterrupted
			}
			name := fmt.Sprintf("OnStart[%d]", h.index)
			elapsed, returned, err := lm.call(r, name, func(ctx context.Context) error {
				return lm.runLifecycleHook(ctx, "OnStart", h.index, h.fn)
			})
			if outcome := lm.recordCall(r, name, tier, elapsed, returned, err); outcome != startOK {
				return outcome
			}
		}
	}
	return lm.enterRunning(r)
}

// recordCall reports the result of a start call that belongs to no
// component — a start hook or a framework step.
func (lm *lifecycleManager) recordCall(
	r *startRun, name string, tier Tier, elapsed time.Duration, returned bool, err error,
) startOutcome {
	switch {
	case !returned:
		r.report.add(LifecycleEntry{Name: name, Tier: tier, Phase: PhaseStart, Outcome: OutcomeAbandoned,
			Duration: elapsed})
		r.report.setCause(r.rollbackContext().Err())
		return startInterrupted
	case err != nil && r.isInterrupted():
		lm.logInterruptedStart(name, err)
		return startInterrupted
	case err != nil:
		r.report.add(LifecycleEntry{Name: name, Tier: tier, Phase: PhaseStart, Outcome: outcomeOf(err), Err: err})
		return startFailed
	}
	return startOK
}

// startUnit runs one start call of a component — a start step, or its
// Start. A Start that fails has released what it opened, so the rollback
// skips it (releases); a failed start step leaves the value to the rollback.
func (lm *lifecycleManager) startUnit(
	r *startRun, u *di.Unit, tier Tier, fn func(ctx context.Context) error, releases bool,
) startOutcome {
	elapsed, returned, err := lm.call(r, u.Name(), func(ctx context.Context) error {
		return lm.invokeStart(ctx, u, fn)
	})
	switch {
	case !returned:
		// Still running: it keeps its dependencies open.
		r.stuck = append(r.stuck, u)
		r.report.add(LifecycleEntry{Name: u.Name(), Tier: tier, Phase: PhaseStart, Outcome: OutcomeAbandoned,
			Duration: elapsed})
		r.report.setCause(r.rollbackContext().Err())
		return startInterrupted
	case err != nil:
		if releases {
			// It released what it opened and is not shut down.
			r.excluded = append(r.excluded, u)
		}
		if r.isInterrupted() {
			lm.logInterruptedStart(u.Name(), err)
			return startInterrupted
		}
		r.report.add(LifecycleEntry{Name: u.Name(), Tier: tier, Phase: PhaseStart, Outcome: outcomeOf(err),
			Err: err})
		return startFailed
	}
	return startOK
}

// invokeStart runs a start call, recovering a panic as a *DIPanicError.
func (lm *lifecycleManager) invokeStart(ctx context.Context, u *di.Unit, fn func(context.Context) error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicErr := &di.PanicError{
				Type:  u.Type(),
				Phase: di.PhaseStart,
				Value: recovered,
				Stack: internalobserve.StackTrace(lifecycleStackSize),
			}
			lm.app.logger.LogAttrs(context.Background(), slog.LevelError, "credo: component Start panic",
				slog.String("component", u.Name()),
				slog.Any("panic", recovered),
				slog.String("stack", panicErr.Stack))
			err = panicErr
		}
	}()
	return fn(ctx)
}

// enterRunning stores running once the walk has succeeded, unless an
// interrupt came first, and opens the ServeHTTP gate.
func (lm *lifecycleManager) enterRunning(r *startRun) startOutcome {
	lm.readiness = lm.componentReadiness()
	if !r.enter() {
		return startInterrupted
	}
	lm.started.Store(true)
	lm.state.Store(uint32(stateRunning))
	lm.finishRun(r, nil)
	return startOK
}

// endStart rolls back a failed or interrupted start and returns what the
// entry point reports: the lifecycle error of a failed start; for an
// interrupted one, nil after a clean rollback under the serving entry
// points, and the interruption's cause (joined with the lifecycle error)
// under App.Start, whose caller must not mistake it for a started App.
func (lm *lifecycleManager) endStart(r *startRun, outcome startOutcome, appStart bool) error {
	ctx := r.rollbackContext()
	if outcome == startFailed {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), lm.shutdownTimeout())
		defer cancel()
	} else {
		lm.app.logger.LogAttrs(context.Background(), slog.LevelInfo,
			"credo: start interrupted; rolling back what was built")
	}
	lm.state.Store(uint32(stateStopping))
	lm.draining.Store(true)
	lm.cancelSession()
	lm.serverMu.Lock()
	redirectSrv := lm.redirectServer
	lm.serverMu.Unlock()
	if redirectSrv != nil {
		_ = redirectSrv.Close()
	}

	_, reloadErr := lm.stopTiers(ctx, r.excluded, r.stuck, &r.report, nil)
	lifecycleErr := r.report.err()

	lm.clearSession()
	lm.state.Store(uint32(stateStopped))
	lm.finishRun(r, lifecycleErr)

	switch {
	case outcome == startFailed:
		return errors.Join(lifecycleErr, reloadErr)
	case appStart:
		r.mu.Lock()
		cause := r.cause
		r.mu.Unlock()
		return errors.Join(fmt.Errorf("credo: Start: %w", cause), lifecycleErr)
	default:
		return lifecycleErr
	}
}

// logInterruptedStart logs the error of a Start or start hook that returned
// after an interrupt cancelled its context. A requested shutdown is not a
// start failure, so the error is not reported.
func (lm *lifecycleManager) logInterruptedStart(name string, err error) {
	lm.app.logger.LogAttrs(context.Background(), slog.LevelInfo,
		"credo: start interrupted by a shutdown request",
		slog.String("component", name), slog.Any("error", err))
}

// outcomeOf classifies a call's error.
func outcomeOf(err error) LifecycleOutcome {
	if _, ok := errors.AsType[*di.PanicError](err); ok {
		return OutcomePanicked
	}
	if _, ok := errors.AsType[*lifecycleHookPanicError](err); ok {
		return OutcomePanicked
	}
	return OutcomeFailed
}

// runStopHooks runs the stop hooks of tier LIFO, each bounded by ctx. Once
// ctx has ended no further hook starts; each one left is reported abandoned.
func (lm *lifecycleManager) runStopHooks(ctx context.Context, tier Tier, report *lifecycleReport) {
	for i := len(lm.onStop) - 1; i >= 0; i-- {
		h := lm.onStop[i]
		if h.tier != tier {
			continue
		}
		name := fmt.Sprintf("OnStop[%d]", h.index)
		if ctx.Err() != nil {
			report.add(LifecycleEntry{Name: name, Tier: tier, Phase: PhaseShutdown, Outcome: OutcomeAbandoned})
			report.setCause(ctx.Err())
			continue
		}
		results := make(chan error, 1)
		began := time.Now()
		go func() { results <- lm.runLifecycleHook(ctx, "OnStop", h.index, h.fn) }()
		select {
		case err := <-results:
			if err != nil {
				report.add(LifecycleEntry{Name: name, Tier: tier, Phase: PhaseShutdown, Outcome: outcomeOf(err),
					Err: err, Duration: time.Since(began)})
			}
		case <-ctx.Done():
			select {
			case err := <-results:
				if err != nil {
					report.add(LifecycleEntry{Name: name, Tier: tier, Phase: PhaseShutdown,
						Outcome: outcomeOf(err), Err: err, Duration: time.Since(began)})
				}
				continue
			default:
			}
			report.add(LifecycleEntry{Name: name, Tier: tier, Phase: PhaseShutdown, Outcome: OutcomeAbandoned,
				Duration: time.Since(began)})
			report.setCause(ctx.Err())
			go lm.logLateReturn(name, PhaseShutdown, began, results)
		}
	}
}

// lifecycleHookPanicError reports a start or stop hook that panicked. It
// reads only "panic: …"; the record written when the panic was recovered
// names the hook and carries the stack.
type lifecycleHookPanicError struct {
	kind  string // "OnStart" or "OnStop"
	index int    // registration index of the hook
	value any    // the recovered value
	cause error
	stack string
}

func (e *lifecycleHookPanicError) Error() string {
	return fmt.Sprintf("panic: %v", e.value)
}

func (e *lifecycleHookPanicError) Unwrap() error {
	return e.cause
}

// runLifecycleHook calls a start or stop hook and turns a panic into that
// hook's error. The recovered panic is logged once with the hook index and
// the stack, because the returned error carries neither.
func (lm *lifecycleManager) runLifecycleHook(
	ctx context.Context,
	kind string,
	index int,
	fn func(ctx context.Context) error,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicErr := &lifecycleHookPanicError{
				kind:  kind,
				index: index,
				value: recovered,
				cause: internalobserve.PanicError(recovered),
				stack: internalobserve.StackTrace(lifecycleStackSize),
			}
			lm.app.logger.LogAttrs(
				context.WithoutCancel(ctx),
				slog.LevelError,
				"credo: "+kind+" hook panic",
				slog.Int("hook_index", index),
				slog.Any("panic", recovered),
				slog.String("stack", panicErr.stack),
			)
			err = panicErr
		}
	}()
	return fn(ctx)
}

// componentReadiness builds the readiness checks of the components that
// answer Ready, once, from the values the start walk built: no DI resolution
// happens per probe.
func (lm *lifecycleManager) componentReadiness() []internalhealth.ReadinessCheck {
	c := lm.app.container
	var checks []internalhealth.ReadinessCheck
	for _, u := range c.Units() {
		if !u.Readies() {
			continue
		}
		v, built := c.UnitValue(u)
		readier, ok := v.(Readier)
		if !built || !ok {
			continue
		}
		checks = append(checks, internalhealth.ReadinessCheck{
			Name: u.Name(),
			Probe: internalhealth.NewProbe(func(ctx context.Context) internalhealth.Result {
				if err := readier.Ready(ctx); err != nil {
					return internalhealth.FailureResult(err)
				}
				return internalhealth.SuccessResult()
			}),
		})
	}
	return checks
}

// Start runs the start phase without a listener; Run, RunContext and
// ServeContext serve. It claims the App, prepares it (Finalize and compile),
// runs the start walk — each component's Start in dependency order, then the
// start hooks, internal tier first — and enters running, for an App served
// through ServeHTTP by an external http.Server or in a test. Shutdown stops
// what it started.
//
// Whoever serves the App through ServeHTTP owns that server's admission and
// drain and completes them before Shutdown: the internal tier stops after
// the HTTP drain only if that drain has happened.
//
// Start is accepted only once, in building; a second call, or a serve entry
// point after it, returns an error. A failed start rolls back what was built,
// leaves the App stopped, and returns a *LifecycleError. ctx interrupts the
// start phase: nothing further starts, what was built is rolled back under
// the shutdown timeout, and Start returns ctx's error — or, when Shutdown
// interrupted it, an error saying so — joined with the lifecycle error when
// the rollback was not clean. In tests, testutil.Start starts an App and
// shuts it down when the test ends.
func (app *App) Start(ctx context.Context) error {
	lm := app.lifecycle
	run, err := lm.claimStartSlot("Start")
	if err != nil {
		return err
	}
	run.watch(ctx, lm.shutdownTimeout())

	p := app.prepare()
	if p == nil {
		lm.abortClaim(run)
		return errors.New("credo: Start: preparation rejected: app is shutting down")
	}
	if p.err != nil {
		lm.abortClaim(run)
		return fmt.Errorf("credo: Start: %w", p.err)
	}
	lm.openSession(nil)
	if outcome := lm.startWalk(run); outcome != startOK {
		return lm.endStart(run, outcome, true)
	}
	return nil
}

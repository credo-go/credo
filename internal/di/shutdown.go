package di

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/credo-go/credo/internal/observe"
)

// lateCleanupBudget is the fixed bound for the single best-effort cleanup of
// an instance constructed after the shutdown context ended. It is deliberately
// not configurable.
const lateCleanupBudget = 5 * time.Second

// Shutdown tears down the container's instances consumers-before-dependencies
// and reports what it could not complete.
//
// Entering Shutdown atomically closes the registration window and the
// resolution phase: every later Resolve returns an error wrapping [ErrClosed],
// no new construction is admitted, and builds already running are tracked as
// pending vertices whose results are withheld from callers. The teardown graph
// is derived from the frozen registrations (constructor parameters, aliases and
// BindMany collections) and the current instance states, so it does not depend
// on a successful Seal.
//
// The pass is a Kahn traversal over the live graph: an instance is eligible
// once every live dependent has retired, ties go to the most recently
// registered type, and a pending build blocks its dependencies until it
// completes. Each eligible Shutdowner is invoked sequentially on a helper
// goroutine with ctx and waited for until it returns or ctx ends; a call that
// outlives ctx is reported as running and keeps its dependencies blocked.
// Panics inside Shutdown are recovered as *PanicError and count as a completed
// failed attempt. Once ctx ends no further ordinary attempts are made and no
// instance is retried.
//
// An instance whose construction completes after ctx ended is not part of the
// pass: it receives one separate, fixed five-second best-effort cleanup whose
// outcome is logged, never written back into the returned report.
//
// Shutdown returns nil when every instance retired cleanly, otherwise a
// *ShutdownError snapshot. A second call returns an error wrapping ErrClosed.
func (c *Container) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return fmt.Errorf("di: Shutdown: %w", ErrClosed)
	}
	c.closing = true
	c.frozen = true
	c.shutdownCtx = ctx
	g := c.teardownGraphLocked()
	logger := c.logger
	c.mu.Unlock()

	g.run(ctx, logger)

	c.mu.Lock()
	c.teardownDone = true
	report := g.reportLocked(ctx, time.Now())
	c.mu.Unlock()

	if report == nil {
		logger.LogAttrs(context.Background(), slog.LevelDebug, "di: shutdown complete",
			slog.Int("registrations", len(g.vertices)))
		return nil
	}
	return report
}

// vertex is one registration in the teardown graph.
type vertex struct {
	t     reflect.Type
	index int
	entry *singletonEntry
	deps  []*vertex // registered dependencies, deduplicated

	// active: built or building at the closing snapshot and not yet retired.
	active bool
	// liveDependents counts active vertices that depend on this one.
	liveDependents int
	// attempted: the pass produced a terminal decision (state/err/duration).
	attempted bool

	state    ShutdownState
	err      error
	started  time.Time
	duration time.Duration
}

// teardownGraph is the pass's private working state. The pass goroutine owns
// every vertex field; only entry states are shared with builders and are read
// under Container.mu.
type teardownGraph struct {
	c        *Container
	vertices []*vertex // registration order
	byType   map[reflect.Type]*vertex
}

// teardownGraphLocked snapshots the registrations and instance states. Edges
// come from constructor parameters, resolved the way construction resolves
// them: a direct registration, an alias, or the BindMany collection of an
// interface slice. Prebuilt values have no visible dependencies.
func (c *Container) teardownGraphLocked() *teardownGraph {
	g := &teardownGraph{c: c, byType: make(map[reflect.Type]*vertex, len(c.order))}
	for i, t := range c.order {
		entry := c.singletons[t]
		if entry == nil {
			entry = &singletonEntry{}
			c.singletons[t] = entry
		}
		v := &vertex{t: t, index: i, entry: entry}
		v.active = entry.state == entryBuilt || entry.state == entryBuilding
		g.vertices = append(g.vertices, v)
		g.byType[t] = v
	}
	for _, v := range g.vertices {
		reg, ok := c.registrations[v.t]
		if !ok {
			continue
		}
		seen := make(map[reflect.Type]struct{})
		for _, pt := range reg.deps() {
			if pt == contextType || c.isFrameworkType(pt) {
				continue
			}
			for _, dt := range c.cycleDependenciesForParam(pt) {
				if dt == v.t {
					continue
				}
				if _, dup := seen[dt]; dup {
					continue
				}
				seen[dt] = struct{}{}
				if d, ok := g.byType[dt]; ok {
					v.deps = append(v.deps, d)
				}
			}
		}
	}
	for _, v := range g.vertices {
		if !v.active {
			continue
		}
		for _, d := range v.deps {
			d.liveDependents++
		}
	}
	return g
}

// run is the ready-queue traversal. It returns when the context ends, when
// every active vertex has retired, or when nothing is ready and nothing is
// pending (a graph inconsistency the report describes as blocked).
func (g *teardownGraph) run(ctx context.Context, logger *slog.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}
		if v := g.pickReady(); v != nil {
			g.attempt(ctx, v, logger)
			continue
		}
		if !g.hasPending() {
			return
		}
		select {
		case <-g.c.buildDone:
		case <-ctx.Done():
			return
		}
		g.absorbCompletions()
	}
}

// pickReady returns the most recently registered active vertex that is built,
// unattempted, not claimed by late cleanup, and has no live dependents.
func (g *teardownGraph) pickReady() *vertex {
	g.c.mu.RLock()
	defer g.c.mu.RUnlock()
	for i := len(g.vertices) - 1; i >= 0; i-- {
		v := g.vertices[i]
		if !v.active || v.attempted || v.liveDependents != 0 {
			continue
		}
		if v.entry.state != entryBuilt || v.entry.late {
			continue
		}
		return v
	}
	return nil
}

// hasPending reports whether an active vertex is still constructing.
func (g *teardownGraph) hasPending() bool {
	g.c.mu.RLock()
	defer g.c.mu.RUnlock()
	for _, v := range g.vertices {
		if v.active && !v.attempted && v.entry.state == entryBuilding {
			return true
		}
	}
	return false
}

// absorbCompletions retires pending builds that failed; builds that succeeded
// simply become eligible for pickReady.
func (g *teardownGraph) absorbCompletions() {
	g.c.mu.RLock()
	var failed []*vertex
	for _, v := range g.vertices {
		if v.active && !v.attempted && v.entry.state == entryFailed {
			failed = append(failed, v)
		}
	}
	g.c.mu.RUnlock()
	for _, v := range failed {
		v.attempted = true
		v.state = ShutdownConstructionFailed
		v.err = v.entry.err
		g.retire(v)
	}
}

// retire releases a vertex's dependencies.
func (g *teardownGraph) retire(v *vertex) {
	v.active = false
	for _, d := range v.deps {
		d.liveDependents--
	}
}

// attempt shuts down one eligible vertex. A non-Shutdowner retires without
// user code; a Shutdowner is invoked with the shared context and bounded by
// it. A completed attempt (success, error or panic) retires the vertex; a
// timed-out one stays running and keeps its dependencies blocked.
func (g *teardownGraph) attempt(ctx context.Context, v *vertex, logger *slog.Logger) {
	v.attempted = true
	s, ok := v.entry.value.(shutdowner)
	if !ok {
		v.state = ShutdownRetired
		g.retire(v)
		return
	}
	v.started = time.Now()
	outcome, completed := g.c.boundedShutdown(ctx, v.t, s, PhaseShutdown, logger)
	if !completed {
		v.state = ShutdownRunning
		return
	}
	v.duration = outcome.duration
	switch {
	case outcome.err == nil:
		v.state = ShutdownSucceeded
		logger.LogAttrs(context.Background(), slog.LevelDebug, "di: shutdown succeeded",
			slog.String("type", v.t.String()), slog.Duration("duration", v.duration))
	case isPanicError(outcome.err):
		v.state = ShutdownPanicked
		v.err = outcome.err
	default:
		v.state = ShutdownFailed
		v.err = outcome.err
	}
	g.retire(v)
}

// reportLocked classifies every vertex at the boundary and returns nil when
// nothing failed or remained incomplete.
func (g *teardownGraph) reportLocked(ctx context.Context, now time.Time) *ShutdownError {
	entries := make([]ShutdownEntry, 0, len(g.vertices))
	var errs []error
	incomplete := false
	for _, v := range g.vertices {
		e := ShutdownEntry{Type: v.t}
		switch {
		case v.attempted:
			e.State, e.Err, e.Duration = v.state, v.err, v.duration
			if v.state == ShutdownRunning {
				e.Duration = now.Sub(v.started)
			}
		case v.entry.state == entryUnbuilt:
			e.State = ShutdownNeverConstructed
		case v.entry.state == entryFailed:
			e.State = ShutdownConstructionFailed
			e.Err = v.entry.err
		case v.entry.state == entryBuilding:
			e.State = ShutdownConstructing
			e.Duration = now.Sub(v.entry.buildStart)
		case v.entry.late:
			e.State = ShutdownLateCleanup
		case v.liveDependents > 0:
			e.State = ShutdownBlocked
			e.Blockers = g.blockersOf(v)
		default:
			if _, ok := v.entry.value.(shutdowner); ok {
				e.State = ShutdownUnattempted
			} else {
				e.State = ShutdownRetired
			}
		}
		switch e.State {
		case ShutdownFailed, ShutdownPanicked:
			incomplete = true
			errs = append(errs, fmt.Errorf("di: shutting down %s: %w", v.t, e.Err))
		case ShutdownRunning, ShutdownConstructing, ShutdownBlocked, ShutdownUnattempted, ShutdownLateCleanup:
			incomplete = true
		}
		entries = append(entries, e)
	}
	if !incomplete {
		return nil
	}
	cause := ctx.Err()
	if cause != nil {
		errs = append(errs, cause)
	}
	return &ShutdownError{Entries: entries, Cause: cause, errs: errs}
}

// blockersOf lists the active vertices that still depend on v.
func (g *teardownGraph) blockersOf(v *vertex) []reflect.Type {
	var blockers []reflect.Type
	for _, u := range g.vertices {
		if !u.active {
			continue
		}
		for _, d := range u.deps {
			if d == v {
				blockers = append(blockers, u.t)
				break
			}
		}
	}
	return blockers
}

// shutdownOutcome is one Shutdown invocation's result.
type shutdownOutcome struct {
	err      error
	duration time.Duration
}

// attemptHandoff decides, exactly once, whether a helper's result is delivered
// to the waiting owner or logged as a late completion after the owner gave up.
type attemptHandoff struct {
	mu        sync.Mutex
	abandoned bool
	delivered bool
}

// boundedShutdown invokes s.Shutdown(ctx) on a helper goroutine and waits for
// completion or the end of ctx, preferring an already-completed result at the
// boundary. completed is false when the helper was still running: it may
// finish later, in which case its result is logged and otherwise discarded.
func (c *Container) boundedShutdown(
	ctx context.Context,
	t reflect.Type,
	s shutdowner,
	phase PanicPhase,
	logger *slog.Logger,
) (outcome shutdownOutcome, completed bool) {
	results := make(chan shutdownOutcome, 1)
	handoff := &attemptHandoff{}
	go func() {
		start := time.Now()
		err := invokeShutdown(ctx, t, s, phase)
		out := shutdownOutcome{err: err, duration: time.Since(start)}
		handoff.mu.Lock()
		defer handoff.mu.Unlock()
		if handoff.abandoned {
			logLateCompletion(logger, t, phase, out)
			return
		}
		handoff.delivered = true
		results <- out
	}()

	select {
	case out := <-results:
		return out, true
	case <-ctx.Done():
	}
	handoff.mu.Lock()
	if handoff.delivered {
		handoff.mu.Unlock()
		return <-results, true
	}
	handoff.abandoned = true
	handoff.mu.Unlock()
	return shutdownOutcome{}, false
}

// invokeShutdown runs one Shutdown call with panic recovery on the invoking
// goroutine, so a recover on the waiting side is never needed.
func invokeShutdown(ctx context.Context, t reflect.Type, s shutdowner, phase PanicPhase) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &PanicError{
				Type:  t,
				Phase: phase,
				Value: recovered,
				Stack: observe.StackTrace(panicStackSize),
			}
		}
	}()
	return s.Shutdown(ctx)
}

func logLateCompletion(logger *slog.Logger, t reflect.Type, phase PanicPhase, out shutdownOutcome) {
	attrs := []slog.Attr{
		slog.String("type", t.String()),
		slog.String("phase", phase.String()),
		slog.Duration("duration", out.duration),
	}
	level := slog.LevelWarn
	if out.err != nil {
		attrs = append(attrs, slog.Any("error", out.err))
		level = slog.LevelError
	}
	logger.LogAttrs(context.Background(), level,
		"di: Shutdown returned after the shutdown boundary; result not reported", attrs...)
}

func isPanicError(err error) bool {
	_, ok := err.(*PanicError)
	return ok
}

// lateCleanup is the single owner of the best-effort cleanup for an instance
// whose construction completed after the shutdown context ended. It runs on
// its own goroutine, bounded by lateCleanupBudget, and only logs outcomes.
func (c *Container) lateCleanup(t reflect.Type, state entryState, value any, buildErr error) {
	c.mu.RLock()
	logger := c.logger
	c.mu.RUnlock()

	typeAttr := slog.String("type", t.String())
	if state == entryFailed {
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"di: construction failed after the shutdown context ended", typeAttr, slog.Any("error", buildErr))
		return
	}
	s, ok := value.(shutdowner)
	if !ok {
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"di: instance constructed after the shutdown context ended; no Shutdown method to call", typeAttr)
		return
	}
	logger.LogAttrs(context.Background(), slog.LevelWarn,
		"di: instance constructed after the shutdown context ended; running one bounded cleanup attempt",
		typeAttr, slog.Duration("budget", lateCleanupBudget))

	ctx, cancel := context.WithTimeout(context.Background(), lateCleanupBudget)
	defer cancel()
	out, completed := c.boundedShutdown(ctx, t, s, PhaseLateCleanup, logger)
	switch {
	case !completed:
		logger.LogAttrs(context.Background(), slog.LevelError,
			"di: late cleanup timed out; Shutdown may still be running", typeAttr,
			slog.Duration("budget", lateCleanupBudget))
	case out.err != nil:
		logger.LogAttrs(context.Background(), slog.LevelError,
			"di: late cleanup failed", typeAttr, slog.Duration("duration", out.duration), slog.Any("error", out.err))
	default:
		logger.LogAttrs(context.Background(), slog.LevelInfo,
			"di: late cleanup completed", typeAttr, slog.Duration("duration", out.duration))
	}
}

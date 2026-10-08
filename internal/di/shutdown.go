package di

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/credo-go/credo/internal/observe"
)

// lateCleanupBudget is the fixed bound for the single best-effort cleanup of
// an instance constructed after the shutdown context ended. It is deliberately
// not configurable.
const lateCleanupBudget = 5 * time.Second

// Shutdown runs the whole teardown — the ingress tier, then the internal tier —
// under one context and reports what it could not complete. It is the
// container's own drain, with no start phase to roll back: the App drives the
// tiers itself through BeginTeardown, StopTier and TeardownReport. A second
// call returns an error wrapping [ErrClosed].
func (c *Container) Shutdown(ctx context.Context) error {
	if err := c.BeginTeardown(nil, nil); err != nil {
		return err
	}
	c.StopTier(ctx, TierIngress)
	c.StopTier(ctx, TierInternal)
	if report := c.TeardownReport(ctx); report != nil {
		return report
	}
	return nil
}

// teardown is the drain's working state, shared by the two tier passes and
// owned by the goroutine that runs them. Instance states are shared with
// builders and read under Container.mu.
type teardown struct {
	// excluded holds units whose Start failed: they released what they
	// opened and are never shut down.
	excluded map[*Unit]bool
	// stuck holds units whose Start was abandoned: still running, they keep
	// their dependencies open and are never shut down.
	stuck map[*Unit]bool
	// retired holds units that no longer use their dependencies.
	retired map[*Unit]bool
	// attempts holds the teardown of each resource, by resource key.
	attempts map[any]*attempt
	// done holds the resource keys whose teardown completed, or that a failed
	// Start released: a holder built afterwards is retired at once.
	done map[any]bool
}

// attempt is one resource's teardown, run once through one holder.
type attempt struct {
	key      any
	holder   *Unit
	kind     teardownKind
	value    any
	started  time.Time
	finished bool
	state    ShutdownState
	err      error
	duration time.Duration
	handoff  *attemptHandoff
}

// node is a resource with a teardown — the holders sharing one identity — or
// a construction still running, in one snapshot of the graph.
type node struct {
	key     any
	members []*Unit
	// holder runs the teardown: the holder registered first among those that
	// have one. nil for a pending construction and for a stuck resource.
	holder  *Unit
	pending bool
	stuck   bool
	tier    Tier
	deps    []*node
	// dependents are the active nodes that depend on this one.
	dependents []*node
}

func (n *node) name() string {
	switch {
	case n.holder != nil:
		return n.holder.name
	default:
		return n.members[0].name
	}
}

// BeginTeardown opens the drain. excluded are the units whose Start failed,
// stuck those whose Start was abandoned at the deadline. It closes the
// registration window without entering the closing phase: resolutions stay
// admitted until the internal tier begins. A second call returns an error
// wrapping [ErrClosed].
func (c *Container) BeginTeardown(excluded, stuck []*Unit) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.td != nil || c.closing {
		return fmt.Errorf("di: Shutdown: %w", ErrClosed)
	}
	c.frozen = true
	td := &teardown{
		excluded: make(map[*Unit]bool),
		stuck:    make(map[*Unit]bool),
		retired:  make(map[*Unit]bool),
		attempts: make(map[any]*attempt),
		done:     make(map[any]bool),
	}
	for _, u := range excluded {
		td.excluded[u] = true
	}
	for _, u := range stuck {
		td.stuck[u] = true
	}
	c.td = td
	return nil
}

// StopTier stops one tier's components, consumers before their dependencies.
// The ingress tier runs while resolutions are still admitted, and its
// resources that no edge orders stop concurrently. The internal tier enters
// the closing phase first — every later Resolve returns an error wrapping
// [ErrClosed] — and stops one resource at a time, in reverse dependency order
// with reverse registration order as the tie-break; it also stops any ingress
// resource built after the ingress tier ran.
//
// A resource is eligible once every active component that depends on one of
// its holders — directly or through bindings that are not components — has
// retired. A pending construction blocks its dependencies until it
// completes. Each teardown is bounded by ctx: one that outlives it is
// abandoned, keeps its dependencies open, and is reported. Panics are
// recovered as *PanicError. Once ctx ends no further teardown starts and none
// is retried.
func (c *Container) StopTier(ctx context.Context, tier Tier) {
	c.mu.Lock()
	td := c.td
	if td == nil {
		c.mu.Unlock()
		return
	}
	if tier == TierInternal {
		c.closing = true
		c.shutdownCtx = ctx
	}
	logger := c.logger
	results := make(chan *attempt, len(c.units)+1)
	c.mu.Unlock()

	running := make(map[any]*attempt)
	for ctx.Err() == nil {
		ready, pending := c.nextReady(td, tier)
		launched := false
		for _, n := range ready {
			if tier == TierInternal && (len(running) > 0 || launched) {
				break
			}
			a := c.launch(ctx, td, n, results, logger)
			running[a.key] = a
			launched = true
		}
		if !launched && len(running) == 0 && !pending {
			break
		}
		select {
		case a := <-results:
			c.finish(td, a, logger)
			delete(running, a.key)
		case <-c.buildDone:
		case <-ctx.Done():
		}
	}

	// The boundary: take what completed, abandon what did not.
	for _, a := range running {
		a.handoff.mu.Lock()
		if !a.handoff.delivered {
			a.handoff.abandoned = true
			a.state = ShutdownRunning
		}
		a.handoff.mu.Unlock()
	}
	for {
		select {
		case a := <-results:
			c.finish(td, a, logger)
			continue
		default:
		}
		break
	}

	if tier == TierInternal {
		c.mu.Lock()
		c.teardownDone = true
		c.mu.Unlock()
	}
}

// launch starts one resource's teardown on a helper goroutine.
func (c *Container) launch(
	ctx context.Context, td *teardown, n *node, results chan<- *attempt, logger *slog.Logger,
) *attempt {
	c.mu.Lock()
	value := c.valueOf(n.holder)
	a := &attempt{
		key:     n.key,
		holder:  n.holder,
		kind:    n.holder.teardownOf(value),
		value:   value,
		started: time.Now(),
		handoff: &attemptHandoff{},
	}
	td.attempts[a.key] = a
	c.mu.Unlock()
	go func() {
		err := invokeTeardown(ctx, a.holder, a.kind, value, PhaseShutdown)
		duration := time.Since(a.started)
		a.handoff.mu.Lock()
		defer a.handoff.mu.Unlock()
		if a.handoff.abandoned {
			logLateCompletion(logger, a.holder, PhaseShutdown, shutdownOutcome{err: err, duration: duration})
			return
		}
		a.handoff.delivered = true
		a.err, a.duration = err, duration
		results <- a
	}()
	return a
}

// finish records a completed teardown and retires every holder of the
// resource, releasing their dependencies.
func (c *Container) finish(td *teardown, a *attempt, logger *slog.Logger) {
	a.finished = true
	switch {
	case a.err == nil:
		a.state = ShutdownSucceeded
		logger.LogAttrs(context.Background(), slog.LevelDebug, "di: shutdown succeeded",
			slog.String("component", a.holder.name), slog.Duration("duration", a.duration))
	case isPanicError(a.err):
		a.state = ShutdownPanicked
	default:
		a.state = ShutdownFailed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	td.done[a.key] = true
	for _, u := range c.units {
		if resourceKey(u) == a.key {
			td.retired[u] = true
		}
	}
}

// resourceKey is the key a built unit's resource is grouped under: its
// identity token, or the unit itself.
func resourceKey(u *Unit) any {
	if u.key == nil {
		return u
	}
	return u.key
}

// nextReady takes one consistent snapshot of the graph and returns the
// resources of tier that may be torn down now — for the internal tier, the
// most recently registered first — and whether a construction this pass
// must wait for is still running. The internal tier also takes ingress
// resources built after the ingress tier ran.
func (c *Container) nextReady(td *teardown, tier Tier) (ready []*node, pending bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	nodes := c.snapshotLocked(td)
	for _, n := range nodes {
		switch {
		case n.pending:
			if tier == TierInternal || n.tier == TierIngress ||
				slices.ContainsFunc(n.deps, func(d *node) bool { return d.tier == TierIngress }) {
				pending = true
			}
		case n.stuck || n.holder == nil || td.attempts[n.key] != nil || len(n.dependents) > 0:
		case tier == TierInternal || n.tier == tier:
			ready = append(ready, n)
		}
	}
	slices.SortFunc(ready, func(a, b *node) int { return cmp.Compare(b.holder.index, a.holder.index) })
	return ready, pending
}

// snapshotLocked groups the active units into nodes and links them. A unit
// is active while its value is built (or being built) and it has not
// retired. Holders that share a resource identity form one node; a resource
// none of whose holders has a teardown — a value that is no component, a
// borrowed resource — is transparent: edges pass through it. It retires
// failed builds and holders of a torn-down resource, so c.mu must be held
// for writing.
func (c *Container) snapshotLocked(td *teardown) []*node {
	byKey := make(map[any]*node)
	var nodes []*node
	for _, u := range c.units {
		if td.retired[u] {
			continue
		}
		e := c.entryOf(u)
		if e == nil {
			continue
		}
		switch {
		case e.state == entryFailed:
			td.retired[u] = true
			continue
		case e.state == entryUnbuilt || e.late:
			continue
		case e.state == entryBuilding:
			nodes = append(nodes, &node{key: pendingKey{u}, members: []*Unit{u}, pending: true, tier: u.Tier()})
			continue
		}
		key := resourceKey(u)
		if td.done[key] {
			td.retired[u] = true
			continue
		}
		n := byKey[key]
		if n == nil {
			n = &node{key: key}
			byKey[key] = n
			nodes = append(nodes, n)
		}
		n.members = append(n.members, u)
	}

	// Decide each resource's teardown; drop the transparent ones.
	active := nodes[:0]
	for _, n := range nodes {
		if n.pending {
			active = append(active, n)
			continue
		}
		excluded, borrowed := false, false
		for _, m := range n.members {
			excluded = excluded || td.excluded[m]
			borrowed = borrowed || m.borrowed
			n.stuck = n.stuck || td.stuck[m]
		}
		if excluded {
			td.done[n.key] = true
			for _, m := range n.members {
				td.retired[m] = true
			}
			continue
		}
		if !borrowed && !n.stuck {
			for _, m := range n.members {
				if m.teardownOf(c.valueOf(m)) != teardownNone {
					n.holder = m
					break
				}
			}
		}
		if n.holder == nil && !n.stuck {
			continue
		}
		n.tier = TierInternal
		switch {
		case n.holder != nil:
			n.tier = n.holder.Tier()
		default:
			for _, m := range n.members {
				if td.stuck[m] {
					n.tier = m.Tier()
				}
			}
		}
		active = append(active, n)
	}
	nodes = active

	nodeOf := make(map[*Unit]*node)
	for _, n := range nodes {
		for _, m := range n.members {
			nodeOf[m] = n
		}
	}
	for _, n := range nodes {
		seen := map[*Unit]bool{}
		linked := map[*node]bool{}
		var walk func(u *Unit)
		walk = func(u *Unit) {
			for _, d := range c.depUnitsLocked(u) {
				if seen[d] {
					continue
				}
				seen[d] = true
				dn := nodeOf[d]
				switch {
				case dn != nil && dn != n:
					if !linked[dn] {
						linked[dn] = true
						n.deps = append(n.deps, dn)
						dn.dependents = append(dn.dependents, n)
					}
				default:
					// A holder of the same resource, or a binding with no
					// teardown: its own dependencies are this node's.
					walk(d)
				}
			}
		}
		for _, m := range n.members {
			seen[m] = true
		}
		for _, m := range n.members {
			walk(m)
		}
	}
	return nodes
}

// pendingKey keys a construction that is still running.
type pendingKey struct{ u *Unit }

// TeardownReport classifies every unit at the boundary and returns nil when
// nothing failed or remained incomplete. Holders that share a resource are
// reported through the one that ran its teardown.
func (c *Container) TeardownReport(ctx context.Context) *ShutdownError {
	c.mu.Lock()
	defer c.mu.Unlock()
	td := c.td
	if td == nil {
		return nil
	}
	now := time.Now()
	nodes := c.snapshotLocked(td)
	nodeOf := make(map[*Unit]*node)
	for _, n := range nodes {
		for _, m := range n.members {
			nodeOf[m] = n
		}
	}

	entries := make([]ShutdownEntry, 0, len(c.units))
	var errs []error
	incomplete := false
	for _, u := range c.units {
		e := ShutdownEntry{Type: u.t, Name: u.name, Tier: u.Tier()}
		entry := c.entryOf(u)
		a := td.attempts[resourceKey(u)]
		switch {
		case td.excluded[u]:
			e.State = ShutdownStartFailed
		case td.stuck[u]:
			e.State = ShutdownStartRunning
		case a != nil && a.holder == u:
			e.State, e.Err, e.Duration = a.state, a.err, a.duration
			if a.state == ShutdownRunning {
				e.Duration = now.Sub(a.started)
			}
		case a != nil:
			e.State = ShutdownShared
		case entry == nil || entry.state == entryUnbuilt:
			e.State = ShutdownNeverConstructed
		case entry.state == entryFailed:
			e.State = ShutdownConstructionFailed
			e.Err = entry.err
		case entry.state == entryBuilding:
			e.State = ShutdownConstructing
			e.Duration = now.Sub(entry.buildStart)
		case entry.late:
			e.State = ShutdownLateCleanup
		case td.retired[u]:
			e.State = ShutdownRetired
		default:
			n := nodeOf[u]
			switch {
			case n == nil:
				e.State = ShutdownRetired
			case n.holder != u:
				e.State = ShutdownShared
			case len(n.dependents) > 0:
				e.State = ShutdownBlocked
				for _, d := range n.dependents {
					e.Blockers = append(e.Blockers, d.members[0].t)
					e.BlockerNames = append(e.BlockerNames, d.name())
				}
			default:
				e.State = ShutdownUnattempted
			}
		}
		switch e.State {
		case ShutdownFailed, ShutdownPanicked:
			incomplete = true
			errs = append(errs, fmt.Errorf("di: shutting down %s: %w", u.name, e.Err))
		case ShutdownRunning, ShutdownConstructing, ShutdownBlocked, ShutdownUnattempted, ShutdownLateCleanup,
			ShutdownStartRunning:
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

// shutdownOutcome is one teardown invocation's result.
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

// invokeTeardown runs one Shutdown or Close call with panic recovery on the
// invoking goroutine, so a recover on the waiting side is never needed.
func invokeTeardown(ctx context.Context, u *Unit, kind teardownKind, value any, phase PanicPhase) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &PanicError{
				Type:  u.t,
				Phase: phase,
				Value: recovered,
				Stack: observe.StackTrace(panicStackSize),
			}
		}
	}()
	switch kind {
	case teardownClose:
		return u.close(ctx, value)
	case teardownShutdown:
		return value.(shutdowner).Shutdown(ctx)
	}
	return nil
}

func logLateCompletion(logger *slog.Logger, u *Unit, phase PanicPhase, out shutdownOutcome) {
	attrs := []slog.Attr{
		slog.String("component", u.name),
		slog.String("phase", phase.String()),
		slog.Duration("duration", out.duration),
	}
	level := slog.LevelWarn
	if out.err != nil {
		attrs = append(attrs, slog.Any("error", out.err))
		level = slog.LevelError
	}
	logger.LogAttrs(context.Background(), level,
		"di: teardown returned after the shutdown boundary; result not reported", attrs...)
}

func isPanicError(err error) bool {
	_, ok := errors.AsType[*PanicError](err)
	return ok
}

// lateCleanup is the single owner of the best-effort cleanup for an instance
// whose construction completed after the shutdown context ended. It runs on
// its own goroutine, bounded by lateCleanupBudget, and only logs outcomes. A
// value whose resource another holder still holds, or whose resource was
// already torn down, is left to that teardown.
func (c *Container) lateCleanup(u *Unit, t reflect.Type, state entryState, value any, buildErr error) {
	c.mu.RLock()
	logger := c.logger
	shared := false
	if u != nil && u.key != nil {
		if c.td != nil && c.td.done[u.key] {
			shared = true
		}
		for _, h := range c.resources[u.key] {
			if e := c.entryOf(h); h != u && e != nil && e.state == entryBuilt && !e.late {
				shared = true
			}
			if h.borrowed {
				shared = true
			}
		}
	}
	c.mu.RUnlock()

	typeAttr := slog.String("type", t.String())
	if state == entryFailed {
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"di: construction failed after the shutdown context ended", typeAttr, slog.Any("error", buildErr))
		return
	}
	kind := teardownNone
	if u != nil {
		kind = u.teardownOf(value)
	}
	if kind == teardownNone || shared {
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"di: instance constructed after the shutdown context ended; no teardown of its own to run", typeAttr)
		return
	}
	logger.LogAttrs(context.Background(), slog.LevelWarn,
		"di: instance constructed after the shutdown context ended; running one bounded cleanup attempt",
		typeAttr, slog.Duration("budget", lateCleanupBudget))

	ctx, cancel := context.WithTimeout(context.Background(), lateCleanupBudget)
	defer cancel()
	out, completed := boundedTeardown(ctx, u, kind, value, logger)
	switch {
	case !completed:
		logger.LogAttrs(context.Background(), slog.LevelError,
			"di: late cleanup timed out; its teardown may still be running", typeAttr,
			slog.Duration("budget", lateCleanupBudget))
	case out.err != nil:
		logger.LogAttrs(context.Background(), slog.LevelError,
			"di: late cleanup failed", typeAttr, slog.Duration("duration", out.duration), slog.Any("error", out.err))
	default:
		logger.LogAttrs(context.Background(), slog.LevelInfo,
			"di: late cleanup completed", typeAttr, slog.Duration("duration", out.duration))
	}
}

// boundedTeardown invokes one teardown on a helper goroutine and waits for
// completion or the end of ctx, preferring an already-completed result at
// the boundary.
func boundedTeardown(
	ctx context.Context, u *Unit, kind teardownKind, value any, logger *slog.Logger,
) (outcome shutdownOutcome, completed bool) {
	results := make(chan shutdownOutcome, 1)
	handoff := &attemptHandoff{}
	go func() {
		start := time.Now()
		err := invokeTeardown(ctx, u, kind, value, PhaseLateCleanup)
		out := shutdownOutcome{err: err, duration: time.Since(start)}
		handoff.mu.Lock()
		defer handoff.mu.Unlock()
		if handoff.abandoned {
			logLateCompletion(logger, u, PhaseLateCleanup, out)
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

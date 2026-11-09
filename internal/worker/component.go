package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Engine is what the loops of one registry share: the logger they write
// through and the restart jitter.
type Engine struct {
	logger *slog.Logger
	// jitter picks a restart delay in [lo, hi]; uniformJitter unless a test
	// pins it to a bound.
	jitter func(lo, hi time.Duration) time.Duration
}

// NewEngine returns an engine whose loops log through logger, each scoped to
// its worker's component.
func NewEngine(logger *slog.Logger) *Engine {
	return &Engine{logger: logger, jitter: uniformJitter}
}

// Component is a worker's lifecycle component, named "worker:<name>": its
// Start launches the loop and its Shutdown stops it. The App starts and stops
// it once, in its tier.
type Component struct {
	e   *Engine
	def *Definition
	r   *runner

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	done    chan struct{} // closed when the loop has returned
}

// NewComponent returns the component that runs w under def. w is nil for a
// run function or a worker constructor, whose worker the component's
// constructor attaches with SetWorker before Start. The loop's records carry
// component=worker:<name>.
func NewComponent(e *Engine, def *Definition, w Worker) *Component {
	r := newRunner(def, w)
	r.logger = e.logger.With("component", ComponentName(def.name))
	return &Component{e: e, def: def, r: r}
}

// ComponentName is the name of a worker's component, under which readiness,
// the lifecycle's reports and the loop's records name it.
func ComponentName(name string) string { return "worker:" + name }

// Definition returns the worker's registration.
func (c *Component) Definition() *Definition { return c.def }

// SetWorker attaches the worker the component's constructor made, from a
// run function and its resolved parameters or from a worker constructor's
// value; the constructor calls it before Start.
func (c *Component) SetWorker(w Worker) { c.r.worker = w }

// Snapshot returns the worker's live state.
func (c *Component) Snapshot() Info { return c.r.snapshot() }

// Start derives the worker's own context from context.WithoutCancel(ctx),
// with a cancel only Shutdown calls, launches the loop on one goroutine and
// returns.
func (c *Component) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.stopped:
		return fmt.Errorf("worker: %q: Start after Shutdown", c.def.name)
	case c.started:
		return fmt.Errorf("worker: %q: Start called twice", c.def.name)
	}
	c.started = true
	workerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.cancel = cancel
	c.done = make(chan struct{})
	if c.def.kind == KindScheduled {
		c.r.setStatus(StatusWaiting)
	}
	go func() {
		defer close(c.done)
		defer cancel()
		c.e.driveLoop(workerCtx, c.r)
	}()
	return nil
}

// Shutdown cancels the worker's context and returns when Run and the loop
// have returned — not at ctx's deadline: a Run that ignores cancellation is
// abandoned by the App, which keeps the components the worker depends on
// open. A worker that never started shuts down at once.
func (c *Component) Shutdown(context.Context) error {
	c.mu.Lock()
	c.stopped = true
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	<-done
	return nil
}

func (c *Component) isStarted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// ReadyComponent is the component of a worker whose registration sets a
// readiness condition: the App plans Ready from the type, so a worker without
// one contributes nothing to readiness.
type ReadyComponent struct{ *Component }

// Ready evaluates the registration's readiness conditions against the
// runner's last state, in memory, without I/O.
func (c *ReadyComponent) Ready(context.Context) error {
	def := c.def
	if !c.isStarted() {
		if def.kind == KindScheduled && def.scheduled.UnreadyUntilFirstSuccess {
			return fmt.Errorf("worker %q has not started", def.name)
		}
		return nil
	}
	info := c.r.snapshot()
	if def.unreadyWhenFailed() && info.Status == StatusFailed {
		return fmt.Errorf("worker %q failed permanently: %s", def.name, info.LastError)
	}
	if def.kind != KindScheduled {
		return nil
	}
	if def.scheduled.UnreadyUntilFirstSuccess && info.LastSucceededAt.IsZero() {
		return fmt.Errorf("worker %q has no successful run yet", def.name)
	}
	if age := def.scheduled.UnreadyAfterSuccessAge; age > 0 && !info.LastSucceededAt.IsZero() {
		if since := time.Since(info.LastSucceededAt); since > age {
			return fmt.Errorf("worker %q last succeeded %s ago, limit %s", def.name, since.Round(time.Second), age)
		}
	}
	return nil
}

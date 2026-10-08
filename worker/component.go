package worker

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// component is a worker's lifecycle component, named "worker:<name>": its
// Start launches the loop and its Shutdown stops it. The App starts and stops
// it once, in its tier.
type component struct {
	s   *Supervisor
	def *definition
	r   *runner

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	done    chan struct{} // closed when the loop has returned
}

// Start derives the worker's own context from context.WithoutCancel(ctx),
// with a cancel only Shutdown calls, launches the loop on one goroutine and
// returns.
func (c *component) Start(ctx context.Context) error {
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
		c.s.driveLoop(workerCtx, c.r)
	}()
	return nil
}

// Shutdown cancels the worker's context and returns when Run and the loop
// have returned — not at ctx's deadline: a Run that ignores cancellation is
// abandoned by the App, which keeps the components the worker depends on
// open. A worker that never started shuts down at once.
func (c *component) Shutdown(context.Context) error {
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

func (c *component) isStarted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// readyComponent is the component of a worker whose registration sets a
// readiness condition: the App plans Ready from the type, so a worker without
// one contributes nothing to readiness.
type readyComponent struct{ *component }

// Ready evaluates the registration's readiness conditions against the
// runner's last state, in memory, without I/O.
func (c *readyComponent) Ready(context.Context) error {
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

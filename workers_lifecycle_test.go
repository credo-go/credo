package credo_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// lifecycleBlocking returns a worker that closes started on its first run,
// blocks until its context is cancelled and then closes stopped.
func lifecycleBlocking() (w workerFunc, started, stopped chan struct{}) {
	started, stopped = make(chan struct{}), make(chan struct{})
	var once sync.Once
	w = func(ctx context.Context) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		close(stopped)
		return nil
	}
	return w, started, stopped
}

// lifecycleStubborn returns a worker that ignores cancellation until release
// is closed, and closes returned once Run has returned.
func lifecycleStubborn() (w workerFunc, started, release, returned chan struct{}) {
	started, release, returned = make(chan struct{}), make(chan struct{}), make(chan struct{})
	w = func(context.Context) error {
		defer close(returned)
		close(started)
		<-release
		return nil
	}
	return w, started, release, returned
}

// workersLifecycleEntry returns the entry of the lifecycle error named name,
// and the number of entries with that name.
func workersLifecycleEntry(lerr *credo.LifecycleError, name string) (credo.LifecycleEntry, int) {
	var found credo.LifecycleEntry
	n := 0
	for _, e := range lerr.Entries {
		if e.Name == name {
			found = e
			n++
		}
	}
	return found, n
}

func TestWorkersLifecycle_OneComponentPerWorkerNamedAfterIt(t *testing.T) {
	app := mustNew(t)
	s := app.Workers()
	cont, contStarted, release1, contReturned := lifecycleStubborn()
	s.Continuous("consumer", cont)
	sched, schedStarted, release2, schedReturned := lifecycleStubborn()
	s.Scheduled("report", "@every 1h", sched, credo.ScheduledConfig{RunOnStart: true})

	if app.Has[*credo.Workers]() {
		t.Error("App.Workers bound the registry into the container; it binds nothing")
	}
	if _, ok := any(s).(credo.Starter); ok {
		t.Error("the registry has Start; the App starts each worker's component")
	}
	if _, ok := any(s).(credo.Component); ok {
		t.Error("the registry has Shutdown; the App stops each worker's component")
	}

	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	workersAwait(t, contStarted, "the continuous worker to run")
	workersAwait(t, schedStarted, "the scheduled worker to run")

	// Both workers ignore cancellation, so each component is abandoned and
	// named in the report: once, under worker:<name>, in its default tier.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 50*time.Millisecond)
	defer cancel()
	err := app.Shutdown(ctx)
	close(release1)
	close(release2)
	workersAwait(t, contReturned, "the continuous Run to return")
	workersAwait(t, schedReturned, "the scheduled Run to return")

	lerr, ok := errors.AsType[*credo.LifecycleError](err)
	if !ok {
		t.Fatalf("Shutdown() = %v, want a *credo.LifecycleError", err)
	}
	for _, want := range []struct {
		name string
		tier credo.Tier
	}{{"worker:consumer", credo.TierInternal}, {"worker:report", credo.TierIngress}} {
		entry, n := workersLifecycleEntry(lerr, want.name)
		if n != 1 {
			t.Errorf("Shutdown() = %v, want %s reported once, got %d", err, want.name, n)
			continue
		}
		if entry.Tier != want.tier || entry.Phase != credo.PhaseShutdown || entry.Outcome != credo.OutcomeAbandoned {
			t.Errorf("%s entry = %s, want (%s) shutdown abandoned", want.name, entry, want.tier)
		}
	}
	for _, e := range lerr.Entries {
		if !strings.HasPrefix(e.Name, "worker:") {
			t.Errorf("Shutdown() reports %q; the workers' only components are worker:<name>", e.Name)
		}
	}
}

// Lifecycle pipeline: an HTTP handler enqueues to an in-process continuous
// worker that writes through a database component.
type lifecycleDB struct {
	consumerDone *atomic.Bool

	mu                  sync.Mutex
	rows                []string
	closed              bool
	consumerDoneAtClose bool
}

func (d *lifecycleDB) write(row string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errors.New("database closed")
	}
	d.rows = append(d.rows, row)
	return nil
}

func (d *lifecycleDB) Shutdown(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	d.consumerDoneAtClose = d.consumerDone.Load()
	return nil
}

type lifecycleQueue struct{ jobs chan string }

type lifecycleConsumer struct {
	queue    *lifecycleQueue
	db       *lifecycleDB
	running  chan struct{}
	stopping atomic.Bool
	done     atomic.Bool
}

func (w *lifecycleConsumer) Run(ctx context.Context) error {
	close(w.running)
	for {
		select {
		case job := <-w.queue.jobs:
			if err := w.db.write(job); err != nil {
				return err
			}
		case <-ctx.Done():
			w.stopping.Store(true)
			for {
				select {
				case job := <-w.queue.jobs:
					if err := w.db.write(job); err != nil {
						return err
					}
				default:
					w.done.Store(true)
					return nil
				}
			}
		}
	}
}

func TestWorkersLifecycle_TiersAgainstTheHTTPDrain(t *testing.T) {
	app := mustNew(t)
	consumer := &lifecycleConsumer{running: make(chan struct{})}
	db := &lifecycleDB{consumerDone: &consumer.done}
	queue := &lifecycleQueue{jobs: make(chan string, 1)}
	app.ProvideValue(queue)
	app.Provide(func() *lifecycleDB { return db })
	s := app.Workers()
	s.Continuous("consumer", func(q *lifecycleQueue, d *lifecycleDB) *lifecycleConsumer {
		consumer.queue, consumer.db = q, d
		return consumer
	})
	sched, schedStarted, schedStopped := lifecycleBlocking()
	s.Scheduled("report", "@every 1h", sched, credo.ScheduledConfig{RunOnStart: true})
	external, externalStarted, externalStopped := lifecycleBlocking()
	s.Continuous("external", external, credo.ContinuousConfig{Tier: credo.TierIngress})

	entered, release := make(chan struct{}), make(chan struct{})
	var stoppingAtEnqueue atomic.Bool
	app.POST("/orders", func(ctx *credo.Context) error {
		close(entered)
		<-release
		stoppingAtEnqueue.Store(consumer.stopping.Load())
		queue.jobs <- "order-1"
		return ctx.Response().NoContent(http.StatusAccepted)
	})

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- app.ServeContext(context.WithoutCancel(t.Context()), l) }()
	workersAwait(t, consumer.running, "the consumer to run")
	workersAwait(t, schedStarted, "the scheduled worker to run")
	workersAwait(t, externalStarted, "the ingress continuous worker to run")

	status := make(chan int, 1)
	go func() {
		resp, err := http.Post("http://"+l.Addr().String()+"/orders", "text/plain", nil)
		if err != nil {
			t.Errorf("POST /orders: %v", err)
			status <- 0
			return
		}
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	workersAwait(t, entered, "the request to reach the handler")

	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		shutdownErr <- app.Shutdown(ctx)
	}()

	// The ingress tier stops concurrently with the HTTP drain: both ingress
	// workers return while the request is still in flight.
	workersAwait(t, schedStopped, "the scheduled worker to stop during the HTTP drain")
	workersAwait(t, externalStopped, "the ingress continuous worker to stop during the HTTP drain")
	if consumer.stopping.Load() {
		t.Error("the internal continuous worker was cancelled before the HTTP drain ended")
	}

	close(release)
	if got := <-status; got != http.StatusAccepted {
		t.Errorf("POST /orders = %d, want %d", got, http.StatusAccepted)
	}
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if err := <-served; err != nil {
		t.Errorf("ServeContext() = %v", err)
	}

	if stoppingAtEnqueue.Load() {
		t.Error("the handler enqueued after the internal worker was cancelled")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if !slices.Equal(db.rows, []string{"order-1"}) {
		t.Errorf("database rows = %v, want the job enqueued during the drain", db.rows)
	}
	if !db.closed {
		t.Error("the database component was not shut down")
	}
	if !db.consumerDoneAtClose {
		t.Error("the database was shut down before the worker that depends on it returned")
	}
	if info, _ := s.Lookup("consumer"); info.Status != credo.WorkerStatusStopped || info.LastError != "" {
		t.Errorf("consumer = %s (%q), want stopped without an error", info.Status, info.LastError)
	}
}

func TestWorkersLifecycle_StatusTransitions(t *testing.T) {
	app := mustNew(t)
	s := app.Workers()
	cont, contStarted, contStopped := lifecycleBlocking()
	s.Continuous("consumer", cont)
	sched, _, _ := lifecycleBlocking()
	s.Scheduled("report", "@every 1h", sched)

	status := func(name string) credo.WorkerStatus {
		t.Helper()
		info, ok := s.Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) = false", name)
		}
		return info.Status
	}
	for _, name := range []string{"consumer", "report"} {
		if got := status(name); got != credo.WorkerStatusPending {
			t.Errorf("%s before start = %s, want pending", name, got)
		}
	}

	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	// Start sets a scheduled worker waiting before it returns; a continuous
	// worker is running once its first run is admitted.
	if got := status("report"); got != credo.WorkerStatusWaiting {
		t.Errorf("report after start = %s, want waiting", got)
	}
	workersAwait(t, contStarted, "the continuous worker to run")
	if got := status("consumer"); got != credo.WorkerStatusRunning {
		t.Errorf("consumer after start = %s, want running", got)
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	workersAwait(t, contStopped, "the continuous Run to return")
	for _, name := range []string{"consumer", "report"} {
		if got := status(name); got != credo.WorkerStatusStopped {
			t.Errorf("%s after Shutdown = %s, want stopped", name, got)
		}
	}
}

// lifecycleFailingStart is a managed component whose Start fails.
type lifecycleFailingStart struct{ shutdowns atomic.Int32 }

func (c *lifecycleFailingStart) Start(context.Context) error { return errors.New("broker unreachable") }
func (c *lifecycleFailingStart) Shutdown(context.Context) error {
	c.shutdowns.Add(1)
	return nil
}

func TestWorkersLifecycle_WorkerNeverStartedStaysPending(t *testing.T) {
	capture := newLogCapture()
	app := mustNew(t, credo.WithLogger(capture.logger()))
	failing := &lifecycleFailingStart{}
	app.Manage(failing, credo.ComponentName("broker"))

	var runs atomic.Int32
	run := workerFunc(func(ctx context.Context) error {
		runs.Add(1)
		<-ctx.Done()
		return nil
	})
	s := app.Workers()
	s.Continuous("consumer", run)
	s.Scheduled("report", "@every 1h", run, credo.ScheduledConfig{RunOnStart: true})

	err := app.Start(t.Context())
	lerr, ok := errors.AsType[*credo.LifecycleError](err)
	if !ok {
		t.Fatalf("App.Start() = %v, want a *credo.LifecycleError", err)
	}
	entry, n := workersLifecycleEntry(lerr, "broker")
	if n != 1 || entry.Phase != credo.PhaseStart || entry.Outcome != credo.OutcomeFailed ||
		!strings.Contains(entry.Err.Error(), "broker unreachable") {
		t.Errorf("App.Start() = %v, want broker start failed", err)
	}
	for _, e := range lerr.Entries {
		if strings.HasPrefix(e.Name, "worker:") {
			t.Errorf("App.Start() reports %s; a worker that never started has nothing to report", e)
		}
	}
	if got := app.State(); got != "stopped" {
		t.Errorf("State() = %q, want stopped", got)
	}

	if got := runs.Load(); got != 0 {
		t.Errorf("Run called %d times, want 0", got)
	}
	for _, info := range s.Snapshot() {
		if info.Status != credo.WorkerStatusPending || !info.LastStartedAt.IsZero() {
			t.Errorf("%s = %s (last started %v), want pending and never admitted",
				info.Name, info.Status, info.LastStartedAt)
		}
	}
	for _, msg := range []string{"worker started", "worker stopped"} {
		if lines := capture.withMessage(msg); len(lines) != 0 {
			t.Errorf("%q logged %d times, want none for a worker that never started", msg, len(lines))
		}
	}
}

func TestWorkersLifecycle_StartContextEndsWithTheCall(t *testing.T) {
	app := mustNew(t)
	runCtx := make(chan context.Context, 1)
	w, started, stopped := lifecycleBlocking()
	app.Workers().Continuous("consumer", workerFunc(func(ctx context.Context) error {
		runCtx <- ctx
		return w(ctx)
	}))

	ctx, cancel := context.WithCancel(t.Context())
	if err := app.Start(ctx); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	cancel()
	workersAwait(t, started, "the worker to run")
	got := <-runCtx
	if err := got.Err(); err != nil {
		t.Fatalf("the run context ended with Start's context: %v", err)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer cancelShutdown()
	if err := app.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	workersAwait(t, stopped, "the worker to stop")
	if !errors.Is(got.Err(), context.Canceled) {
		t.Errorf("run context after Shutdown = %v, want canceled", got.Err())
	}
}

// lifecycleStore is a dependency component of a constructed worker.
type lifecycleStore struct{ shutdowns atomic.Int32 }

func (d *lifecycleStore) Shutdown(context.Context) error {
	d.shutdowns.Add(1)
	return nil
}

// lifecycleStubbornWorker uses a lifecycleStore and ignores cancellation
// until released.
type lifecycleStubbornWorker struct {
	store *lifecycleStore
	run   workerFunc
}

func (w *lifecycleStubbornWorker) Run(ctx context.Context) error { return w.run(ctx) }

func TestWorkersLifecycle_RunIgnoringCancellationIsAbandoned(t *testing.T) {
	t.Run("constructed continuous worker keeps its dependency open", func(t *testing.T) {
		capture := newLogCapture()
		app := mustNew(t, credo.WithLogger(capture.logger()))
		stopLine := awaitWorkerLog(capture, "worker stopped", "stubborn")
		store := &lifecycleStore{}
		run, started, release, returned := lifecycleStubborn()
		app.Provide(func() *lifecycleStore { return store })
		s := app.Workers()
		s.Continuous("stubborn", func(s *lifecycleStore) *lifecycleStubbornWorker {
			return &lifecycleStubbornWorker{store: s, run: run}
		})

		if err := app.Start(t.Context()); err != nil {
			t.Fatalf("App.Start() = %v", err)
		}
		workersAwait(t, started, "the worker to run")

		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 50*time.Millisecond)
		defer cancel()
		err := app.Shutdown(ctx)
		shutdownsAtReturn := store.shutdowns.Load()
		// Release the run so the test leaves no goroutine behind.
		close(release)
		workersAwait(t, returned, "the Run to return")
		workersAwait(t, stopLine, "the worker's stop line")

		lerr, ok := errors.AsType[*credo.LifecycleError](err)
		if !ok {
			t.Fatalf("Shutdown() = %v, want a *credo.LifecycleError", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Shutdown() = %v, want the deadline as its cause", err)
		}
		entry, n := workersLifecycleEntry(lerr, "worker:stubborn")
		if n != 1 || entry.Outcome != credo.OutcomeAbandoned || entry.Tier != credo.TierInternal ||
			entry.Phase != credo.PhaseShutdown {
			t.Fatalf("Shutdown() = %v, want worker:stubborn (internal) shutdown abandoned, once", err)
		}
		if !strings.Contains(err.Error(), "worker:stubborn (internal) shutdown abandoned") {
			t.Errorf("Shutdown() = %q, want the text to name worker:stubborn as abandoned", err)
		}
		if shutdownsAtReturn != 0 {
			t.Error("the store was shut down while the worker that uses it was still running")
		}
		if got := store.shutdowns.Load(); got != 0 {
			t.Errorf("store shut down %d times after the abandoned run returned, want 0 (kept open)", got)
		}
		kept, n := workersLifecycleEntry(lerr, "*credo_test.lifecycleStore")
		if n != 1 || kept.Outcome != credo.OutcomeKeptOpen || !slices.Equal(kept.KeptOpenBy, []string{"worker:stubborn"}) {
			t.Errorf("Shutdown() = %v, want the store reported kept_open by worker:stubborn", err)
		}
		if info, _ := s.Lookup("stubborn"); info.Status != credo.WorkerStatusStopped {
			t.Errorf("stubborn after its Run returned = %s, want stopped", info.Status)
		}
	})

	t.Run("scheduled worker in the ingress tier", func(t *testing.T) {
		app := mustNew(t)
		run, started, release, returned := lifecycleStubborn()
		app.Workers().Scheduled("report", "@every 1h", run, credo.ScheduledConfig{RunOnStart: true})
		if err := app.Start(t.Context()); err != nil {
			t.Fatalf("App.Start() = %v", err)
		}
		workersAwait(t, started, "the worker to run")

		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 50*time.Millisecond)
		defer cancel()
		err := app.Shutdown(ctx)
		close(release)
		workersAwait(t, returned, "the Run to return")

		if err == nil || !strings.Contains(err.Error(), "worker:report (ingress) shutdown abandoned") {
			t.Fatalf("Shutdown() = %v, want worker:report (ingress) shutdown abandoned", err)
		}
	})
}

// raceCountingWorker blocks until cancelled and counts its runs.
func raceCountingWorker(runs *atomic.Int32) workerFunc {
	return func(ctx context.Context) error {
		runs.Add(1)
		<-ctx.Done()
		return nil
	}
}

// raceGate is a dependency component whose Start blocks until the start phase
// is interrupted.
type raceGate struct {
	entered   chan struct{}
	shutdowns atomic.Int32
}

// raceGatedWorker is a constructed worker that depends on a raceGate.
type raceGatedWorker struct{ runs atomic.Int32 }

// TestApp_ShutdownDuringStartWalkStartsNoWorker: a Shutdown that interrupts
// the start walk before the workers' turn starts none of them; they stay
// pending, write no lifecycle line, and App.Start reports the interruption.
func TestApp_ShutdownDuringStartWalkStartsNoWorker(t *testing.T) {
	capture := newLogCapture()
	app := mustNew(t, credo.WithLogger(capture.logger()))
	gate := &raceGate{entered: make(chan struct{})}
	gated := &raceGatedWorker{}
	app.Provide(func() *raceGate { return gate })
	s := app.Workers()
	s.Continuous("gated", func(*raceGate) *raceGatedWorker { return gated })
	var byValueRuns atomic.Int32
	s.Scheduled("report", "@every 1h", raceCountingWorker(&byValueRuns), credo.ScheduledConfig{RunOnStart: true})

	startErr := make(chan error, 1)
	go func() { startErr <- app.Start(t.Context()) }()
	workersAwait(t, gate.entered, "the dependency's Start")

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown during the start phase = %v, want nil after a clean rollback", err)
	}
	select {
	case err := <-startErr:
		if err == nil || !strings.Contains(err.Error(), "shut down") {
			t.Fatalf("App.Start interrupted by Shutdown = %v, want an error saying so", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("App.Start did not return")
	}

	if got := app.State(); got != "stopped" {
		t.Errorf("State() = %q, want stopped", got)
	}
	if got := gate.shutdowns.Load(); got != 1 {
		t.Errorf("the started dependency was shut down %d times, want 1", got)
	}
	if got := gated.runs.Load() + byValueRuns.Load(); got != 0 {
		t.Errorf("workers ran %d times, want 0", got)
	}
	for _, info := range s.Snapshot() {
		if info.Status != credo.WorkerStatusPending {
			t.Errorf("%s = %s, want pending (its component never started)", info.Name, info.Status)
		}
	}
	for _, msg := range []string{"worker started", "worker stopped"} {
		if lines := capture.withMessage(msg); len(lines) != 0 {
			t.Errorf("%q logged %d times, want none", msg, len(lines))
		}
	}
}

func (g *raceGate) Start(ctx context.Context) error {
	close(g.entered)
	<-ctx.Done()
	return nil
}

func (g *raceGate) Shutdown(context.Context) error {
	g.shutdowns.Add(1)
	return nil
}

func (w *raceGatedWorker) Run(ctx context.Context) error {
	w.runs.Add(1)
	<-ctx.Done()
	return nil
}

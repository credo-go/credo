package worker

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

// lifecycleAwait waits for ch to close, failing the test after five seconds.
// It is a safety net against hangs, not a timing assertion.
func lifecycleAwait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// lifecycleBlocking returns a worker that closes started on its first run,
// blocks until its context is cancelled and then closes stopped.
func lifecycleBlocking() (w Func, started, stopped chan struct{}) {
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
func lifecycleStubborn() (w Func, started, release, returned chan struct{}) {
	started, release, returned = make(chan struct{}), make(chan struct{}), make(chan struct{})
	w = func(context.Context) error {
		defer close(returned)
		close(started)
		<-release
		return nil
	}
	return w, started, release, returned
}

// lifecycleEntry returns the entry of the lifecycle error named name, and the
// number of entries with that name.
func lifecycleEntry(lerr *credo.LifecycleError, name string) (credo.LifecycleEntry, int) {
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

// lifecycleStoppedLine returns a channel closed when the capture sees the
// "worker stopped" line of name. Call it before the worker starts.
func lifecycleStoppedLine(capture *logCapture, name string) <-chan struct{} {
	ch := make(chan struct{})
	var once sync.Once
	capture.setObserve(func(entry capturedLog) {
		if entry.Message == "worker stopped" && entry.Attrs["worker"] == name {
			once.Do(func() { close(ch) })
		}
	})
	return ch
}

func TestLifecycle_OneComponentPerWorkerNamedAfterIt(t *testing.T) {
	app := newTestApp(t)
	s := Use(app)
	cont, contStarted, release1, contReturned := lifecycleStubborn()
	s.Continuous("consumer", cont)
	sched, schedStarted, release2, schedReturned := lifecycleStubborn()
	s.Scheduled("report", "@every 1h", sched, ScheduledConfig{RunOnStart: true})

	if app.Has[*Supervisor]() {
		t.Error("Use bound the supervisor into the container; it binds nothing")
	}
	if _, ok := any(s).(credo.Starter); ok {
		t.Error("the supervisor has Start; the App starts each worker's component")
	}
	if _, ok := any(s).(credo.Component); ok {
		t.Error("the supervisor has Shutdown; the App stops each worker's component")
	}

	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	lifecycleAwait(t, contStarted, "the continuous worker to run")
	lifecycleAwait(t, schedStarted, "the scheduled worker to run")

	// Both workers ignore cancellation, so each component is abandoned and
	// named in the report: once, under worker:<name>, in its default tier.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 50*time.Millisecond)
	defer cancel()
	err := app.Shutdown(ctx)
	close(release1)
	close(release2)
	lifecycleAwait(t, contReturned, "the continuous Run to return")
	lifecycleAwait(t, schedReturned, "the scheduled Run to return")

	lerr, ok := errors.AsType[*credo.LifecycleError](err)
	if !ok {
		t.Fatalf("Shutdown() = %v, want a *credo.LifecycleError", err)
	}
	for _, want := range []struct {
		name string
		tier credo.Tier
	}{{"worker:consumer", credo.TierInternal}, {"worker:report", credo.TierIngress}} {
		entry, n := lifecycleEntry(lerr, want.name)
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

func TestLifecycle_TiersAgainstTheHTTPDrain(t *testing.T) {
	app := newTestApp(t)
	consumer := &lifecycleConsumer{running: make(chan struct{})}
	db := &lifecycleDB{consumerDone: &consumer.done}
	queue := &lifecycleQueue{jobs: make(chan string, 1)}
	app.ProvideValue(queue)
	app.Provide[*lifecycleDB](func() *lifecycleDB { return db })
	app.Provide[*lifecycleConsumer](func(q *lifecycleQueue, d *lifecycleDB) *lifecycleConsumer {
		consumer.queue, consumer.db = q, d
		return consumer
	})

	s := Use(app)
	s.ContinuousProvided[*lifecycleConsumer]("consumer")
	sched, schedStarted, schedStopped := lifecycleBlocking()
	s.Scheduled("report", "@every 1h", sched, ScheduledConfig{RunOnStart: true})
	external, externalStarted, externalStopped := lifecycleBlocking()
	s.Continuous("external", external, ContinuousConfig{Tier: credo.TierIngress})

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
	lifecycleAwait(t, consumer.running, "the consumer to run")
	lifecycleAwait(t, schedStarted, "the scheduled worker to run")
	lifecycleAwait(t, externalStarted, "the ingress continuous worker to run")

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
	lifecycleAwait(t, entered, "the request to reach the handler")

	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		shutdownErr <- app.Shutdown(ctx)
	}()

	// The ingress tier stops concurrently with the HTTP drain: both ingress
	// workers return while the request is still in flight.
	lifecycleAwait(t, schedStopped, "the scheduled worker to stop during the HTTP drain")
	lifecycleAwait(t, externalStopped, "the ingress continuous worker to stop during the HTTP drain")
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
	if info, _ := s.Lookup("consumer"); info.Status != StatusStopped || info.LastError != "" {
		t.Errorf("consumer = %s (%q), want stopped without an error", info.Status, info.LastError)
	}
}

func TestLifecycle_StatusTransitions(t *testing.T) {
	app := newTestApp(t)
	s := Use(app)
	cont, contStarted, contStopped := lifecycleBlocking()
	s.Continuous("consumer", cont)
	sched, _, _ := lifecycleBlocking()
	s.Scheduled("report", "@every 1h", sched)

	status := func(name string) Status {
		t.Helper()
		info, ok := s.Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) = false", name)
		}
		return info.Status
	}
	for _, name := range []string{"consumer", "report"} {
		if got := status(name); got != StatusPending {
			t.Errorf("%s before start = %s, want pending", name, got)
		}
	}

	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	// Start sets a scheduled worker waiting before it returns; a continuous
	// worker is running once its first run is admitted.
	if got := status("report"); got != StatusWaiting {
		t.Errorf("report after start = %s, want waiting", got)
	}
	lifecycleAwait(t, contStarted, "the continuous worker to run")
	if got := status("consumer"); got != StatusRunning {
		t.Errorf("consumer after start = %s, want running", got)
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	lifecycleAwait(t, contStopped, "the continuous Run to return")
	for _, name := range []string{"consumer", "report"} {
		if got := status(name); got != StatusStopped {
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

func TestLifecycle_WorkerNeverStartedStaysPending(t *testing.T) {
	capture := newLogCapture()
	app := newTestApp(t, credo.WithLogger(capture.logger()))
	failing := &lifecycleFailingStart{}
	app.Manage(failing, credo.Named("broker"))

	var runs atomic.Int32
	run := Func(func(ctx context.Context) error {
		runs.Add(1)
		<-ctx.Done()
		return nil
	})
	s := Use(app)
	s.Continuous("consumer", run)
	s.Scheduled("report", "@every 1h", run, ScheduledConfig{RunOnStart: true})

	err := app.Start(t.Context())
	lerr, ok := errors.AsType[*credo.LifecycleError](err)
	if !ok {
		t.Fatalf("App.Start() = %v, want a *credo.LifecycleError", err)
	}
	entry, n := lifecycleEntry(lerr, "broker")
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
		if info.Status != StatusPending || !info.LastStartedAt.IsZero() {
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

func TestLifecycle_StartContextEndsWithTheCall(t *testing.T) {
	app := newTestApp(t)
	runCtx := make(chan context.Context, 1)
	w, started, stopped := lifecycleBlocking()
	Use(app).Continuous("consumer", Func(func(ctx context.Context) error {
		runCtx <- ctx
		return w(ctx)
	}))

	ctx, cancel := context.WithCancel(t.Context())
	if err := app.Start(ctx); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	cancel()
	lifecycleAwait(t, started, "the worker to run")
	got := <-runCtx
	if err := got.Err(); err != nil {
		t.Fatalf("the run context ended with Start's context: %v", err)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer cancelShutdown()
	if err := app.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	lifecycleAwait(t, stopped, "the worker to stop")
	if !errors.Is(got.Err(), context.Canceled) {
		t.Errorf("run context after Shutdown = %v, want canceled", got.Err())
	}
}

// lifecycleStore is a dependency component of a provided worker.
type lifecycleStore struct{ shutdowns atomic.Int32 }

func (d *lifecycleStore) Shutdown(context.Context) error {
	d.shutdowns.Add(1)
	return nil
}

// lifecycleStubbornWorker uses a lifecycleStore and ignores cancellation
// until released.
type lifecycleStubbornWorker struct {
	store *lifecycleStore
	run   Func
}

func (w *lifecycleStubbornWorker) Run(ctx context.Context) error { return w.run(ctx) }

func TestLifecycle_RunIgnoringCancellationIsAbandoned(t *testing.T) {
	t.Run("provided continuous worker keeps its dependency open", func(t *testing.T) {
		capture := newLogCapture()
		app := newTestApp(t, credo.WithLogger(capture.logger()))
		stopLine := lifecycleStoppedLine(capture, "stubborn")
		store := &lifecycleStore{}
		run, started, release, returned := lifecycleStubborn()
		app.Provide[*lifecycleStore](func() *lifecycleStore { return store })
		app.Provide[*lifecycleStubbornWorker](func(s *lifecycleStore) *lifecycleStubbornWorker {
			return &lifecycleStubbornWorker{store: s, run: run}
		})
		s := Use(app)
		s.ContinuousProvided[*lifecycleStubbornWorker]("stubborn")

		if err := app.Start(t.Context()); err != nil {
			t.Fatalf("App.Start() = %v", err)
		}
		lifecycleAwait(t, started, "the worker to run")

		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 50*time.Millisecond)
		defer cancel()
		err := app.Shutdown(ctx)
		shutdownsAtReturn := store.shutdowns.Load()
		// Release the run so the test leaves no goroutine behind.
		close(release)
		lifecycleAwait(t, returned, "the Run to return")
		lifecycleAwait(t, stopLine, "the worker's stop line")

		lerr, ok := errors.AsType[*credo.LifecycleError](err)
		if !ok {
			t.Fatalf("Shutdown() = %v, want a *credo.LifecycleError", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Shutdown() = %v, want the deadline as its cause", err)
		}
		entry, n := lifecycleEntry(lerr, "worker:stubborn")
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
		kept, n := lifecycleEntry(lerr, "*worker.lifecycleStore")
		if n != 1 || kept.Outcome != credo.OutcomeKeptOpen || !slices.Equal(kept.KeptOpenBy, []string{"worker:stubborn"}) {
			t.Errorf("Shutdown() = %v, want the store reported kept_open by worker:stubborn", err)
		}
		if info, _ := s.Lookup("stubborn"); info.Status != StatusStopped {
			t.Errorf("stubborn after its Run returned = %s, want stopped", info.Status)
		}
	})

	t.Run("scheduled worker in the ingress tier", func(t *testing.T) {
		app := newTestApp(t)
		run, started, release, returned := lifecycleStubborn()
		Use(app).Scheduled("report", "@every 1h", run, ScheduledConfig{RunOnStart: true})
		if err := app.Start(t.Context()); err != nil {
			t.Fatalf("App.Start() = %v", err)
		}
		lifecycleAwait(t, started, "the worker to run")

		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 50*time.Millisecond)
		defer cancel()
		err := app.Shutdown(ctx)
		close(release)
		lifecycleAwait(t, returned, "the Run to return")

		if err == nil || !strings.Contains(err.Error(), "worker:report (ingress) shutdown abandoned") {
			t.Fatalf("Shutdown() = %v, want worker:report (ingress) shutdown abandoned", err)
		}
	})
}

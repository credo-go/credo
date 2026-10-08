package worker

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/credo-go/credo"
)

// raceCountingWorker blocks until cancelled and counts its runs.
func raceCountingWorker(runs *atomic.Int32) Func {
	return func(ctx context.Context) error {
		runs.Add(1)
		<-ctx.Done()
		return nil
	}
}

func TestComponent_ShutdownBeforeStart(t *testing.T) {
	var runs atomic.Int32
	c := addWorker(newTestSupervisor(), continuousDef("w"), raceCountingWorker(&runs))

	// A component that never started shuts down at once, whatever its context.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Shutdown(cancelled); err != nil {
		t.Fatalf("Shutdown before Start = %v, want nil", err)
	}
	err := c.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), `"w"`) || !strings.Contains(err.Error(), "Start after Shutdown") {
		t.Fatalf("Start after Shutdown = %v, want a refusal naming the worker", err)
	}
	if got := runs.Load(); got != 0 {
		t.Errorf("Run called %d times, want 0", got)
	}
	if got := c.r.status(); got != StatusPending {
		t.Errorf("status = %s, want pending", got)
	}
}

func TestComponent_StartTwiceRefused(t *testing.T) {
	var runs atomic.Int32
	c := addWorker(newTestSupervisor(), continuousDef("w"), raceCountingWorker(&runs))
	startWorker(t, c)
	err := c.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), `"w"`) || !strings.Contains(err.Error(), "Start called twice") {
		t.Fatalf("second Start = %v, want a refusal naming the worker", err)
	}
}

func TestComponent_ConcurrentShutdownsReturnAfterTheLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var returned atomic.Bool
		c := addWorker(newTestSupervisor(), continuousDef("w"), Func(func(ctx context.Context) error {
			<-ctx.Done()
			<-release // a final flush that outlives the cancellation
			returned.Store(true)
			return nil
		}))
		if err := c.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		// The deadline passing does not make Shutdown return: it waits for Run.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		var done [2]atomic.Bool
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() {
				errs[i] = c.Shutdown(ctx)
				if !returned.Load() {
					t.Errorf("Shutdown #%d returned before Run", i+1)
				}
				done[i].Store(true)
			})
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		for i := range done {
			if done[i].Load() {
				t.Fatalf("Shutdown #%d returned while Run was still flushing", i+1)
			}
		}

		close(release)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("Shutdown #%d = %v, want nil", i+1, err)
			}
		}
		if got := c.r.status(); got != StatusStopped {
			t.Errorf("status = %s, want stopped", got)
		}
	})
}

func TestComponent_ShutdownAfterCompletionIsStable(t *testing.T) {
	var runs atomic.Int32
	c := addWorker(newTestSupervisor(), continuousDef("w"), raceCountingWorker(&runs))
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("first Shutdown = %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for i := range 100 {
		if err := c.Shutdown(cancelled); err != nil {
			t.Fatalf("Shutdown #%d with a cancelled ctx = %v, want nil after completion", i+2, err)
		}
	}
}

// TestComponent_StartShutdownRace: a Start racing a Shutdown is either
// refused — and launches nothing — or has its loop joined before Shutdown
// returns, so a nil Shutdown never leaves a loop running.
func TestComponent_StartShutdownRace(t *testing.T) {
	for range 200 {
		var runs atomic.Int32
		c := addWorker(newTestSupervisor(), continuousDef("w"), raceCountingWorker(&runs))

		var wg sync.WaitGroup
		var startErr, shutdownErr error
		wg.Go(func() { startErr = c.Start(context.Background()) })
		wg.Go(func() { shutdownErr = c.Shutdown(context.Background()) })
		wg.Wait()

		if shutdownErr != nil {
			t.Fatalf("Shutdown = %v", shutdownErr)
		}
		if startErr != nil {
			if !strings.Contains(startErr.Error(), "Start after Shutdown") {
				t.Fatalf("Start = %v, want the refusal", startErr)
			}
			if got := runs.Load(); got != 0 {
				t.Fatalf("refused Start ran the worker %d times", got)
			}
			continue
		}
		select {
		case <-c.done:
		default:
			t.Fatal("Start succeeded and Shutdown returned, but the loop is still running")
		}
		if got := c.r.status(); got != StatusStopped {
			t.Fatalf("status = %s, want stopped", got)
		}
	}
}

// raceGate is a dependency component whose Start blocks until the start phase
// is interrupted.
type raceGate struct {
	entered   chan struct{}
	shutdowns atomic.Int32
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

// raceGatedWorker is a provided worker that depends on a raceGate.
type raceGatedWorker struct{ runs atomic.Int32 }

func (w *raceGatedWorker) Run(ctx context.Context) error {
	w.runs.Add(1)
	<-ctx.Done()
	return nil
}

// TestApp_ShutdownDuringStartWalkStartsNoWorker: a Shutdown that interrupts
// the start walk before the workers' turn starts none of them; they stay
// pending, write no lifecycle line, and App.Start reports the interruption.
func TestApp_ShutdownDuringStartWalkStartsNoWorker(t *testing.T) {
	capture := newLogCapture()
	app := newTestApp(t, credo.WithLogger(capture.logger()))
	gate := &raceGate{entered: make(chan struct{})}
	gated := &raceGatedWorker{}
	app.Provide[*raceGate](func() *raceGate { return gate })
	app.Provide[*raceGatedWorker](func(*raceGate) *raceGatedWorker { return gated })
	s := Use(app)
	s.ContinuousProvided[*raceGatedWorker]("gated")
	var byValueRuns atomic.Int32
	s.Scheduled("report", "@every 1h", raceCountingWorker(&byValueRuns), ScheduledConfig{RunOnStart: true})

	startErr := make(chan error, 1)
	go func() { startErr <- app.Start(t.Context()) }()
	lifecycleAwait(t, gate.entered, "the dependency's Start")

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
		if info.Status != StatusPending {
			t.Errorf("%s = %s, want pending (its component never started)", info.Name, info.Status)
		}
	}
	for _, msg := range []string{"worker started", "worker stopped"} {
		if lines := capture.withMessage(msg); len(lines) != 0 {
			t.Errorf("%q logged %d times, want none", msg, len(lines))
		}
	}
}

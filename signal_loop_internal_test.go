package credo

import (
	"context"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// signalLoopFixture drives lifecycleManager.signalLoop with plain channels in
// place of the OS signal plumbing, so the loop's behaviour is testable on
// every platform (there is no SIGHUP delivery on Windows).
type signalLoopFixture struct {
	app         *App
	termination chan struct{}
	reloads     chan os.Signal
	resetCalled atomic.Bool
	terminated  sync.Once
	result      chan error
}

// terminate closes the termination channel once.
func (f *signalLoopFixture) terminate() { f.terminated.Do(func() { close(f.termination) }) }

func newSignalLoopFixture(t *testing.T) *signalLoopFixture {
	t.Helper()
	app, err := New(WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	return &signalLoopFixture{
		app:         app,
		termination: make(chan struct{}),
		reloads:     make(chan os.Signal),
		result:      make(chan error, 1),
	}
}

// run starts the loop over ServeContext on an ephemeral listener and waits
// for the running state.
func (f *signalLoopFixture) run(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		f.result <- f.app.lifecycle.signalLoop(
			f.termination,
			func() { f.resetCalled.Store(true) },
			f.reloads,
			func(ctx context.Context) error { return f.app.ServeContext(ctx, l) },
		)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !f.app.IsRunning() {
		if time.Now().After(deadline) {
			t.Fatal("app did not reach running state")
		}
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() {
		f.terminate()
		select {
		case <-f.result:
		case <-time.After(5 * time.Second):
			t.Error("signal loop did not return")
		}
	})
}

func (f *signalLoopFixture) sendReload(t *testing.T) {
	t.Helper()
	select {
	case f.reloads <- syscall.SIGHUP:
	case <-time.After(5 * time.Second):
		t.Fatal("signal loop did not accept the reload signal")
	}
}

// TestSignalLoop_TerminationDuringReload: a termination signal that arrives
// while a signal-triggered reload is in flight is serviced immediately — the
// signal handler is reset (so a second signal force-kills), the drain starts,
// the reload's context is cancelled, and the loop returns once the drain has
// waited for the reload.
func TestSignalLoop_TerminationDuringReload(t *testing.T) {
	f := newSignalLoopFixture(t)
	entered := make(chan struct{})
	var sawCancel atomic.Bool
	f.app.OnReload(func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		sawCancel.Store(true)
		return nil
	})
	f.run(t)

	f.sendReload(t)
	<-entered

	f.terminate()
	select {
	case err := <-f.result:
		if err != nil {
			t.Fatalf("signal loop = %v, want nil", err)
		}
		f.result <- err // let Cleanup observe the result too
	case <-time.After(5 * time.Second):
		t.Fatal("termination was not serviced while the reload was in flight")
	}
	if !f.resetCalled.Load() {
		t.Error("signal handler was not reset on termination")
	}
	if !sawCancel.Load() {
		t.Error("the in-flight reload was not cancelled by the drain")
	}
	if got := f.app.State(); got != "stopped" {
		t.Errorf("State() = %q, want stopped", got)
	}
}

// TestSignalLoop_CoalescesReloadSignals: signals that land while a reload is
// in flight produce exactly one follow-up reload, and reloads never overlap.
func TestSignalLoop_CoalescesReloadSignals(t *testing.T) {
	f := newSignalLoopFixture(t)
	var runs, inFlight, maxInFlight atomic.Int32
	gate := make(chan struct{})
	f.app.OnReload(func(context.Context) error {
		n := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if n <= old || maxInFlight.CompareAndSwap(old, n) {
				break
			}
		}
		runs.Add(1)
		<-gate
		inFlight.Add(-1)
		return nil
	})
	f.run(t)

	f.sendReload(t) // starts reload #1, which blocks on gate
	for range 3 {
		f.sendReload(t) // accepted while #1 is in flight: coalesce to one follow-up
	}
	gate <- struct{}{} // release #1
	gate <- struct{}{} // release the single follow-up

	deadline := time.Now().Add(5 * time.Second)
	for runs.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("runs = %d, want 2", runs.Load())
		}
		time.Sleep(time.Millisecond)
	}
	// Give a spurious third reload a chance to show up before asserting.
	select {
	case gate <- struct{}{}:
		t.Fatalf("a third reload ran; runs = %d, want exactly one follow-up", runs.Load())
	case <-time.After(100 * time.Millisecond):
	}
	if got := maxInFlight.Load(); got != 1 {
		t.Errorf("reloads overlapped: max in flight = %d", got)
	}
}

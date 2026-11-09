package credo

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/credo-go/credo/config"
)

// TestHandleReloadSignal_ConfigurationThatCannotReload: over a RawConfig
// other than *config.Config, a reload signal is ignored with one Info line
// naming the signal and the configuration's type, and never reaches Reload.
func TestHandleReloadSignal_ConfigurationThatCannotReload(t *testing.T) {
	var logs bytes.Buffer
	app, err := New(WithRawConfig(stubServerRC{fields: map[string]any{}}),
		WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	app.lifecycle.handleReloadSignal(syscall.SIGHUP)
	got := logs.String()
	for _, part := range []string{
		"level=INFO",
		`msg="credo: reload signal ignored: the configuration cannot reload"`,
		"signal=hangup",
		"config_type=credo.stubServerRC",
	} {
		if !strings.Contains(got, part) {
			t.Errorf("log %q does not contain %q", got, part)
		}
	}
	if strings.Contains(got, "reload signal received") || strings.Contains(got, "credo: Reload:") {
		t.Errorf("the signal reached the reload path:\n%s", got)
	}
}

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

// newSignalLoopFixture builds an App over a one-key configuration file. A
// non-nil onReload runs in the OnChange callback of a live section over that
// key; the callback first rewrites the file with the key's next value, so
// every reload, including one queued behind a running callback, changes the
// section again.
func newSignalLoopFixture(t *testing.T, onReload func(ctx context.Context) error) *signalLoopFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(n int) {
		if err := os.WriteFile(path, []byte("n: "+strconv.Itoa(n)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(1)
	cfg, err := config.Load(config.WithFiles(path), config.WithoutProcessEnv(), config.WithoutDotenv())
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(WithRawConfig(cfg), WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	if onReload != nil {
		live, err := config.Section[int]("n").Live(cfg)
		if err != nil {
			t.Fatal(err)
		}
		live.OnChange(func(ctx context.Context, _, next int) error {
			write(next + 1)
			return onReload(ctx)
		})
		write(2)
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
	entered := make(chan struct{})
	var sawCancel atomic.Bool
	f := newSignalLoopFixture(t, func(ctx context.Context) error {
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
	var runs, inFlight, maxInFlight atomic.Int32
	gate := make(chan struct{})
	f := newSignalLoopFixture(t, func(context.Context) error {
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

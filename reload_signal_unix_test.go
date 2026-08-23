//go:build unix

package credo_test

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startRun runs the fixture through Run (the signal-aware entry point) and
// waits until it is running; Cleanup stops it through a programmatic Shutdown
// so no SIGINT is sent to the test process.
func (f *reloadFixture) startRun(t *testing.T) {
	t.Helper()
	go func() { f.errC <- f.app.Run() }()
	deadline := time.Now().Add(5 * time.Second)
	for !f.app.IsRunning() {
		if time.Now().After(deadline) {
			t.Fatal("app did not reach running state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.app.Shutdown(ctx)
		select {
		case <-f.errC:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after Shutdown")
		}
	})
}

// signalIgnore makes SIGHUP a no-op for the process until the test ends, so a
// signal nobody handles cannot kill the test binary.
func signalIgnore(t *testing.T) {
	t.Helper()
	signal.Ignore(syscall.SIGHUP)
	t.Cleanup(func() { signal.Reset(syscall.SIGHUP) })
}

func sendHUP(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}
}

// TestRun_SIGHUPTriggersReload is the end-to-end signal path: a SIGHUP under
// Run re-reads the config file and notifies the affected subscriber; a second
// SIGHUP after a failing hook still leaves the server running.
func TestRun_SIGHUPTriggersReload(t *testing.T) {
	f := newReloadFixture(t, "feature:\n  limit: 1\n")
	type feature struct {
		Limit int `credo:"limit"`
	}
	got := make(chan int, 4)
	f.app.OnConfigChange("feature", func(_ context.Context, c feature) error {
		got <- c.Limit
		return nil
	})
	fail := false
	f.app.OnReload(func(context.Context) error {
		if fail {
			return os.ErrInvalid
		}
		return nil
	})
	f.startRun(t)

	writeYAML(t, f.path, "feature:\n  limit: 2\n")
	sendHUP(t)
	select {
	case v := <-got:
		if v != 2 {
			t.Fatalf("subscriber got limit=%d, want 2", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not trigger the OnConfigChange subscriber")
	}

	// A failing reload is logged, not fatal: the server keeps running and the
	// next signal reloads again.
	fail = true
	writeYAML(t, f.path, "feature:\n  limit: 3\n")
	sendHUP(t)
	select {
	case v := <-got:
		if v != 3 {
			t.Fatalf("subscriber got limit=%d, want 3", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second SIGHUP did not trigger the subscriber")
	}
	if !f.app.IsRunning() {
		t.Fatal("a failing reload must not stop the server")
	}
	if logs := f.logs.String(); !strings.Contains(logs, "credo: reload signal received") {
		t.Errorf("missing signal log line:\n%s", logs)
	}
}

// TestRunContext_IgnoresSIGHUP locks in that only Run installs the reload
// handler. Under RunContext the process-level default for SIGHUP would
// terminate the test binary, so the signal is ignored for the test's duration
// and the assertion is that no reload happened.
func TestRunContext_IgnoresSIGHUP(t *testing.T) {
	signalIgnore(t)
	f := newReloadFixture(t, "feature:\n  limit: 1\n")
	type feature struct {
		Limit int `credo:"limit"`
	}
	got := make(chan int, 1)
	f.app.OnConfigChange("feature", func(_ context.Context, c feature) error {
		got <- c.Limit
		return nil
	})
	f.start(t)

	writeYAML(t, f.path, "feature:\n  limit: 2\n")
	sendHUP(t)
	select {
	case v := <-got:
		t.Fatalf("RunContext must not handle SIGHUP, but subscriber got limit=%d", v)
	case <-time.After(300 * time.Millisecond):
	}
}

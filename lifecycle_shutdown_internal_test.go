package credo

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestInitiateShutdown_NonRunningStates locks the CAS guard directly: from any
// state other than running, initiateShutdown refuses with
// errShutdownNotRunning and leaves the state untouched.
func TestInitiateShutdown_NonRunningStates(t *testing.T) {
	states := []appState{stateBuilding, stateStarting, stateStopping, stateStopped}
	for _, s := range states {
		t.Run(s.String(), func(t *testing.T) {
			app, err := New()
			if err != nil {
				t.Fatal(err)
			}
			app.lifecycle.state.Store(uint32(s))

			if got := app.lifecycle.initiateShutdown(t.Context()); !errors.Is(got, errShutdownNotRunning) {
				t.Fatalf("initiateShutdown from %s = %v, want errShutdownNotRunning", s, got)
			}
			if got := appState(app.lifecycle.state.Load()); got != s {
				t.Errorf("state after refused shutdown = %s, want unchanged %s", got, s)
			}
		})
	}
}

// TestInitiateShutdown_FromRunning_Drains verifies the winning path end to
// end on an unstarted app: the CAS succeeds, the shared drain sequence runs
// (nil servers and empty hook sets are all tolerated), and the state lands on
// stopped.
func TestInitiateShutdown_FromRunning_Drains(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	var shutdownHookCalls int
	app.OnShutdown(func(context.Context) error { shutdownHookCalls++; return nil })
	app.lifecycle.state.Store(uint32(stateRunning))

	if err := app.lifecycle.initiateShutdown(t.Context()); err != nil {
		t.Fatalf("initiateShutdown from running = %v, want nil", err)
	}
	if got := appState(app.lifecycle.state.Load()); got != stateStopped {
		t.Errorf("state after drain = %s, want %s", got, stateStopped)
	}
	if shutdownHookCalls != 1 {
		t.Errorf("OnShutdown hook ran %d times, want 1", shutdownHookCalls)
	}
}

// TestInitiateShutdown_ConcurrentCallers_SingleDrain races several callers at
// the CAS: exactly one must win and run the drain sequence exactly once; every
// loser must receive errShutdownNotRunning.
func TestInitiateShutdown_ConcurrentCallers_SingleDrain(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	drainRuns := 0
	app.OnShutdown(func(context.Context) error {
		mu.Lock()
		drainRuns++
		mu.Unlock()
		return nil
	})
	app.lifecycle.state.Store(uint32(stateRunning))

	const callers = 8
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() { results <- app.lifecycle.initiateShutdown(t.Context()) })
	}
	wg.Wait()
	close(results)

	winners, losers := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, errShutdownNotRunning):
			losers++
		default:
			t.Fatalf("unexpected initiateShutdown error: %v", err)
		}
	}
	if winners != 1 || losers != callers-1 {
		t.Errorf("winners = %d, losers = %d, want 1 and %d", winners, losers, callers-1)
	}
	if drainRuns != 1 {
		t.Errorf("drain ran %d times, want exactly 1", drainRuns)
	}
	if got := appState(app.lifecycle.state.Load()); got != stateStopped {
		t.Errorf("state after concurrent shutdown = %s, want %s", got, stateStopped)
	}
}

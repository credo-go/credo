package credo_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// The fixtures of the panic-error tests: a type whose constructor panics, a
// type that depends on it, and an interface both bind for ResolveAll.
type (
	panicLeaf      struct{}
	panicDependent struct{ leaf *panicLeaf }
	panicNamer     interface{ PanicName() string }
)

func (*panicLeaf) PanicName() string { return "leaf" }

// panicApp binds panicLeaf with a constructor that panics with value, and
// panicDependent, whose constructor takes the leaf.
func panicApp(t *testing.T, value any) *credo.App {
	t.Helper()
	app := mustNew(t)
	app.Provide(func() *panicLeaf { panic(value) })
	app.Provide(func(l *panicLeaf) *panicDependent { return &panicDependent{leaf: l} })
	return app
}

// TestPanicError_FromResolve: a constructor panic reaches the caller of
// Resolve as the root's *PanicError — the error itself for the type that
// panicked, a wrapped one for a type that depends on it — and the same value
// on every call.
func TestPanicError_FromResolve(t *testing.T) {
	app := panicApp(t, "boom")

	_, err := app.Resolve[*panicLeaf]()
	pe, ok := err.(*credo.PanicError) //nolint:errorlint // The error itself is the PanicError, not a wrapper.
	if !ok {
		t.Fatalf("Resolve error is a %T (%v), want *credo.PanicError itself", err, err)
	}
	if pe.Type != reflect.TypeFor[*panicLeaf]() || pe.Phase != credo.PanicConstruction || pe.Value != "boom" || pe.Stack == "" {
		t.Errorf("PanicError = {Type: %v, Phase: %q, Value: %v, Stack: %d bytes}, want *panicLeaf, construction, boom and a stack",
			pe.Type, pe.Phase, pe.Value, len(pe.Stack))
	}
	if got, want := pe.Error(), "credo: panic during construction of *credo_test.panicLeaf: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if found, ok := errors.AsType[*credo.PanicError](err); !ok || found != pe {
		t.Errorf("errors.AsType[*credo.PanicError] = %v, %v; want the error itself", found, ok)
	}

	// Every resolver of one failure holds one value.
	_, again := app.Resolve[*panicLeaf]()
	if again != error(pe) { //nolint:errorlint // Identity is what is asserted.
		t.Errorf("a later Resolve returned %p, want the same *PanicError %p", again, pe)
	}

	// A dependent fails with an error that wraps the same PanicError.
	_, depErr := app.Resolve[*panicDependent]()
	if depErr == nil {
		t.Fatal("Resolve of a dependent of a panicked constructor returned nil")
	}
	if found, ok := errors.AsType[*credo.PanicError](depErr); !ok || found != pe {
		t.Errorf("errors.AsType through the dependent's error = %p, %v; want the leaf's %p", found, ok, pe)
	}
	if !errors.Is(depErr, pe) {
		t.Errorf("errors.Is(dependent's error, the PanicError) = false for %v", depErr)
	}
	if !strings.Contains(depErr.Error(), "panicDependent") || !strings.Contains(depErr.Error(), pe.Error()) {
		t.Errorf("dependent's error = %q, want it to name the dependent and carry the panic's text", depErr)
	}
}

// TestPanicError_MustResolvePanicsWithIt: the value MustResolve and
// MustResolveAll panic with is the error Resolve returns, so a recover finds
// the *PanicError by a type assertion.
func TestPanicError_MustResolvePanicsWithIt(t *testing.T) {
	recovered := func(fn func()) (v any) {
		defer func() { v = recover() }()
		fn()
		return nil
	}

	t.Run("MustResolve", func(t *testing.T) {
		app := panicApp(t, "boom")
		v := recovered(func() { app.MustResolve[*panicLeaf]() })
		pe, ok := v.(*credo.PanicError)
		if !ok || pe.Phase != credo.PanicConstruction || pe.Value != "boom" {
			t.Fatalf("MustResolve panicked with %T (%v), want a construction *credo.PanicError carrying boom", v, v)
		}
		if _, err := app.Resolve[*panicLeaf](); err != error(pe) { //nolint:errorlint // Identity is what is asserted.
			t.Errorf("Resolve returned %v, want the value MustResolve panicked with", err)
		}
	})

	t.Run("MustResolve of a dependent", func(t *testing.T) {
		app := panicApp(t, "boom")
		v := recovered(func() { app.MustResolve[*panicDependent]() })
		err, ok := v.(error)
		if !ok {
			t.Fatalf("MustResolve panicked with %T, want an error", v)
		}
		if pe, ok := errors.AsType[*credo.PanicError](err); !ok || pe.Type != reflect.TypeFor[*panicLeaf]() {
			t.Errorf("errors.AsType through the panic value = %v, %v; want the leaf's PanicError", pe, ok)
		}
	})

	t.Run("ResolveAll and MustResolveAll", func(t *testing.T) {
		app := panicApp(t, "boom")
		app.BindMany[panicNamer, *panicLeaf]()
		_, err := app.ResolveAll[panicNamer]()
		pe, ok := errors.AsType[*credo.PanicError](err)
		if !ok || pe.Phase != credo.PanicConstruction {
			t.Fatalf("ResolveAll = %v, want an error carrying a construction *credo.PanicError", err)
		}
		v := recovered(func() { app.MustResolveAll[panicNamer]() })
		verr, ok := v.(error)
		if !ok || !errors.Is(verr, pe) {
			t.Errorf("MustResolveAll panicked with %T (%v), want an error carrying the same PanicError", v, v)
		}
	})
}

// TestPanicError_ErrorValueIsReachable: a panic value that is an error stays
// reachable through errors.Is and errors.As, from the PanicError itself, from
// a dependent's error and through a LifecycleError.
func TestPanicError_ErrorValueIsReachable(t *testing.T) {
	cause := fmt.Errorf("open store: %w", context.DeadlineExceeded)

	app := panicApp(t, cause)
	_, err := app.Resolve[*panicLeaf]()
	_, depErr := app.Resolve[*panicDependent]()
	for name, got := range map[string]error{"Resolve": err, "Resolve of a dependent": depErr} {
		if !errors.Is(got, cause) || !errors.Is(got, context.DeadlineExceeded) {
			t.Errorf("%s: errors.Is(err, the panic value) = false for %v", name, got)
		}
		if pe, ok := errors.AsType[*credo.PanicError](got); !ok || pe.Value != any(cause) || !errors.Is(pe.Unwrap(), cause) {
			t.Errorf("%s: PanicError = %v (found %v), want Value and Unwrap to be the panic's error", name, pe, ok)
		}
	}

	// Through a LifecycleError: the start walk builds the component.
	started := panicApp(t, cause)
	started.Provide(func(*panicDependent) *panicStarter { return &panicStarter{} })
	startErr := started.Start(t.Context())
	if _, ok := errors.AsType[*credo.LifecycleError](startErr); !ok {
		t.Fatalf("App.Start() = %v, want a *credo.LifecycleError", startErr)
	}
	if !errors.Is(startErr, cause) {
		t.Errorf("errors.Is(lifecycle error, the panic value) = false for %v", startErr)
	}
	if pe, ok := errors.AsType[*credo.PanicError](startErr); !ok || pe.Phase != credo.PanicConstruction {
		t.Errorf("errors.AsType through the LifecycleError = %v, %v; want the construction PanicError", pe, ok)
	}
}

// panicStarter is a component, so the start walk builds it and what it
// depends on.
type panicStarter struct{}

func (*panicStarter) Start(context.Context) error    { return nil }
func (*panicStarter) Shutdown(context.Context) error { return nil }

// TestPanicError_ThroughLifecycleError: each phase a component can panic in
// is reported in a LifecycleEntry whose Err is the root's *PanicError, found
// from the entry and from the LifecycleError.
func TestPanicError_ThroughLifecycleError(t *testing.T) {
	tests := []struct {
		name  string
		entry string
		phase credo.PanicPhase
		// run returns the lifecycle error of the phase under test.
		run func(t *testing.T) error
		// exact reports that the entry's error is the PanicError itself,
		// not an error that wraps it.
		exact bool
	}{
		{
			name: "a constructor in the start walk", entry: "*credo_test.panicStarter", phase: credo.PanicConstruction, exact: true,
			run: func(t *testing.T) error {
				app := mustNew(t)
				app.Provide(func() *panicStarter { panic("boom") })
				return app.Start(t.Context())
			},
		},
		{
			name: "a dependency's constructor in the start walk", entry: "*credo_test.panicStarter", phase: credo.PanicConstruction,
			run: func(t *testing.T) error {
				app := panicApp(t, "boom")
				app.Provide(func(*panicDependent) *panicStarter { return &panicStarter{} })
				return app.Start(t.Context())
			},
		},
		{
			name: "Start", entry: "*credo_test.partA", phase: credo.PanicStart, exact: true,
			run: func(t *testing.T) error {
				app := mustNew(t)
				app.ProvideValue[*partA](&partA{&part{name: "a", j: &journal{}, startPanic: true}})
				return callRecovering(t, "App.Start", func() error { return app.Start(t.Context()) })
			},
		},
		{
			name: "Shutdown", entry: "*credo_test.partB", phase: credo.PanicShutdown, exact: true,
			run: func(t *testing.T) error {
				j := &journal{}
				app := mustNew(t)
				app.ProvideValue[*partB](&partB{&part{name: "b", j: j, shutdownPanic: true}})
				startComponents(t, app)
				return callRecovering(t, "Shutdown", func() error { return shutdownApp(t, app, 2*time.Second) })
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(t)
			e := lifecycleEntry(t, err, tt.entry)
			if e.Outcome != credo.OutcomePanicked {
				t.Errorf("entry outcome = %q, want %q", e.Outcome, credo.OutcomePanicked)
			}
			pe, ok := errors.AsType[*credo.PanicError](e.Err)
			if !ok || pe.Phase != tt.phase || pe.Stack == "" {
				t.Fatalf("entry error = %T (%v), want one carrying a %q *credo.PanicError with a stack", e.Err, e.Err, tt.phase)
			}
			// The text names the framework, not the container: a Start or a
			// Shutdown panic is not dependency injection. An entry whose
			// error wraps the panic carries the same text.
			if want := "credo: panic during " + string(tt.phase) + " of "; !strings.HasPrefix(pe.Error(), want) || !strings.Contains(e.Err.Error(), pe.Error()) {
				t.Errorf("PanicError text = %q in the entry error %q, want it to begin with %q", pe, e.Err, want)
			}
			if _, isExact := e.Err.(*credo.PanicError); isExact != tt.exact { //nolint:errorlint // Whether the entry's error is the PanicError itself is what is asserted.
				t.Errorf("entry error is a %T, want the *credo.PanicError itself: %v", e.Err, tt.exact)
			}
			if found, ok := errors.AsType[*credo.PanicError](err); !ok || found != pe {
				t.Errorf("errors.AsType through the LifecycleError = %p, %v; want the entry's %p", found, ok, pe)
			}
		})
	}
}

// TestPanicPhase_Values pins the four phases and their text, which the
// PanicError's message prints.
func TestPanicPhase_Values(t *testing.T) {
	tests := []struct {
		phase credo.PanicPhase
		want  string
	}{
		{credo.PanicConstruction, "construction"},
		{credo.PanicStart, "start"},
		{credo.PanicShutdown, "shutdown"},
		{credo.PanicLateCleanup, "late cleanup"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if string(tt.phase) != tt.want {
				t.Errorf("phase = %q, want %q", string(tt.phase), tt.want)
			}
			pe := &credo.PanicError{Type: reflect.TypeFor[*panicLeaf](), Phase: tt.phase, Value: "boom"}
			if want := "credo: panic during " + tt.want + " of *credo_test.panicLeaf: boom"; pe.Error() != want {
				t.Errorf("Error() = %q, want %q", pe.Error(), want)
			}
		})
	}
}

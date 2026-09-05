package di_test

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/credo-go/credo/internal/di"
)

// panicky is a service whose constructor panics.
type panicky struct{}

// dependsOnPanicky is built from panicky, so its construction fails through
// its parameter.
type dependsOnPanicky struct{ P *panicky }

// TestResolve_ConstructorPanic_FailFastRegression pins the public behavior
// that predates terminal completion and must survive it: MustResolve of a
// panicking constructor still panics, and no later caller ever receives a nil
// instance with a nil error.
func TestResolve_ConstructorPanic_FailFastRegression(t *testing.T) {
	c := di.New()
	c.MustProvide[*panicky](func() *panicky { panic("boom") })
	seal(t, c)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("MustResolve should panic when the constructor panics")
			}
		}()
		c.MustResolve[*panicky]()
	}()

	got, err := c.Resolve[*panicky]()
	if err == nil {
		t.Fatalf("second Resolve = (%v, nil), want an error (never a silent nil instance)", got)
	}
	if got != nil {
		t.Fatalf("second Resolve returned an instance %v with error %v", got, err)
	}
}

func TestResolve_ConstructorPanic_TerminalTypedError(t *testing.T) {
	var calls atomic.Int32
	c := di.New()
	c.MustProvide[*panicky](func() *panicky {
		calls.Add(1)
		panic("boom")
	})
	seal(t, c)

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := range goroutines {
		wg.Go(func() {
			_, errs[i] = c.Resolve[*panicky]()
		})
	}
	wg.Wait()

	// One construction; every waiter receives the same recorded failure.
	if got := calls.Load(); got != 1 {
		t.Fatalf("constructor ran %d times, want 1 (no retry)", got)
	}
	for i, err := range errs {
		pe, ok := errors.AsType[*di.PanicError](err)
		if !ok {
			t.Fatalf("goroutine %d: error %v is not a *di.PanicError", i, err)
		}
		if pe.Phase != di.PhaseConstruction {
			t.Errorf("Phase = %v, want construction", pe.Phase)
		}
		if pe.Value != "boom" {
			t.Errorf("Value = %v, want the original panic value", pe.Value)
		}
		if pe.Type == nil || !strings.Contains(pe.Type.String(), "panicky") {
			t.Errorf("Type = %v, want *panicky", pe.Type)
		}
		if pe.Stack == "" {
			t.Error("Stack should be captured on the panicking goroutine")
		}
		if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "construction") {
			t.Errorf("Error() = %q, want phase and value", err)
		}
	}

	// A later caller gets the same terminal failure without a retry.
	if _, err := c.Resolve[*panicky](); !errors.Is(err, errs[0]) {
		if _, ok := errors.AsType[*di.PanicError](err); !ok {
			t.Fatalf("later Resolve = %v, want the recorded PanicError", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("constructor ran %d times after later Resolve, want 1", got)
	}

	// MustResolve panics with the error wrapper as its payload.
	func() {
		defer func() {
			r := recover()
			if _, ok := r.(*di.PanicError); !ok {
				t.Fatalf("MustResolve panic payload = %T (%v), want *di.PanicError", r, r)
			}
		}()
		c.MustResolve[*panicky]()
	}()
}

func TestResolve_ConstructorPanic_ErrorValueUnwraps(t *testing.T) {
	cause := errors.New("db offline")
	c := di.New()
	c.MustProvide[*panicky](func() *panicky { panic(cause) })
	seal(t, c)

	_, err := c.Resolve[*panicky]()
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is(err, cause) = false for %v", err)
	}
	pe, ok := errors.AsType[*di.PanicError](err)
	if !ok || pe.Value != cause {
		t.Fatalf("PanicError.Value = %v, want the original error value", pe)
	}
}

func TestResolve_ConstructorPanic_DependentFailsThroughParameter(t *testing.T) {
	c := di.New()
	c.MustProvide[*panicky](func() *panicky { panic("boom") })
	c.MustProvide[*dependsOnPanicky](func(p *panicky) *dependsOnPanicky { return &dependsOnPanicky{P: p} })
	seal(t, c)

	_, err := c.Resolve[*dependsOnPanicky]()
	if err == nil {
		t.Fatal("dependent should fail when its dependency panicked")
	}
	if _, ok := errors.AsType[*di.PanicError](err); !ok {
		t.Fatalf("dependent error %v should wrap the dependency's PanicError", err)
	}
	if !strings.Contains(err.Error(), "dependsOnPanicky") {
		t.Errorf("dependent error should name the dependent type: %v", err)
	}
}

func TestShutdown_ConstructionFailuresAreDiagnosticsOnly(t *testing.T) {
	c := di.New()
	c.MustProvide[*panicky](func() *panicky { panic("boom") })
	c.MustProvide[*ServiceWithError](NewServiceFailing)
	seal(t, c)
	_, _ = c.Resolve[*panicky]()
	_, _ = c.Resolve[*ServiceWithError]()

	// Failed constructors have nothing to clean and never make teardown fail.
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown = %v, want nil for construction-failed registrations", err)
	}
}

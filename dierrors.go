package credo

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/credo-go/credo/internal/di"
)

// ErrDIClosed is the sentinel wrapped by every [App.Resolve] rejected because
// DI teardown has begun or completed. Compare with errors.Is. It applies from
// the moment the container enters its closing phase — when [App.Shutdown]
// begins the internal tier, after the HTTP drain and the ingress tier — and
// takes precedence over a failed Finalize.
var ErrDIClosed = di.ErrClosed

// PanicError is the recovered panic of a constructor, or of a component's
// Start, Shutdown or Close. Obtain it with errors.AsType[*credo.PanicError]:
// from the error [App.Resolve] returns, from the value [App.MustResolve]
// panics with, and through a [*LifecycleError], whose entry for a component
// that panicked carries it.
//
// A constructor panic is terminal: the constructor is never retried, and the
// first, concurrent and later resolvers of that type all receive the same
// *PanicError. A type that depends on the one that panicked fails with an
// error that wraps it, so errors.AsType finds the PanicError there too.
//
// When Value is an error it is also reachable through errors.Is and
// errors.As.
type PanicError struct {
	// Type is the registered type whose constructor, Start, Shutdown or Close
	// panicked.
	Type reflect.Type

	// Phase says where the panic was recovered.
	Phase PanicPhase

	// Value is the original recovered panic value.
	Value any

	// Stack is the stack trace captured on the panicking goroutine.
	Stack string
}

// Error describes the phase, the type and the panic value:
// "credo: panic during start of *app.Cache: boom". The text names the
// framework and not the container, since a panic in Start, Shutdown or Close
// is not dependency injection.
func (e *PanicError) Error() string {
	return fmt.Sprintf("credo: panic during %s of %s: %v", e.Phase, e.Type, e.Value)
}

// Unwrap returns the panic value when it is an error, else nil.
func (e *PanicError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// PanicPhase identifies where a [PanicError] was recovered. It is finer than
// [LifecyclePhase], which says only whether an entry belongs to the start or
// to the shutdown: a constructor that panics during the start walk is a
// PanicConstruction in a [PhaseStart] entry.
type PanicPhase string

const (
	// PanicConstruction: a constructor panicked while building a singleton.
	PanicConstruction PanicPhase = "construction"
	// PanicStart: a component's Start panicked during the start walk.
	PanicStart PanicPhase = "start"
	// PanicShutdown: a component's Shutdown or Close panicked during the
	// drain.
	PanicShutdown PanicPhase = "shutdown"
	// PanicLateCleanup: a teardown panicked during the best-effort cleanup
	// of an instance constructed after the drain deadline.
	PanicLateCleanup PanicPhase = "late cleanup"
)

// panicPhaseOf returns the root phase of a container phase.
func panicPhaseOf(p di.PanicPhase) PanicPhase {
	switch p {
	case di.PhaseConstruction:
		return PanicConstruction
	case di.PhaseStart:
		return PanicStart
	case di.PhaseShutdown:
		return PanicShutdown
	case di.PhaseLateCleanup:
		return PanicLateCleanup
	default:
		return PanicPhase(p.String())
	}
}

// publicDIError converts an error of the container into the one the root
// package shows. It runs at every boundary where a container error reaches
// the caller — [App.Resolve] and [App.ResolveAll], the values
// [App.MustResolve] and [App.MustResolveAll] panic with, and each
// [LifecycleEntry] of a [LifecycleError] — so the container's own panic type
// never does.
//
//   - The container's panic error itself becomes its *PanicError, the same
//     pointer on every call, so every caller of one failure holds one value.
//   - An error that wraps the container's panic error — the failure of a type
//     whose dependency panicked, or of a ResolveAll binding — keeps its text
//     and its chain and gains the *PanicError for errors.As and errors.Is.
//   - Any other error, and nil, pass through.
func publicDIError(err error) error {
	if err == nil {
		return nil
	}
	if pe, ok := err.(*di.PanicError); ok { //nolint:errorlint // The node itself is replaced; a wrapped one is handled below.
		return publicPanicError(pe)
	}
	if pe, ok := errors.AsType[*di.PanicError](err); ok {
		return &dependencyPanicError{err: err, panic: publicPanicError(pe)}
	}
	return err
}

// publicPanicError returns the root form of a container panic error, built
// on the first call and returned as the same pointer afterwards.
func publicPanicError(pe *di.PanicError) *PanicError {
	public := pe.Surface(func(pe *di.PanicError) error {
		return &PanicError{Type: pe.Type, Phase: panicPhaseOf(pe.Phase), Value: pe.Value, Stack: pe.Stack}
	})
	return public.(*PanicError) //nolint:errorlint // Surface returns what the function above built.
}

// dependencyPanicError is a container error that wraps a panic — the failure
// of a type whose dependency's constructor panicked — in the form the root
// package returns: the container's text and chain, with the root *PanicError
// in place of the container's own for errors.As and errors.Is.
type dependencyPanicError struct {
	err   error
	panic *PanicError
}

func (e *dependencyPanicError) Error() string { return e.err.Error() }

// Unwrap returns the container's error, which keeps a panic value that is an
// error reachable through errors.Is.
func (e *dependencyPanicError) Unwrap() error { return e.err }

// As sets a **PanicError target to the root form of the wrapped panic.
func (e *dependencyPanicError) As(target any) bool {
	if t, ok := target.(**PanicError); ok {
		*t = e.panic
		return true
	}
	return false
}

// Is reports whether target is the root form of the wrapped panic.
func (e *dependencyPanicError) Is(target error) bool {
	return target == error(e.panic)
}

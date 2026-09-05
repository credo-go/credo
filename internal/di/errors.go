package di

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// ErrClosed is the sentinel wrapped by every resolution rejected because the
// container has begun or completed shutdown. It applies from the moment
// Shutdown starts (the closing phase) and is checked before any Seal error, so
// teardown rejection wins even after a failed Finalize.
var ErrClosed = errors.New("di: container is closed")

// PanicPhase identifies where a recovered panic happened.
type PanicPhase uint8

const (
	// PhaseConstruction: a constructor panicked while building a singleton.
	PhaseConstruction PanicPhase = iota + 1
	// PhaseShutdown: a Shutdowner panicked during the dependency-ordered
	// shutdown pass.
	PhaseShutdown
	// PhaseLateCleanup: a Shutdowner panicked during the best-effort cleanup of
	// an instance constructed after the shutdown context ended.
	PhaseLateCleanup
)

// String returns the phase name used in error text and logs.
func (p PanicPhase) String() string {
	switch p {
	case PhaseConstruction:
		return "construction"
	case PhaseShutdown:
		return "shutdown"
	case PhaseLateCleanup:
		return "late cleanup"
	default:
		return "unknown"
	}
}

// PanicError is the recovered panic of a constructor or Shutdowner. The
// original panic value is preserved as Value; when it is an error it is also
// exposed through Unwrap so errors.Is/As keep working. Stack is the goroutine
// stack captured where the panic occurred.
type PanicError struct {
	// Type is the registered type whose constructor or Shutdowner panicked.
	Type reflect.Type
	// Phase says whether the panic occurred during construction, the shutdown
	// pass, or late cleanup.
	Phase PanicPhase
	// Value is the original recovered panic value.
	Value any
	// Stack is the stack trace captured on the panicking goroutine.
	Stack string
}

// Error describes the phase, type and panic value.
func (e *PanicError) Error() string {
	return fmt.Sprintf("di: panic during %s of %s: %v", e.Phase, e.Type, e.Value)
}

// Unwrap returns the panic value when it is an error, else nil.
func (e *PanicError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// ShutdownState is the per-registration outcome recorded in a ShutdownError.
type ShutdownState string

const (
	// ShutdownRetired: the instance has no Shutdown method; it was released
	// once its dependents retired, without calling user code.
	ShutdownRetired ShutdownState = "retired"
	// ShutdownSucceeded: Shutdown returned nil.
	ShutdownSucceeded ShutdownState = "succeeded"
	// ShutdownFailed: Shutdown returned an error (in Err).
	ShutdownFailed ShutdownState = "failed"
	// ShutdownPanicked: Shutdown panicked; Err is a *PanicError.
	ShutdownPanicked ShutdownState = "panicked"
	// ShutdownRunning: Shutdown was started but had not returned when the
	// shutdown context ended. Its dependencies stayed blocked.
	ShutdownRunning ShutdownState = "running"
	// ShutdownConstructing: the constructor was still running at the boundary.
	ShutdownConstructing ShutdownState = "constructing"
	// ShutdownBlocked: live dependents (Blockers) never retired.
	ShutdownBlocked ShutdownState = "blocked"
	// ShutdownUnattempted: the instance became ready but the context ended
	// before its turn.
	ShutdownUnattempted ShutdownState = "unattempted"
	// ShutdownNeverConstructed: the constructor never ran; nothing to clean.
	ShutdownNeverConstructed ShutdownState = "never_constructed"
	// ShutdownConstructionFailed: the constructor failed earlier (Err); nothing
	// to clean.
	ShutdownConstructionFailed ShutdownState = "construction_failed"
	// ShutdownLateCleanup: the instance was constructed after the shutdown
	// context ended and received the separate best-effort cleanup attempt; its
	// outcome is logged, not reported.
	ShutdownLateCleanup ShutdownState = "late_cleanup"
)

// ShutdownEntry is one registration's teardown record.
type ShutdownEntry struct {
	// Type is the registered (canonical) type.
	Type reflect.Type
	// State is the outcome.
	State ShutdownState
	// Err is the failure for ShutdownFailed, ShutdownPanicked and
	// ShutdownConstructionFailed; nil otherwise.
	Err error
	// Blockers lists the live dependents that kept a ShutdownBlocked entry
	// from retiring.
	Blockers []reflect.Type
	// Duration is the completed attempt's duration for terminal states, and the
	// elapsed time at the reporting boundary for ShutdownRunning and
	// ShutdownConstructing.
	Duration time.Duration
}

func (e ShutdownEntry) String() string {
	var b strings.Builder
	b.WriteString(e.Type.String())
	b.WriteString(" ")
	b.WriteString(string(e.State))
	if e.Duration > 0 {
		fmt.Fprintf(&b, " (%s)", e.Duration.Round(time.Millisecond))
	}
	if len(e.Blockers) > 0 {
		b.WriteString(" by ")
		for i, t := range e.Blockers {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(t.String())
		}
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// ShutdownError reports a failed or incomplete container shutdown. It is an
// immutable snapshot taken when Shutdown returned: entries are in registration
// order and later completions (a Shutdowner returning after the boundary, a
// late-cleanup result) are logged, never written back.
//
// A fully successful shutdown returns nil, not an empty report.
type ShutdownError struct {
	// Entries holds one record per registration, in registration order,
	// including successful and diagnostic entries. Callers must not modify it.
	Entries []ShutdownEntry
	// Cause is the shutdown context error when the context ended before the
	// pass completed; nil when every failure is a callback result.
	Cause error

	errs []error
}

// Error summarizes the non-successful entries.
func (e *ShutdownError) Error() string {
	var b strings.Builder
	b.WriteString("di: shutdown incomplete")
	if e.Cause != nil {
		fmt.Fprintf(&b, " (%v)", e.Cause)
	}
	sep := ": "
	for _, entry := range e.Entries {
		switch entry.State {
		case ShutdownSucceeded, ShutdownRetired, ShutdownNeverConstructed, ShutdownConstructionFailed:
			continue
		}
		b.WriteString(sep)
		b.WriteString(entry.String())
		sep = "; "
	}
	return b.String()
}

// Unwrap exposes the underlying callback failures and the context cause so
// errors.Is and errors.As traverse the report.
func (e *ShutdownError) Unwrap() []error {
	return e.errs
}

// Incomplete reports whether any entry did not reach a terminal cleanup state:
// running, constructing, blocked, unattempted or late.
func (e *ShutdownError) Incomplete() bool {
	for _, entry := range e.Entries {
		switch entry.State {
		case ShutdownRunning, ShutdownConstructing, ShutdownBlocked, ShutdownUnattempted, ShutdownLateCleanup:
			return true
		}
	}
	return false
}

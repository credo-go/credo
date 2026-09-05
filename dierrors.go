package credo

import "github.com/credo-go/credo/internal/di"

// ErrDIClosed is the sentinel wrapped by every [App.Resolve] rejected because
// DI teardown has begun or completed. Compare with errors.Is. It applies from
// the moment the container enters its closing phase (the DI teardown stage of
// [App.Shutdown], after the HTTP and OnDrain phases) and takes precedence over
// a failed Finalize.
var ErrDIClosed = di.ErrClosed

// DIPanicError is the recovered panic of a DI constructor or of a
// [Shutdowner] invoked during teardown. Its fields are the failing registered
// Type, the Phase (construction, normal shutdown, or late cleanup), the
// original panic Value and the Stack captured on the panicking goroutine.
// When Value is an error it is also reachable through errors.Is/As. Obtain it
// with errors.AsType[*credo.DIPanicError], including through a
// [DIShutdownError] or a joined App shutdown error.
//
// A constructor panic is terminal: the constructor is never retried, and the
// first, concurrent and later resolvers of that type all receive the same
// DIPanicError. [App.MustResolve] panics with the error as its value.
type DIPanicError = di.PanicError

// DIPanicPhase identifies where a [DIPanicError] was recovered.
type DIPanicPhase = di.PanicPhase

const (
	// DIPanicConstruction: a constructor panicked while building a singleton.
	DIPanicConstruction = di.PhaseConstruction
	// DIPanicShutdown: a Shutdowner panicked during the dependency-ordered
	// shutdown pass.
	DIPanicShutdown = di.PhaseShutdown
	// DIPanicLateCleanup: a Shutdowner panicked during the best-effort cleanup
	// of an instance constructed after the shutdown context ended.
	DIPanicLateCleanup = di.PhaseLateCleanup
)

// DIShutdownError reports a failed or incomplete DI teardown. It is returned
// by [App.Shutdown] (joined with other teardown errors) whenever a singleton's
// Shutdown failed, panicked, or could not be attempted or completed within the
// shutdown budget. Obtain it with errors.AsType[*credo.DIShutdownError].
//
// Entries holds one [DIShutdownEntry] per registration in registration order,
// including the successful and diagnostic ones, and Cause is the shutdown
// context error when the budget ran out. The report is an immutable snapshot:
// a Shutdown call returning after the boundary, or a late-cleanup result, is
// logged and never written back. Unwrap exposes the callback failures and the
// context cause so errors.Is and errors.As traverse the report.
type DIShutdownError = di.ShutdownError

// DIShutdownEntry is one registration's record inside a [DIShutdownError]:
// its Type, State, the Err of a failed or panicked attempt, the Blockers that
// kept a blocked entry from retiring, and the Duration of a completed attempt
// or the elapsed time of an incomplete one.
type DIShutdownEntry = di.ShutdownEntry

// DIShutdownState is the per-registration outcome recorded in a
// [DIShutdownEntry].
type DIShutdownState = di.ShutdownState

const (
	// DIShutdownRetired: no Shutdown method; released after its dependents.
	DIShutdownRetired = di.ShutdownRetired
	// DIShutdownSucceeded: Shutdown returned nil.
	DIShutdownSucceeded = di.ShutdownSucceeded
	// DIShutdownFailed: Shutdown returned an error.
	DIShutdownFailed = di.ShutdownFailed
	// DIShutdownPanicked: Shutdown panicked; Err is a *DIPanicError.
	DIShutdownPanicked = di.ShutdownPanicked
	// DIShutdownRunning: Shutdown was started but had not returned when the
	// shutdown context ended; its dependencies stayed blocked.
	DIShutdownRunning = di.ShutdownRunning
	// DIShutdownConstructing: the constructor was still running at the
	// boundary.
	DIShutdownConstructing = di.ShutdownConstructing
	// DIShutdownBlocked: live dependents never retired.
	DIShutdownBlocked = di.ShutdownBlocked
	// DIShutdownUnattempted: ready, but the context ended before its turn.
	DIShutdownUnattempted = di.ShutdownUnattempted
	// DIShutdownNeverConstructed: the constructor never ran.
	DIShutdownNeverConstructed = di.ShutdownNeverConstructed
	// DIShutdownConstructionFailed: the constructor failed earlier.
	DIShutdownConstructionFailed = di.ShutdownConstructionFailed
	// DIShutdownLateCleanup: constructed after the shutdown context ended and
	// handed to the separate best-effort cleanup attempt.
	DIShutdownLateCleanup = di.ShutdownLateCleanup
)

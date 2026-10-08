package credo

import "github.com/credo-go/credo/internal/di"

// ErrDIClosed is the sentinel wrapped by every [App.Resolve] rejected because
// DI teardown has begun or completed. Compare with errors.Is. It applies from
// the moment the container enters its closing phase — when [App.Shutdown]
// begins the internal tier, after the HTTP drain and the ingress tier — and
// takes precedence over a failed Finalize.
var ErrDIClosed = di.ErrClosed

// DIPanicError is the recovered panic of a DI constructor, or of a
// component's Start, Shutdown or Close. Its fields are the failing Type, the
// Phase (construction, start, shutdown, or late cleanup), the original panic
// Value and the Stack captured on the panicking goroutine. When Value is an
// error it is also reachable through errors.Is/As. Obtain it with
// errors.AsType[*credo.DIPanicError], including through a [*LifecycleError].
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
	// DIPanicStart: a component's Start panicked during the start walk.
	DIPanicStart = di.PhaseStart
	// DIPanicShutdown: a component's Shutdown or Close panicked during the
	// drain.
	DIPanicShutdown = di.PhaseShutdown
	// DIPanicLateCleanup: a teardown panicked during the best-effort cleanup
	// of an instance constructed after the drain deadline.
	DIPanicLateCleanup = di.PhaseLateCleanup
)

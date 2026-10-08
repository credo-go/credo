package credo

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/credo-go/credo/internal/di"
)

// LifecyclePhase is the phase a [LifecycleEntry] belongs to.
type LifecyclePhase string

const (
	// PhaseStart is the start walk: a component's construction or Start, or
	// a start hook.
	PhaseStart LifecyclePhase = "start"
	// PhaseShutdown is the drain, or the rollback of a failed or interrupted
	// start: a component's Shutdown or Close, or a stop hook.
	PhaseShutdown LifecyclePhase = "shutdown"
)

// LifecycleOutcome is what happened to a component in a [LifecycleEntry].
type LifecycleOutcome string

const (
	// OutcomeFailed: the call returned an error (in Err).
	OutcomeFailed LifecycleOutcome = "failed"
	// OutcomePanicked: the call panicked; Err is a *DIPanicError for a
	// component.
	OutcomePanicked LifecycleOutcome = "panicked"
	// OutcomeAbandoned: the call had not returned at the deadline, or the
	// deadline ended before its turn. A component still running keeps the
	// components it depends on open.
	OutcomeAbandoned LifecycleOutcome = "abandoned"
	// OutcomeKeptOpen: the component was not stopped because a consumer that
	// was abandoned may still use it (KeptOpenBy).
	OutcomeKeptOpen LifecycleOutcome = "kept_open"
)

// LifecycleEntry is one component's record in a [LifecycleError].
type LifecycleEntry struct {
	// Name is the component's credo.Named name, its type name, worker:<name>
	// for a worker, or OnStart[i]/OnStop[i] for a hook.
	Name string
	// Tier is the component's tier.
	Tier Tier
	// Phase is the phase the outcome belongs to.
	Phase LifecyclePhase
	// Outcome is what happened.
	Outcome LifecycleOutcome
	// Err is the failure for OutcomeFailed and OutcomePanicked; nil otherwise.
	Err error
	// KeptOpenBy names the abandoned consumers of an OutcomeKeptOpen entry.
	KeptOpenBy []string
	// Duration is how long an abandoned call had run at the boundary, or how
	// long a completed one took; zero when unknown.
	Duration time.Duration
}

// String renders the entry as it appears in the error text.
func (e LifecycleEntry) String() string {
	var b strings.Builder
	b.WriteString(e.Name)
	if e.Tier != 0 {
		fmt.Fprintf(&b, " (%s)", e.Tier)
	}
	fmt.Fprintf(&b, " %s %s", e.Phase, e.Outcome)
	if e.Duration > 0 {
		fmt.Fprintf(&b, " after %s", e.Duration.Round(time.Millisecond))
	}
	if len(e.KeptOpenBy) > 0 {
		b.WriteString(" by ")
		b.WriteString(strings.Join(e.KeptOpenBy, ", "))
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// LifecycleError reports a failed start, or a start or drain that did not
// complete. It names each component that failed, panicked, was abandoned at
// the deadline or was kept open by an abandoned consumer, with its tier and
// phase. It is an immutable snapshot taken at the boundary: a call returning
// later, or the late cleanup of a construction that completed after the
// deadline, is logged and never written back. Obtain it with
// errors.AsType[*credo.LifecycleError]; Unwrap exposes each failure and the
// context cause, so errors.Is and errors.As traverse it.
type LifecycleError struct {
	// Entries holds the records, start entries first, then the drain's in
	// the order the components were registered. Callers must not modify it.
	Entries []LifecycleEntry
	// Cause is the context error when the deadline or a cancellation ended
	// the phase before it completed; nil otherwise.
	Cause error

	errs []error
}

// Error summarizes the entries.
func (e *LifecycleError) Error() string {
	var b strings.Builder
	b.WriteString("credo: lifecycle")
	if e.Cause != nil {
		fmt.Fprintf(&b, " (%v)", e.Cause)
	}
	sep := ": "
	for _, entry := range e.Entries {
		b.WriteString(sep)
		b.WriteString(entry.String())
		sep = "; "
	}
	return b.String()
}

// Unwrap returns the entries' errors and the context cause.
func (e *LifecycleError) Unwrap() []error {
	return e.errs
}

// lifecycleReport accumulates the entries of one start phase and its
// rollback, or of one drain. The ingress tier writes it beside the HTTP
// drain, so every access holds mu.
type lifecycleReport struct {
	mu      sync.Mutex
	entries []LifecycleEntry
	cause   error
}

func (r *lifecycleReport) add(e LifecycleEntry) {
	r.mu.Lock()
	r.entries = append(r.entries, e)
	r.mu.Unlock()
}

// setCause records the context error that ended a phase early, keeping the
// first.
func (r *lifecycleReport) setCause(err error) {
	r.mu.Lock()
	if r.cause == nil {
		r.cause = err
	}
	r.mu.Unlock()
}

// addShutdown converts the container's teardown report into entries.
// Outcomes that are not failures — succeeded, shared, retired, never built,
// a construction that failed earlier, a late cleanup — are left out.
func (r *lifecycleReport) addShutdown(report *di.ShutdownError) {
	if report == nil {
		return
	}
	if report.Cause != nil {
		r.setCause(report.Cause)
	}
	for _, e := range report.Entries {
		entry := LifecycleEntry{Name: e.Name, Tier: Tier(e.Tier), Phase: PhaseShutdown, Err: e.Err,
			Duration: e.Duration}
		switch e.State {
		case di.ShutdownFailed:
			entry.Outcome = OutcomeFailed
		case di.ShutdownPanicked:
			entry.Outcome = OutcomePanicked
		case di.ShutdownRunning, di.ShutdownConstructing, di.ShutdownUnattempted:
			entry.Outcome = OutcomeAbandoned
		case di.ShutdownStartRunning:
			// Reported by the start walk, which abandoned it.
			continue
		case di.ShutdownBlocked:
			entry.Outcome = OutcomeKeptOpen
			entry.KeptOpenBy = e.BlockerNames
			entry.Duration = 0
		default:
			continue
		}
		r.add(entry)
	}
}

// err returns the report as a *LifecycleError, or nil when it holds nothing.
func (r *lifecycleReport) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) == 0 {
		return nil
	}
	errs := make([]error, 0, len(r.entries)+1)
	for _, e := range r.entries {
		if e.Err != nil {
			errs = append(errs, e.Err)
		}
	}
	if r.cause != nil {
		errs = append(errs, r.cause)
	}
	return &LifecycleError{Entries: slices.Clone(r.entries), Cause: r.cause, errs: errs}
}

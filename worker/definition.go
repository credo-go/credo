package worker

import (
	"errors"
	"time"
)

// DefaultRestartDelay is the default delay between continuous worker restarts.
const DefaultRestartDelay = 3 * time.Second

// Option configures worker registration.
type Option func(*options)

type options struct {
	hasSchedule               bool
	scheduleExpr              string
	hasMaxRestarts            bool
	maxRestarts               int
	hasRestartDelay           bool
	restartDelay              time.Duration
	hasMaxConsecutiveFailures bool
	maxConsecutiveFailures    int
	startImmediately          bool
	hasRunTimeout             bool
	runTimeout                time.Duration
	hasReadiness              bool
	readiness                 ReadinessPolicy
}

type restartPolicy struct {
	maxRestarts  int
	restartDelay time.Duration
}

type failurePolicy struct {
	maxConsecutiveFailures int
}

// definition is the immutable configuration of a registered worker.
type definition struct {
	name string
	// resolve yields the worker instance when the pool starts: a closure over
	// the instance for Register, a DI resolution for RegisterProvided.
	resolve func() (Worker, error)
	// source names what resolve produces in resolution errors.
	source           string
	schedule         *Schedule
	restartPolicy    restartPolicy
	failurePolicy    failurePolicy
	startImmediately bool
	runTimeout       time.Duration    // scheduled only; 0: no timeout
	readiness        *ReadinessPolicy // nil: the worker does not take part in readiness
}

func (d *definition) kind() Kind {
	if d.schedule != nil {
		return KindScheduled
	}
	return KindContinuous
}

func (d *definition) scheduleExpr() string {
	if d.schedule == nil {
		return ""
	}
	return d.schedule.String()
}

// config projects the definition onto its public, effective form. Readiness
// is a fresh copy, so a caller mutating a snapshot never reaches the
// definition.
func (d *definition) config() Config {
	cfg := Config{
		Schedule:               d.scheduleExpr(),
		StartImmediately:       d.startImmediately,
		RunTimeout:             d.runTimeout,
		MaxConsecutiveFailures: d.failurePolicy.maxConsecutiveFailures,
		MaxRestarts:            d.restartPolicy.maxRestarts,
		RestartDelay:           d.restartPolicy.restartDelay,
	}
	if d.readiness != nil {
		policy := *d.readiness
		cfg.Readiness = &policy
	}
	return cfg
}

// info is the single builder of Info: every snapshot — before Start from the
// definition alone, afterwards from a runner — goes through it.
func (d *definition) info(state runState) Info {
	return Info{
		Name:                d.name,
		Kind:                d.kind(),
		Config:              d.config(),
		Status:              state.status,
		Restarts:            state.restarts,
		ConsecutiveFailures: state.consecutiveFailures,
		LastRun:             state.lastRun,
		LastSuccess:         state.lastSuccess,
		LastError:           state.lastError,
	}
}

// WithMaxRestarts limits how often a continuous worker is restarted: a
// positive n allows the first run plus at most n restarts, and the worker is
// marked failed when a run fails after the n-th restart. Zero (the default)
// means unlimited restarts.
func WithMaxRestarts(n int) Option {
	return func(o *options) {
		o.hasMaxRestarts = true
		o.maxRestarts = n
	}
}

// WithRestartDelay sets the delay between continuous worker restarts.
// A zero delay is treated as the default (DefaultRestartDelay) to avoid
// busy-looping a worker that fails immediately on every run.
func WithRestartDelay(d time.Duration) Option {
	return func(o *options) {
		o.hasRestartDelay = true
		o.restartDelay = d
	}
}

// WithSchedule makes the worker scheduled using a cron expression.
func WithSchedule(expr string) Option {
	return func(o *options) {
		o.hasSchedule = true
		o.scheduleExpr = expr
	}
}

// WithMaxConsecutiveFailures sets the failure threshold for scheduled workers.
// Zero (the default) means unlimited failures; the worker is marked failed
// only once a positive limit is reached.
func WithMaxConsecutiveFailures(n int) Option {
	return func(o *options) {
		o.hasMaxConsecutiveFailures = true
		o.maxConsecutiveFailures = n
	}
}

// WithStartImmediately runs a scheduled worker once during startup.
func WithStartImmediately() Option {
	return func(o *options) {
		o.startImmediately = true
	}
}

// ErrRunTimeout is the cancellation cause of a run that exceeded its
// [WithRunTimeout] budget. Inside Run,
//
//	errors.Is(context.Cause(ctx), worker.ErrRunTimeout)
//
// tells the budget running out from the application shutting down. A run cut
// short by the timeout is recorded as a failure wrapping ErrRunTimeout, even
// when Run returns nil.
var ErrRunTimeout = errors.New("worker: run timed out")

// WithRunTimeout bounds every run of a scheduled worker, including the
// startup run of [WithStartImmediately]: once d has elapsed the run context
// is cancelled with the cause [ErrRunTimeout], and the run counts as a
// failure whatever Run then returns. The timeout is cooperative — it cancels
// the context and never abandons the running goroutine — so a Run that
// ignores its context keeps the worker busy, and activations that pass in the
// meantime are skipped. Zero (the default) means no timeout.
func WithRunTimeout(d time.Duration) Option {
	return func(o *options) {
		o.hasRunTimeout = true
		o.runTimeout = d
	}
}

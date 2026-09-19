package worker

import "time"

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

// WithMaxRestarts sets the maximum restart count for continuous workers.
// Zero (the default) means unlimited restarts; the worker is marked failed
// only once a positive limit is reached.
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

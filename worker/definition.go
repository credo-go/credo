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
}

type restartPolicy struct {
	maxRestarts  int
	restartDelay time.Duration
}

type failurePolicy struct {
	maxConsecutiveFailures int
}

// Definition is the immutable configuration of a registered worker.
type Definition struct {
	name             string
	worker           Worker
	schedule         *Schedule
	restartPolicy    restartPolicy
	failurePolicy    failurePolicy
	startImmediately bool
}

// Kind reports whether the worker is continuous or scheduled.
func (d *Definition) Kind() string {
	if d != nil && d.schedule != nil {
		return kindScheduled
	}
	return kindContinuous
}

func (d *Definition) scheduleExpr() string {
	if d == nil || d.schedule == nil {
		return ""
	}
	return d.schedule.String()
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

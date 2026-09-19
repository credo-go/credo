package worker

import "time"

// Kind reports whether a worker runs continuously or on a schedule.
type Kind string

const (
	// KindContinuous is a worker whose Run lives until the pool stops.
	KindContinuous Kind = "continuous"
	// KindScheduled is a worker that runs once per cron activation.
	KindScheduled Kind = "scheduled"
)

// Status represents a worker's current lifecycle state.
type Status string

const (
	// StatusIdle means the worker is registered but not yet started.
	StatusIdle Status = "idle"
	// StatusRunning means the worker is actively executing.
	StatusRunning Status = "running"
	// StatusWaiting means the worker is waiting for restart delay or next tick.
	StatusWaiting Status = "waiting"
	// StatusStopped means the worker exited normally and will not run again.
	StatusStopped Status = "stopped"
	// StatusFailed means the worker exceeded its configured failure threshold.
	StatusFailed Status = "failed"
)

// Config is the effective, immutable configuration of a registered worker:
// the policy the runner executes once defaults and the pool configuration are
// applied, not an echo of the options passed to [Register]. Fields that do
// not apply to the worker's [Kind] are zero, and zero limits mean unlimited.
type Config struct {
	// Schedule is the cron expression of a scheduled worker, as registered.
	Schedule string `json:"schedule"`
	// StartImmediately reports [WithStartImmediately] (scheduled workers).
	StartImmediately bool `json:"start_immediately"`
	// RunTimeout is the [WithRunTimeout] budget of each run (scheduled
	// workers); 0 means no timeout.
	RunTimeout time.Duration `json:"run_timeout"`
	// MaxConsecutiveFailures is the [WithMaxConsecutiveFailures] limit
	// (scheduled workers); 0 means unlimited.
	MaxConsecutiveFailures int `json:"max_consecutive_failures"`
	// MaxRestarts is the [WithMaxRestarts] limit (continuous workers); 0
	// means unlimited.
	MaxRestarts int `json:"max_restarts"`
	// RestartDelay is the base restart delay of a continuous worker — the
	// first and the minimum wait before a restart — after the option, the
	// pool's worker.restart_delay configuration and [DefaultRestartDelay] are
	// resolved.
	RestartDelay time.Duration `json:"restart_delay"`
	// MaxRestartDelay is the cap the restart delay of a continuous worker
	// backs off to, resolved as described at [WithMaxRestartDelay]; never
	// below RestartDelay.
	MaxRestartDelay time.Duration `json:"max_restart_delay"`
	// Readiness is a copy of the worker's [ReadinessPolicy]; nil when the
	// worker does not take part in readiness.
	Readiness *ReadinessPolicy `json:"readiness,omitzero"`
}

// Info is a point-in-time snapshot of a worker: its identity, its effective
// configuration and its live state.
//
// Info is shaped for direct JSON encoding (for example from an admin
// endpoint): field names are snake_case, durations encode as integer
// nanoseconds under Credo's response profile, and last_run, last_success,
// last_error and config.readiness are omitted while zero.
type Info struct {
	Name   string `json:"name"`
	Kind   Kind   `json:"kind"`
	Config Config `json:"config"`

	Status Status `json:"status"`
	// Restarts counts the restarts of a continuous worker.
	Restarts int64 `json:"restarts"`
	// ConsecutiveFailures counts the failed runs of a scheduled worker since
	// its last success.
	ConsecutiveFailures int64     `json:"consecutive_failures"`
	LastRun             time.Time `json:"last_run,omitzero"`
	// LastSuccess is the completion time of the last successful run of a
	// scheduled worker; zero until then, and always zero for a continuous
	// worker, whose Run never completes successfully.
	LastSuccess time.Time `json:"last_success,omitzero"`
	// LastError is the error of the most recent failed run, without any
	// stack trace; a successful scheduled run clears it.
	LastError string `json:"last_error,omitzero"`
}

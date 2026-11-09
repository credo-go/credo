package worker

import "time"

// Kind reports whether a worker runs continuously or on a schedule.
type Kind string

const (
	// KindContinuous is a worker whose Run lives until its component stops.
	KindContinuous Kind = "continuous"
	// KindScheduled is a worker that runs once per cron activation.
	KindScheduled Kind = "scheduled"
)

// Status is a worker's lifecycle state.
type Status string

const (
	// StatusPending means the worker's component has not started, or a
	// continuous worker has not yet been admitted to its first run.
	StatusPending Status = "pending"
	// StatusRunning means a run has been admitted and is executing.
	StatusRunning Status = "running"
	// StatusBackoff means a continuous worker is waiting to restart after a
	// failure: unhealthy, recovering.
	StatusBackoff Status = "backoff"
	// StatusWaiting means a scheduled worker is waiting for its next
	// activation, whatever its last run did.
	StatusWaiting Status = "waiting"
	// StatusStopped means the loop ended because the worker's component was
	// shut down; the worker will not run again.
	StatusStopped Status = "stopped"
	// StatusFailed means a positive failure limit was exhausted, restarts are
	// disabled and a run failed, or the schedule has no future activation. It
	// is permanent until the application restarts.
	StatusFailed Status = "failed"
)

// Info is a point-in-time snapshot of a worker: its identity, its resolved
// configuration and its live state. The root package converts it to
// credo.WorkerInfo, whose fields document these and which owns the JSON
// shape.
type Info struct {
	Name string
	Kind Kind
	// Schedule is the expression of a scheduled worker, as registered; empty
	// for a continuous worker.
	Schedule string
	// Continuous is the resolved configuration of a continuous worker; nil
	// for a scheduled one. Each snapshot holds its own copy.
	Continuous *ContinuousConfig
	// Scheduled is the resolved configuration of a scheduled worker; nil for
	// a continuous one. Each snapshot holds its own copy.
	Scheduled *ScheduledConfig

	Status Status
	// Restarts counts the restarts of a continuous worker that started.
	Restarts int64
	// ConsecutiveFailures counts the failed runs of a scheduled worker since
	// its last success.
	ConsecutiveFailures int64
	// LastStartedAt is the start time of the most recently admitted run.
	LastStartedAt time.Time
	// LastSucceededAt is the completion time of the last successful run of a
	// scheduled worker; always zero for a continuous worker.
	LastSucceededAt time.Time
	// LastError is the error of the most recent failed run, without any stack
	// trace; a successful scheduled run clears it, a graceful stop does not.
	LastError string
}

package worker

import "time"

const (
	kindContinuous = "continuous"
	kindScheduled  = "scheduled"
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

// Info is a point-in-time snapshot of a worker's state.
type Info struct {
	Name      string
	Kind      string
	Schedule  string
	Status    Status
	Attempts  int64 // restart count for continuous workers; consecutive failures for scheduled workers.
	LastRun   time.Time
	LastError string
}

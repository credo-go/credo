package health

import "time"

// StoreResult holds the outcome of a store health check.
type StoreResult struct {
	Name    string
	Status  string
	Latency time.Duration
	Cause   error  `json:"-"`
	Error   string `json:"-"`
}

// StoreCheck describes one independently bounded store probe. Probe must be a
// stable pointer retained across readiness requests so overlapping calls join
// one flight instead of starting unbounded goroutines.
type StoreCheck struct {
	Name  string
	Probe *Probe
}

// StoreFunc returns an in-memory snapshot of independently executable store
// checks for the readiness endpoint. Implementations must not perform I/O or
// block; only each StoreCheck.Probe is executed through the bounded runner.
// The root builds the checks once, from the stores the start phase pinged.
type StoreFunc func() []StoreCheck

// ReadinessCheck is one named readiness contribution: a component's Ready,
// a worker's among them. It is reported exactly like a check added
// through credo.App.AddReadinessCheck and shares that name space: a name that
// collides with a named or store check fails closed as a configuration error.
// Probe must be a stable pointer retained across readiness requests.
type ReadinessCheck struct {
	Name  string
	Probe *Probe
}

// ReadinessFunc returns an in-memory snapshot of contributed readiness checks.
// Implementations must not perform I/O or block; only each Probe is executed
// through the bounded runner. Root supplies the Ready of the components that
// answer it, built once by the start walk.
type ReadinessFunc func() []ReadinessCheck

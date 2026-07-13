package health

import "time"

// StoreResult holds the outcome of a store health check
// (provided by store.Registry via [StoreFunc]).
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
// store.Register provides one into DI and root resolves it lazily, so
// registration order does not matter.
type StoreFunc func() []StoreCheck

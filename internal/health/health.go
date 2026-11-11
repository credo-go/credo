package health

// ReadinessCheck is one contributed readiness entry: the Ready of a
// resource's owner, a worker's among them. It is reported exactly
// like a check added through credo.App.AddReadinessCheck and shares that name
// space: a name that collides with a named check or another contribution
// fails closed as a configuration error. Probe must be a stable pointer
// retained across readiness requests. An Optional entry is reported without
// gating readiness.
type ReadinessCheck struct {
	Name     string
	Probe    *Probe
	Optional bool
}

// ReadinessFunc returns an in-memory snapshot of contributed readiness checks.
// Implementations must not perform I/O or block; only each Probe is executed
// through the bounded runner. Root supplies one check per resource whose
// holder answers Ready, built once by the start walk.
type ReadinessFunc func() []ReadinessCheck

// Package health defines stable, bounded health probes and the readiness
// engine the root package runs them through.
//
// The root package builds a [StoreCheck] for every store the start phase
// pinged and a [ReadinessCheck] for every component that answers Ready, once,
// and hands them to [Engine.CheckReadiness], which runs every stable [Probe]
// through the same bounded parallel scheduler as named checks. The root
// package owns the HTTP endpoints, the public registration API, and the
// response/logging policy; this package owns scheduling, store-result
// normalization, and name validation. It cannot be imported from outside the
// module, so none of this is visible to user code.
package health

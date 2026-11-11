// Package health defines stable, bounded health probes and the readiness
// engine the root package runs them through.
//
// The root package builds a [ReadinessCheck] for every resource whose owner
// answers Ready, once, and hands them to [Engine.CheckReadiness], which runs
// every stable [Probe] through the same bounded parallel scheduler as named
// checks and measures each run's latency. The root package owns the HTTP
// endpoints, the public registration API, and the response/logging policy;
// this package owns scheduling, the aggregation of gating and optional
// checks, and name validation. It cannot be imported from outside the
// module, so none of this is visible to user code.
package health

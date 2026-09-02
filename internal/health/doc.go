// Package health defines stable, bounded health probes and the module-internal
// seam through which integration packages contribute store checks to the root
// health engine.
//
// store.Register provides a [StoreFunc] into the DI container; the root package
// resolves it lazily and hands it to [Engine.CheckReadiness], which runs every
// stable [Probe] through the same bounded parallel scheduler as named checks.
// The root package owns the HTTP endpoints, the public registration API, and
// the response/logging policy; this package owns scheduling, store-result
// normalization, and name validation. Keeping the seam here makes the wiring
// invisible to user code because this package cannot be imported from outside
// the module.
package health

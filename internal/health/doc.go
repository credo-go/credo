// Package health defines stable, bounded health probes and the module-internal
// seam through which integration packages contribute store checks to the root
// health engine.
//
// store.Register provides a [StoreFunc] into the DI container; the root package
// resolves it lazily and runs every stable [Probe] through the same parallel
// scheduler as named checks. Keeping the seam here makes the wiring invisible
// to user code: the engine is unexported and this package cannot be imported
// from outside the module.
package health

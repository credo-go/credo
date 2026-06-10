// Package health defines the module-internal seam through which integration
// packages contribute store health results to the root health engine.
//
// store.Register provides a [StoreFunc] into the DI container; the root
// package resolves it lazily on each readiness check. Keeping the seam types
// here (instead of exporting them from the root package) makes the wiring
// invisible to user code: the engine itself lives unexported in the root
// package, and this package cannot be imported from outside the module.
package health

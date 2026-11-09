// Package kernel is the seam between the root package and the integration
// packages that register with an App's kernel registries rather than with
// its DI container. The root package cannot be imported back by the
// integrations' internals, and the registries are not part of the public
// API, so the root installs the entry points here at init and the
// integrations call them.
package kernel

// Component is a component an integration adds to an App on behalf of its
// own registration call.
type Component struct {
	// Value is a component value, or a constructor over DI parameters, as
	// App.Manage takes it.
	Value any
	// Name names the component in reports and readiness.
	Name string
	// Ingress places the component in the ingress tier.
	Ingress bool
	// IngressRemedy is how the integration's registration declares the
	// component ingress, for the Finalize finding of an internal component
	// that depends on an ingress one (e.g. a worker's Tier field).
	IngressRemedy string
}

// Manage adds c to app, a *credo.App, as App.Manage does, and panics as it
// does. The root package sets it at init.
var Manage func(app any, c Component)

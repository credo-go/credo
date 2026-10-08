// Package kernel is the seam between the root package and the integration
// packages that register with an App's kernel registries rather than with
// its DI container. The root package cannot be imported back by the
// integrations' internals, and the registries are not part of the public
// API, so the root installs the entry points here at init and the
// integrations call them.
package kernel

import (
	"context"
	"reflect"

	internalhealth "github.com/credo-go/credo/internal/health"
)

// Store is one store registration: the binding type the start phase pings
// and readiness reports.
type Store struct {
	// Name is the store's name in readiness reports.
	Name string
	// Type is the binding type R of store.Register[R].
	Type reflect.Type
	// Ping verifies the built value; the start phase calls it once, before
	// the value's consumers start, and a failure is a start failure.
	Ping func(ctx context.Context, value any) error
	// Probe builds the readiness probe of the built value, once.
	Probe func(value any) *internalhealth.Probe
}

// RegisterStore adds a store registration to app, a *credo.App. It panics
// on misuse, naming the registering call. The root package sets it at init.
var RegisterStore func(app any, s Store)

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

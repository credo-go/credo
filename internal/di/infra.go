package di

import "reflect"

// FrameworkProvider produces a value for constructor parameters of Type that
// have no registration of their own: the framework injects them. The root
// package registers credo.Infra this way so the container can produce it
// without importing the root package (avoiding an import cycle). Factory
// receives the short service name of the constructor being built (see
// deriveServiceName) and returns the value to pass.
type FrameworkProvider struct {
	Type    reflect.Type
	Factory func(serviceName string) any
}

// SetFrameworkProvider registers (or replaces) the framework provider for
// p.Type. Must be called before any Resolve or Seal (typically in credo.New);
// the resolve path reads the provider map without locking.
func (c *Container) SetFrameworkProvider(p FrameworkProvider) {
	if p.Type == nil || p.Factory == nil {
		panic("di: SetFrameworkProvider: Type and Factory must not be nil")
	}
	c.frameworkProviders[p.Type] = p
}

// frameworkProvider returns the provider for t when the framework produces it.
func (c *Container) frameworkProvider(t reflect.Type) (FrameworkProvider, bool) {
	p, ok := c.frameworkProviders[t]
	return p, ok
}

// isFrameworkType reports whether t is produced by a framework provider
// rather than satisfied by a registration.
func (c *Container) isFrameworkType(t reflect.Type) bool {
	_, ok := c.frameworkProviders[t]
	return ok
}

// deriveServiceName extracts a short service name from a reflect.Type.
// For "*myapp.OrderService" it returns "OrderService".
func deriveServiceName(t reflect.Type) string {
	name := t.Name()
	if name != "" {
		return name
	}
	// Pointer types: dereference.
	if t.Kind() == reflect.Pointer {
		return deriveServiceName(t.Elem())
	}
	return t.String()
}

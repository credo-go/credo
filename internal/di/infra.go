package di

import "reflect"

// InfraProvider configures automatic Infra injection for constructors.
// The root package sets this on the container so the DI system can produce
// infrastructure infra without importing the root package (avoiding cycles).
type InfraProvider struct {
	// InfraType is the reflect.Type of the Infra struct (e.g., credo.Infra).
	InfraType reflect.Type

	// Factory creates an Infra value with the logger scoped to serviceName.
	// Returns the Infra struct value (not a pointer).
	Factory func(serviceName string) any
}

// SetInfraProvider configures infra auto-injection for constructors.
// Must be called before any Resolve calls (typically in credo.New).
func (c *Container) SetInfraProvider(p *InfraProvider) {
	c.infraProvider = p
}

// isInfraType reports whether the given type is the configured Infra type.
func (c *Container) isInfraType(t reflect.Type) bool {
	return c.infraProvider != nil && t == c.infraProvider.InfraType
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

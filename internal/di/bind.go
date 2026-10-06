package di

import (
	"fmt"
	"reflect"
)

// Alias creates a type alias so that Resolve[I] returns the singleton
// registered for concrete type T. Contract rules:
//   - T must already be registered via Provide or ProvideValue
//   - I must be an interface type
//   - T must implement I
//   - I must not already have a registration or alias
//   - Container must not be frozen (container is sealed)
func (c *Container) Alias[I, T any]() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.frozen {
		return fmt.Errorf("di: Alias[%s, %s]: container is frozen (container is sealed)",
			reflect.TypeFor[I](), reflect.TypeFor[T]())
	}

	ifaceType := reflect.TypeFor[I]()
	concreteType := reflect.TypeFor[T]()

	// I must be an interface.
	if ifaceType.Kind() != reflect.Interface {
		return fmt.Errorf("di: Alias[%s, %s]: first type parameter must be an interface",
			ifaceType, concreteType)
	}

	// T must implement I.
	if !concreteType.Implements(ifaceType) {
		return fmt.Errorf("di: Alias[%s, %s]: %s does not implement %s",
			ifaceType, concreteType, concreteType, ifaceType)
	}

	// T must be registered.
	if _, ok := c.registrations[concreteType]; !ok {
		return fmt.Errorf("di: Alias[%s, %s]: concrete type %s is not registered",
			ifaceType, concreteType, concreteType)
	}

	// I must not already be registered or aliased.
	if _, ok := c.registrations[ifaceType]; ok {
		return fmt.Errorf("di: Alias[%s, %s]: interface %s already has a direct registration",
			ifaceType, concreteType, ifaceType)
	}
	if _, ok := c.aliases[ifaceType]; ok {
		return fmt.Errorf("di: Alias[%s, %s]: interface %s already has an alias",
			ifaceType, concreteType, ifaceType)
	}

	c.aliases[ifaceType] = concreteType
	return nil
}

// MustAlias is like Alias but panics on error.
func (c *Container) MustAlias[I, T any]() {
	if err := c.Alias[I, T](); err != nil {
		panic(err)
	}
}

// BindMany adds concrete type T to the ordered collection for interface I.
// Contract rules:
//   - T must already be registered via Provide or ProvideValue
//   - I must be an interface type
//   - T must be a concrete type (not an interface)
//   - T must implement I
//   - The same (I, T) pair must not already exist
//   - Container must not be frozen (container is sealed)
func (c *Container) BindMany[I, T any]() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.frozen {
		return fmt.Errorf("di: BindMany[%s, %s]: container is frozen (container is sealed)",
			reflect.TypeFor[I](), reflect.TypeFor[T]())
	}

	ifaceType := reflect.TypeFor[I]()
	concreteType := reflect.TypeFor[T]()

	if ifaceType.Kind() != reflect.Interface {
		return fmt.Errorf("di: BindMany[%s, %s]: first type parameter must be an interface",
			ifaceType, concreteType)
	}

	if concreteType.Kind() == reflect.Interface {
		return fmt.Errorf("di: BindMany[%s, %s]: second type parameter must be a concrete type",
			ifaceType, concreteType)
	}

	if !concreteType.Implements(ifaceType) {
		return fmt.Errorf("di: BindMany[%s, %s]: %s does not implement %s",
			ifaceType, concreteType, concreteType, ifaceType)
	}

	if _, ok := c.registrations[concreteType]; !ok {
		return fmt.Errorf("di: BindMany[%s, %s]: concrete type %s is not registered",
			ifaceType, concreteType, concreteType)
	}

	set, ok := c.manyBindingSet[ifaceType]
	if !ok {
		set = make(map[reflect.Type]struct{})
		c.manyBindingSet[ifaceType] = set
	}
	if _, exists := set[concreteType]; exists {
		return fmt.Errorf("di: BindMany[%s, %s]: binding already exists", ifaceType, concreteType)
	}

	set[concreteType] = struct{}{}
	c.manyBindings[ifaceType] = append(c.manyBindings[ifaceType], concreteType)
	return nil
}

// MustBindMany is like BindMany but panics on error.
func (c *Container) MustBindMany[I, T any]() {
	if err := c.BindMany[I, T](); err != nil {
		panic(err)
	}
}

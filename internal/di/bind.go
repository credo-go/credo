package di

import (
	"reflect"
)

// Alias creates a type alias so that Resolve[I] returns the singleton
// registered for concrete type T. Contract rules, each rejection a
// [*MisuseError]:
//   - T must already be registered via Provide or ProvideValue
//   - I must be an interface type
//   - T must implement I
//   - I must not already have a registration or alias
//   - the registration window must be open (before Seal and Shutdown)
func (c *Container) Alias[I, T any]() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	ifaceType := reflect.TypeFor[I]()
	concreteType := reflect.TypeFor[T]()
	types := []reflect.Type{ifaceType, concreteType}
	if err := c.closedLocked("Alias", types...); err != nil {
		return err
	}

	switch {
	case ifaceType.Kind() != reflect.Interface:
		return misuse("Alias", types, "first type parameter must be an interface")
	case !concreteType.Implements(ifaceType):
		return misuse("Alias", types, "%s does not implement %s", concreteType, ifaceType)
	}
	if _, ok := c.registrations[concreteType]; !ok {
		return misuse("Alias", types, "concrete type %s is not registered; provide it before aliasing it",
			concreteType)
	}
	if _, ok := c.registrations[ifaceType]; ok {
		return misuse("Alias", types, "interface %s already has a direct registration", ifaceType)
	}
	if _, ok := c.aliases[ifaceType]; ok {
		return misuse("Alias", types, "interface %s already has an alias", ifaceType)
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
// Contract rules, each rejection a [*MisuseError]:
//   - T must already be registered via Provide or ProvideValue
//   - I must be an interface type
//   - T must be a concrete type (not an interface)
//   - T must implement I
//   - The same (I, T) pair must not already exist
//   - the registration window must be open (before Seal and Shutdown)
func (c *Container) BindMany[I, T any]() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	ifaceType := reflect.TypeFor[I]()
	concreteType := reflect.TypeFor[T]()
	types := []reflect.Type{ifaceType, concreteType}
	if err := c.closedLocked("BindMany", types...); err != nil {
		return err
	}

	switch {
	case ifaceType.Kind() != reflect.Interface:
		return misuse("BindMany", types, "first type parameter must be an interface")
	case concreteType.Kind() == reflect.Interface:
		return misuse("BindMany", types, "second type parameter must be a concrete type")
	case !concreteType.Implements(ifaceType):
		return misuse("BindMany", types, "%s does not implement %s", concreteType, ifaceType)
	}
	if _, ok := c.registrations[concreteType]; !ok {
		return misuse("BindMany", types, "concrete type %s is not registered; provide it before binding it",
			concreteType)
	}

	set, ok := c.manyBindingSet[ifaceType]
	if !ok {
		set = make(map[reflect.Type]struct{})
		c.manyBindingSet[ifaceType] = set
	}
	if _, exists := set[concreteType]; exists {
		return misuse("BindMany", types, "binding already exists")
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

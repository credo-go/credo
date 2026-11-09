package di

import (
	"fmt"
	"reflect"
)

// Alias creates a type alias so that Resolve[I] returns the singleton
// registered for concrete type T. An alias is a binding of I: a direct
// binding of I after it is a duplicate unless it carries credo.Override(),
// which replaces the alias. Contract rules, each rejection a [*MisuseError]:
//   - T must already be registered via Provide or ProvideValue
//   - I must be an interface type, other than T
//   - T must implement I
//   - I must not already have a registration or alias, unless o.Override
//     replaces it; o.Override with neither is refused
//   - the registration window must be open (before Seal and Shutdown)
//
// The type an overridden alias pointed at stays registered until Seal, which
// drops it when nothing else reaches it.
func (c *Container) Alias[I, T any]() error {
	return c.AliasWith[I, T](Options{})
}

// AliasWith is [Container.Alias] with registration options; Override is the
// one it reads.
func (c *Container) AliasWith[I, T any](o Options) error {
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
	case ifaceType == concreteType:
		return misuse("Alias", types, "an interface cannot be an alias of itself")
	case !concreteType.Implements(ifaceType):
		return misuse("Alias", types, "%s does not implement %s", concreteType, ifaceType)
	}
	if _, ok := c.registrations[concreteType]; !ok {
		return misuse("Alias", types, "concrete type %s is not registered; provide it before aliasing it",
			concreteType)
	}
	_, direct := c.registrations[ifaceType]
	old, aliased := c.aliases[ifaceType]
	switch {
	case direct && !o.Override:
		return misuse("Alias", types, "interface %s already has a direct registration; give credo.Override() "+
			"to replace it with the alias, or drop one of the two", ifaceType)
	case aliased && !o.Override:
		return misuse("Alias", types, "interface %s already has an alias, to %s; give credo.Override() to "+
			"point it at %s, or drop one of the two", ifaceType, old, concreteType)
	case !direct && !aliased && o.Override:
		return misuse("Alias", types, "credo.Override() replaces an earlier binding, but %s has none; "+
			"drop credo.Override()", ifaceType)
	}

	if direct {
		c.removeBindingLocked(ifaceType)
	}
	if aliased && old != concreteType {
		c.noteUnreachedLocked(old, fmt.Sprintf("Alias[%s, %s] with credo.Override() replaced app.Alias[%s, %s]()",
			ifaceType, concreteType, ifaceType, old))
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

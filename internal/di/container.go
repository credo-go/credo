// Adapted from github.com/samber/do (MIT License).

package di

import (
	"reflect"
	"sync"
	"sync/atomic"
)

// Container is a type-safe dependency injection container using Go generics.
// Services are registered with Provide[T] and resolved with Resolve[T].
// All services use the Singleton lifecycle.
type Container struct {
	mu             sync.RWMutex
	registrations  map[reflect.Type]*registration
	singletons     map[reflect.Type]*singletonEntry
	aliases        map[reflect.Type]reflect.Type // interface → concrete type (Bind)
	manyBindings   map[reflect.Type][]reflect.Type
	manyBindingSet map[reflect.Type]map[reflect.Type]struct{}
	order          []reflect.Type // registration order (for shutdown)
	infraProvider  *InfraProvider // optional: auto-injects Infra for Model 1
	frozen         bool           // set after Seal(); prevents new bindings/registrations
	sealOnce       sync.Once
	sealErr        error
}

// singletonEntry provides per-type synchronization for concurrent singleton
// resolution via sync.Once. Multiple different singletons can resolve
// concurrently without blocking each other.
type singletonEntry struct {
	once  sync.Once
	done  atomic.Bool // set after construction completes (for status checks)
	value any
	err   error
}

// New creates a new Container.
func New() *Container {
	return &Container{
		registrations:  make(map[reflect.Type]*registration),
		singletons:     make(map[reflect.Type]*singletonEntry),
		aliases:        make(map[reflect.Type]reflect.Type),
		manyBindings:   make(map[reflect.Type][]reflect.Type),
		manyBindingSet: make(map[reflect.Type]map[reflect.Type]struct{}),
	}
}

// findRegistration searches for a registration by type, following aliases.
// It returns the registration, the canonical (concrete) type under which the
// singleton is cached, and whether the lookup succeeded.
func (c *Container) findRegistration(t reflect.Type) (*registration, reflect.Type, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Direct registration lookup.
	if reg, ok := c.registrations[t]; ok {
		return reg, t, true
	}

	// Follow alias chain: interface → concrete type.
	if concrete, ok := c.aliases[t]; ok {
		if reg, ok := c.registrations[concrete]; ok {
			return reg, concrete, true
		}
	}

	return nil, t, false
}

// hasDirectRegistration reports whether a type has its own registration entry.
func (c *Container) hasDirectRegistration(t reflect.Type) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, ok := c.registrations[t]
	return ok
}

// collectionBindings returns a copy of the concrete types bound to an
// interface via BindMany, preserving binding order.
func (c *Container) collectionBindings(t reflect.Type) []reflect.Type {
	c.mu.RLock()
	defer c.mu.RUnlock()

	bindings := c.manyBindings[t]
	if len(bindings) == 0 {
		return nil
	}

	clone := make([]reflect.Type, len(bindings))
	copy(clone, bindings)
	return clone
}

func isInterfaceSlice(t reflect.Type) bool {
	return t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Interface
}

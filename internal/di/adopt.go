package di

import (
	"fmt"
	"reflect"
)

// Has reports whether type T is registered, directly or through an alias. It
// never constructs, adopts or protects anything and says nothing about the
// instance's health. The result is a snapshot, not a reservation: a later
// registration can still race it.
func (c *Container) Has[T any]() bool {
	_, _, ok := c.findRegistration(reflect.TypeFor[T]())
	return ok
}

// AdoptValue is the registration-phase read of a prebuilt binding: it reads
// the existing value bound directly to T, validates it, and atomically
// compare-and-protects that same binding before returning it. It is how
// framework integrations (store, worker) take ownership of a value the
// composition root provided ahead of them.
//
// It never constructs: a constructor binding for T is rejected with an
// explanatory error without being invoked. A nil validate accepts every
// value. Validation runs outside the container lock; if the binding was
// replaced or the registration window closed meanwhile, adoption fails and
// nothing is protected, because a prior read confers no ownership. A
// validation failure likewise leaves the binding unprotected and repairable
// through Replace. Adopting an already protected binding succeeds when the
// same instance validates.
func (c *Container) AdoptValue[T any](validate func(T) error) (T, error) {
	var zero T
	targetType := reflect.TypeFor[T]()

	c.mu.RLock()
	if c.frozen {
		c.mu.RUnlock()
		return zero, frozenError("AdoptValue", targetType)
	}
	reg, exists := c.registrations[targetType]
	if !exists {
		c.mu.RUnlock()
		return zero, fmt.Errorf("di: AdoptValue[%s]: not registered", targetType)
	}
	if _, isValue := reg.(valueProvider); !isValue {
		c.mu.RUnlock()
		return zero, fmt.Errorf("di: AdoptValue[%s]: binding is a constructor; provide a ready value instead "+
			"(constructors run only after Finalize)", targetType)
	}
	entry := c.singletons[targetType]
	c.mu.RUnlock()

	if entry == nil || entry.state != entryBuilt {
		// A value binding always has a built entry; defensive.
		return zero, fmt.Errorf("di: AdoptValue[%s]: binding has no ready value", targetType)
	}
	value, ok := entry.value.(T)
	if !ok {
		return zero, fmt.Errorf("di: AdoptValue[%s]: type assertion failed", targetType)
	}

	if validate != nil {
		if err := validate(value); err != nil {
			return zero, fmt.Errorf("di: AdoptValue[%s]: validation failed: %w", targetType, err)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frozen {
		return zero, frozenError("AdoptValue", targetType)
	}
	// The entry pointer is replaced by Replace, so pointer identity proves the
	// validated instance is still the bound one.
	if current := c.singletons[targetType]; current != entry {
		return zero, fmt.Errorf("di: AdoptValue[%s]: binding was replaced during validation", targetType)
	}
	c.protected[targetType] = struct{}{}
	return value, nil
}

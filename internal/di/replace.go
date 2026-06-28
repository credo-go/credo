// Adapted from github.com/samber/do (MIT License).

package di

import (
	"fmt"
	"reflect"
)

// Replace registers a pre-built value for type T as a Singleton, overwriting
// any existing registration for T. Unlike [ProvideValue], Replace does not
// return an error when T is already registered: it replaces the binding and
// discards the previous registration along with any cached singleton.
//
// Replace is intended for composition-root overrides and testing, where a
// real or default binding must be swapped for a stub or fake. The replacement
// is a value (no constructor), so it has no dependencies and is always valid
// during Seal. Replace is rejected once the container is sealed.
func (c *Container) Replace[T any](value T) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.frozen {
		return fmt.Errorf("di: Replace[%s]: container is frozen (container is sealed)", reflect.TypeFor[T]())
	}

	targetType := reflect.TypeFor[T]()

	reg := &registration{
		resultType: targetType,
		isValue:    true,
		value:      value,
	}

	// Preserve registration order: only append when this type is new, so
	// repeated replacements never create duplicate shutdown-order entries.
	if _, exists := c.registrations[targetType]; !exists {
		c.order = append(c.order, targetType)
	}
	c.registrations[targetType] = reg

	// Cache the value immediately, superseding any previously resolved
	// singleton for this type.
	entry := &singletonEntry{value: value}
	entry.done.Store(true)
	c.singletons[targetType] = entry

	return nil
}

// MustReplace is like Replace but panics on error.
func (c *Container) MustReplace[T any](value T) {
	if err := c.Replace[T](value); err != nil {
		panic(err)
	}
}

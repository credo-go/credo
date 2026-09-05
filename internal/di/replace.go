// Adapted from github.com/samber/do (MIT License).

package di

import (
	"fmt"
	"reflect"
)

// Replace registers a pre-built value for type T as a Singleton, overwriting
// any existing unprotected registration. Bindings created by
// [Container.ProvideProtectedValue], locked by [Container.ProtectBinding] or
// adopted through [Container.AdoptValue] reject replacement so external
// lifecycle state cannot diverge from DI.
//
// On success the container owns the new value and stops tracking the
// superseded instance: it returns that instance together with existed == true
// when a previously created instance existed (a prebuilt value or a built
// singleton), and the caller assumes its cleanup responsibility. A superseded
// constructor binding that never ran yields the zero value and false; Replace
// never constructs an old provider merely to return it. A rejected replacement
// changes neither the binding nor ownership.
//
// Replace is intended for composition-root overrides and testing, where a
// real or default binding must be swapped for a stub or fake. The replacement
// is a value (no constructor), so it has no dependencies and is always valid
// during Seal. Replace is rejected once the container is frozen.
func (c *Container) Replace[T any](value T) (old T, existed bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	targetType := reflect.TypeFor[T]()
	if c.frozen {
		return old, false, frozenError("Replace", targetType)
	}
	if _, protected := c.protected[targetType]; protected {
		return old, false, fmt.Errorf("di: Replace[%s]: binding is protected", targetType)
	}

	// Preserve registration order: only append when this type is new, so
	// repeated replacements never create duplicate teardown-order entries.
	if _, exists := c.registrations[targetType]; !exists {
		c.order = append(c.order, targetType)
	}
	c.registrations[targetType] = valueProvider{value: value}

	// Detach the superseded instance, if one was ever created, and cache the
	// new value immediately. Builds happen only after Seal, so the previous
	// entry is either prebuilt/built or never started; never in flight.
	if previous, ok := c.singletons[targetType]; ok && previous.state == entryBuilt {
		if typed, ok := previous.value.(T); ok {
			old, existed = typed, true
		}
	}
	c.singletons[targetType] = &singletonEntry{state: entryBuilt, value: value}

	return old, existed, nil
}

// MustReplace is like Replace but panics on error. It returns the same
// previous-instance information.
func (c *Container) MustReplace[T any](value T) (old T, existed bool) {
	old, existed, err := c.Replace[T](value)
	if err != nil {
		panic(err)
	}
	return old, existed
}

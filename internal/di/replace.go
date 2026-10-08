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
	targetType := reflect.TypeFor[T]()
	token, hasToken, idErr := identityToken(any(value))

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.closedLocked("Replace", targetType); err != nil {
		return old, false, err
	}
	if _, protected := c.protected[targetType]; protected {
		return old, false, fmt.Errorf("di: Replace[%s]: binding is protected", targetType)
	}
	if idErr != nil {
		return old, false, fmt.Errorf("di: Replace[%s]: %w", targetType, idErr)
	}

	u := &Unit{t: targetType, name: targetType.String(), valueBinding: true}
	planUnit(u, Options{})
	c.forgetHolderLocked(c.unitOf[targetType])
	if err := c.admitValueLocked(u, any(value), token, hasToken); err != nil {
		return old, false, fmt.Errorf("di: Replace[%s]: %w", targetType, err)
	}

	// Detach the superseded instance, if one was ever created. Builds happen
	// only after Seal, so the previous entry is either prebuilt/built or never
	// started; never in flight.
	if previous, ok := c.singletons[targetType]; ok && previous.state == entryBuilt {
		if typed, ok := previous.value.(T); ok {
			old, existed = typed, true
		}
	}
	c.bindLocked(u, valueProvider{value: value}, &singletonEntry{state: entryBuilt, value: value})

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

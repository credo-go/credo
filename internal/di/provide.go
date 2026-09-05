// Adapted from github.com/samber/do (MIT License).

package di

import (
	"errors"
	"fmt"
	"reflect"
)

// Provide registers a constructor for type T. The constructor can accept
// any number of parameters that are themselves registered in the container,
// and must return T or (T, error). It runs at most once, on the first
// resolution after Seal.
//
//	c.Provide[MyService](NewMyService)
func (c *Container) Provide[T any](constructor any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	targetType := reflect.TypeFor[T]()
	if c.frozen {
		return frozenError("Provide", targetType)
	}

	reg, err := inspectConstructor(constructor, targetType)
	if err != nil {
		return fmt.Errorf("di: Provide[%s]: %w", targetType, err)
	}

	if _, exists := c.registrations[targetType]; exists {
		return fmt.Errorf("di: Provide[%s]: already registered", targetType)
	}

	c.registrations[targetType] = reg
	c.order = append(c.order, targetType)

	// Pre-create singleton entry for later lazy resolution.
	c.singletons[targetType] = &singletonEntry{}

	return nil
}

// MustProvide is like Provide but panics on error.
func (c *Container) MustProvide[T any](constructor any) {
	if err := c.Provide[T](constructor); err != nil {
		panic(err)
	}
}

// CanProvideValue reports whether [Container.ProvideValue] could currently
// register type T. It performs only the frozen-container and direct duplicate-T
// checks, without registering or reserving the type.
//
// The result is a point-in-time preflight. A later ProvideValue call can still
// fail if another registration or container sealing occurs in between.
func (c *Container) CanProvideValue[T any]() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.canProvideValueLocked(reflect.TypeFor[T]())
}

// ProvideValue registers a pre-built value for type T as a Singleton.
// The value is cached immediately.
func (c *Container) ProvideValue[T any](value T) error {
	return c.provideValue(value, false)
}

// ProvideProtectedValue registers a pre-built singleton whose binding cannot
// later be overwritten through [Container.Replace].
func (c *Container) ProvideProtectedValue[T any](value T) error {
	return c.provideValue(value, true)
}

func (c *Container) provideValue[T any](value T, protected bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	targetType := reflect.TypeFor[T]()
	if err := c.canProvideValueLocked(targetType); err != nil {
		return err
	}

	c.registrations[targetType] = valueProvider{value: value}
	if protected {
		c.protected[targetType] = struct{}{}
	}
	c.order = append(c.order, targetType)

	// Cache in singletons immediately.
	c.singletons[targetType] = &singletonEntry{state: entryBuilt, value: value}

	return nil
}

// ProtectBinding prevents Replace from overwriting the existing direct
// registration for T. Calling it repeatedly is safe. When one expected value
// is supplied, protection succeeds only if the bound prebuilt value is the
// same comparable value. A mismatch adds no protection; protection already
// present on the binding remains in effect. [Container.AdoptValue] is the
// read-validate-protect form for integrations.
func (c *Container) ProtectBinding[T any](expected ...T) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	targetType := reflect.TypeFor[T]()
	if c.frozen {
		return frozenError("ProtectBinding", targetType)
	}
	if _, exists := c.registrations[targetType]; !exists {
		return fmt.Errorf("di: ProtectBinding[%s]: type is not registered", targetType)
	}
	if len(expected) > 1 {
		return fmt.Errorf("di: ProtectBinding[%s]: accepts at most one expected value", targetType)
	}
	if len(expected) == 1 {
		entry, exists := c.singletons[targetType]
		if !exists || entry.state != entryBuilt {
			return fmt.Errorf("di: ProtectBinding[%s]: expected value is not resolved", targetType)
		}
		matches, comparable := sameComparableValue(entry.value, any(expected[0]))
		if !comparable {
			return fmt.Errorf("di: ProtectBinding[%s]: expected value is not comparable", targetType)
		}
		if !matches {
			return fmt.Errorf("di: ProtectBinding[%s]: resolved value changed", targetType)
		}
	}
	c.protected[targetType] = struct{}{}
	return nil
}

func sameComparableValue(left, right any) (matches bool, comparable bool) {
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	if !leftValue.IsValid() || !rightValue.IsValid() {
		return !leftValue.IsValid() && !rightValue.IsValid(), true
	}
	if !leftValue.Comparable() || !rightValue.Comparable() {
		return false, false
	}
	if leftValue.Type() != rightValue.Type() {
		return false, true
	}
	return leftValue.Equal(rightValue), true
}

func (c *Container) canProvideValueLocked(targetType reflect.Type) error {
	if c.frozen {
		return frozenError("ProvideValue", targetType)
	}
	if _, exists := c.registrations[targetType]; exists {
		return fmt.Errorf("di: ProvideValue[%s]: already registered", targetType)
	}
	return nil
}

// MustProvideValue is like ProvideValue but panics on error.
func (c *Container) MustProvideValue[T any](value T) {
	if err := c.ProvideValue[T](value); err != nil {
		panic(err)
	}
}

var errorType = reflect.TypeFor[error]()

// inspectConstructor validates the constructor function signature and
// extracts parameter/return type information.
func inspectConstructor(constructor any, targetType reflect.Type) (*constructorProvider, error) {
	if constructor == nil {
		return nil, errors.New("constructor must not be nil")
	}

	cv := reflect.ValueOf(constructor)
	ct := cv.Type()

	if ct.Kind() != reflect.Func {
		return nil, fmt.Errorf("constructor must be a function, got %s", ct.Kind())
	}

	numOut := ct.NumOut()
	if numOut == 0 || numOut > 2 {
		return nil, fmt.Errorf("constructor must return 1 or 2 values, got %d", numOut)
	}

	// First return must be assignable to targetType.
	if !ct.Out(0).AssignableTo(targetType) {
		return nil, fmt.Errorf("first return type %s is not assignable to %s", ct.Out(0), targetType)
	}

	returnsError := false
	if numOut == 2 {
		if !ct.Out(1).Implements(errorType) {
			return nil, fmt.Errorf("second return type must implement error, got %s", ct.Out(1))
		}
		returnsError = true
	}

	// Cache parameter types.
	paramTypes := make([]reflect.Type, ct.NumIn())
	for i := range paramTypes {
		paramTypes[i] = ct.In(i)
	}

	return &constructorProvider{
		constructor:  cv,
		paramTypes:   paramTypes,
		resultType:   targetType,
		returnsError: returnsError,
	}, nil
}

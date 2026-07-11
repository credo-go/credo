// Adapted from github.com/samber/do (MIT License).

package di

import (
	"errors"
	"fmt"
	"reflect"
)

// registration holds the metadata for a registered service.
type registration struct {
	constructor  reflect.Value       // the constructor function (Provide)
	paramTypes   []reflect.Type      // cached constructor parameter types
	resultType   reflect.Type        // the T being registered
	returnsError bool                // true if constructor returns (T, error)
	isValue      bool                // true for ProvideValue (no constructor)
	value        any                 // pre-built value for ProvideValue
	funcCtor     func() (any, error) // typed factory adapter (ProvideFactory)
	protected    bool                // true when Replace must not overwrite this binding
}

// Provide registers a constructor for type T. The constructor can accept
// any number of parameters that are themselves registered in the container,
// and must return T or (T, error).
//
//	c.Provide[MyService](NewMyService)
func (c *Container) Provide[T any](constructor any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.frozen {
		return fmt.Errorf("di: Provide[%s]: container is frozen (container is sealed)", reflect.TypeFor[T]())
	}

	targetType := reflect.TypeFor[T]()

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

// ProvideFactory registers a compile-time-checked factory for type T.
// Unlike Provide, whose constructor is typed any and inspected via reflection
// at registration time, fn's signature is enforced by the compiler. fn runs
// lazily on first resolution, exactly once.
//
// fn is opaque to the container: dependencies it resolves internally are not
// visible to Seal's graph validation or to resolve-time cycle detection.
func (c *Container) ProvideFactory[T any](fn func() (T, error)) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	targetType := reflect.TypeFor[T]()

	if c.frozen {
		return fmt.Errorf("di: ProvideFactory[%s]: container is frozen (container is sealed)", targetType)
	}

	if fn == nil {
		return fmt.Errorf("di: ProvideFactory[%s]: factory must not be nil", targetType)
	}

	if _, exists := c.registrations[targetType]; exists {
		return fmt.Errorf("di: ProvideFactory[%s]: already registered", targetType)
	}

	c.registrations[targetType] = &registration{
		resultType: targetType,
		funcCtor:   func() (any, error) { return fn() },
	}
	c.order = append(c.order, targetType)

	// Pre-create singleton entry for later lazy resolution.
	c.singletons[targetType] = &singletonEntry{}

	return nil
}

// MustProvideFactory is like ProvideFactory but panics on error.
func (c *Container) MustProvideFactory[T any](fn func() (T, error)) {
	if err := c.ProvideFactory[T](fn); err != nil {
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

	reg := &registration{
		resultType: targetType,
		isValue:    true,
		value:      value,
		protected:  protected,
	}

	c.registrations[targetType] = reg
	c.order = append(c.order, targetType)

	// Cache in singletons immediately.
	entry := &singletonEntry{value: value}
	entry.done.Store(true)
	c.singletons[targetType] = entry

	return nil
}

// ProtectBinding prevents Replace from overwriting the existing direct
// registration for T. Calling it repeatedly is safe. When one expected value
// is supplied, protection succeeds only if the currently resolved singleton is
// the same comparable value. A mismatch adds no protection; protection already
// present on the binding remains in effect.
func (c *Container) ProtectBinding[T any](expected ...T) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	targetType := reflect.TypeFor[T]()
	if c.frozen {
		return fmt.Errorf("di: ProtectBinding[%s]: container is frozen (container is sealed)", targetType)
	}
	reg, exists := c.registrations[targetType]
	if !exists {
		return fmt.Errorf("di: ProtectBinding[%s]: type is not registered", targetType)
	}
	if len(expected) > 1 {
		return fmt.Errorf("di: ProtectBinding[%s]: accepts at most one expected value", targetType)
	}
	if len(expected) == 1 {
		entry, exists := c.singletons[targetType]
		if !exists || !entry.done.Load() || entry.err != nil {
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
	reg.protected = true
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
		return fmt.Errorf("di: ProvideValue[%s]: container is frozen (container is sealed)", targetType)
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
func inspectConstructor(constructor any, targetType reflect.Type) (*registration, error) {
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

	return &registration{
		constructor:  cv,
		paramTypes:   paramTypes,
		resultType:   targetType,
		returnsError: returnsError,
	}, nil
}

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
}

// Provide registers a constructor for type T. The constructor can accept
// any number of parameters that are themselves registered in the container,
// and must return T or (T, error).
//
//	di.Provide[MyService](c, NewMyService)
func Provide[T any](c *Container, constructor any) error {
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
func MustProvide[T any](c *Container, constructor any) {
	if err := Provide[T](c, constructor); err != nil {
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
func ProvideFactory[T any](c *Container, fn func() (T, error)) error {
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
func MustProvideFactory[T any](c *Container, fn func() (T, error)) {
	if err := ProvideFactory[T](c, fn); err != nil {
		panic(err)
	}
}

// ProvideValue registers a pre-built value for type T as a Singleton.
// The value is cached immediately.
func ProvideValue[T any](c *Container, value T) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.frozen {
		return fmt.Errorf("di: ProvideValue[%s]: container is frozen (container is sealed)", reflect.TypeFor[T]())
	}

	targetType := reflect.TypeFor[T]()

	if _, exists := c.registrations[targetType]; exists {
		return fmt.Errorf("di: ProvideValue[%s]: already registered", targetType)
	}

	reg := &registration{
		resultType: targetType,
		isValue:    true,
		value:      value,
	}

	c.registrations[targetType] = reg
	c.order = append(c.order, targetType)

	// Cache in singletons immediately.
	entry := &singletonEntry{value: value}
	entry.done.Store(true)
	c.singletons[targetType] = entry

	return nil
}

// MustProvideValue is like ProvideValue but panics on error.
func MustProvideValue[T any](c *Container, value T) {
	if err := ProvideValue[T](c, value); err != nil {
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

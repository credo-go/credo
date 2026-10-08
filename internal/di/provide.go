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
// resolution after Seal. Every rejection is a [*MisuseError].
//
//	c.Provide[MyService](NewMyService)
func (c *Container) Provide[T any](constructor any) error {
	return c.ProvideWith[T](constructor, Options{})
}

// ProvideWith is [Container.Provide] with registration options.
func (c *Container) ProvideWith[T any](constructor any, o Options) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	targetType := reflect.TypeFor[T]()
	types := []reflect.Type{targetType}
	if err := c.closedLocked("Provide", targetType); err != nil {
		return err
	}

	reg, err := inspectConstructor(constructor, targetType)
	if err != nil {
		return misuse("Provide", types, "%v; want func(dependencies...) %s or "+
			"func(dependencies...) (%s, error)", err, targetType, targetType)
	}
	if o.Borrowed {
		return misuse("Provide", types, "credo.Borrowed() is accepted on ProvideValue only: a value a "+
			"constructor builds belongs to the App")
	}
	if err := c.checkOverrideLocked("Provide", targetType, o.Override); err != nil {
		return err
	}
	u := &Unit{t: targetType, name: targetType.String()}
	if reason := planUnit(u, o); reason != "" {
		return misuse("Provide", types, "%s", reason)
	}

	// Pre-create singleton entry for later lazy resolution.
	c.bindLocked(u, reg, &singletonEntry{})
	return nil
}

// checkOverrideLocked enforces that a second binding of one type is an
// override and that an override has a binding to replace.
func (c *Container) checkOverrideLocked(op string, t reflect.Type, override bool) error {
	_, exists := c.registrations[t]
	switch {
	case exists && !override:
		return duplicateError(op, t)
	case !exists && override:
		return misuse(op, []reflect.Type{t}, "credo.Override() replaces an earlier binding, but %s has none; "+
			"bind %s first, or drop credo.Override()", t, t)
	}
	return nil
}

// bindLocked publishes a binding unit. An override takes the replaced
// binding's place in registration order, and the value it replaces never
// becomes the App's. c.mu must be held.
func (c *Container) bindLocked(u *Unit, reg provider, entry *singletonEntry) {
	if old, ok := c.unitOf[u.t]; ok {
		u.index = old.index
		c.units[old.index] = u
		c.forgetHolderLocked(old)
	} else {
		u.index = len(c.units)
		c.units = append(c.units, u)
		c.order = append(c.order, u.t)
	}
	c.unitOf[u.t] = u
	c.registrations[u.t] = reg
	c.singletons[u.t] = entry
}

// MustProvide is like Provide but panics on error.
func (c *Container) MustProvide[T any](constructor any) {
	if err := c.Provide[T](constructor); err != nil {
		panic(err)
	}
}

// ProvideValue registers a pre-built value for type T as a Singleton.
// The value is cached immediately. Every rejection is a [*MisuseError].
func (c *Container) ProvideValue[T any](value T) error {
	return c.provideValue("ProvideValue", value, Options{})
}

// ProvideValueWith is [Container.ProvideValue] with registration options.
func (c *Container) ProvideValueWith[T any](value T, o Options) error {
	return c.provideValue("ProvideValue", value, o)
}

func (c *Container) provideValue[T any](op string, value T, o Options) error {
	targetType := reflect.TypeFor[T]()
	types := []reflect.Type{targetType}
	// The identity runs user code, so it is taken before the lock.
	token, hasToken, idErr := identityToken(any(value))

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.closedLocked(op, targetType); err != nil {
		return err
	}
	if err := c.checkOverrideLocked(op, targetType, o.Override); err != nil {
		return err
	}
	u := &Unit{t: targetType, name: targetType.String(), valueBinding: true}
	if reason := planUnit(u, o); reason != "" {
		return misuse(op, types, "%s", reason)
	}
	if idErr != nil {
		return misuse(op, types, "%v", idErr)
	}
	if o.Override {
		c.forgetHolderLocked(c.unitOf[targetType])
	}
	if err := c.admitValueLocked(u, any(value), token, hasToken); err != nil {
		return misuse(op, types, "%v", err)
	}

	c.bindLocked(u, valueProvider{value: value}, &singletonEntry{state: entryBuilt, value: value})
	return nil
}

// duplicateError rejects a second binding of one type. Two instances of one
// type are bound as two wrapper types, so each consumer names the one it
// needs in its constructor.
func duplicateError(op string, targetType reflect.Type) error {
	return misuse(op, []reflect.Type{targetType}, "%s is already registered; bind a second instance "+
		"under a wrapper type of its own", targetType)
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

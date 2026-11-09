// Adapted from github.com/samber/do (MIT License).

package di

import (
	"errors"
	"fmt"
	"reflect"
)

// Provide registers a constructor. The binding's type is the constructor's
// first result type: func(dependencies...) T or func(dependencies...)
// (T, error) binds T. The constructor runs at most once, on the first
// resolution after Seal. Every rejection is a [*MisuseError].
//
//	c.Provide(NewMyService)
func (c *Container) Provide(constructor any) error {
	return c.ProvideWith(constructor, Options{})
}

// ProvideWith is [Container.Provide] with registration options.
func (c *Container) ProvideWith(constructor any, o Options) error {
	reg, err := inspectConstructor(constructor)
	if err != nil {
		var types []reflect.Type
		if ct := reflect.TypeOf(constructor); ct != nil && ct.Kind() == reflect.Func && ct.NumOut() > 0 {
			types = []reflect.Type{ct.Out(0)}
		}
		return misuse("Provide", types, "%v; want func(dependencies...) T or func(dependencies...) (T, error), "+
			"which binds T", err)
	}
	targetType := reg.resultType
	types := []reflect.Type{targetType}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.closedLocked("Provide", targetType); err != nil {
		return err
	}
	if o.Borrowed {
		return misuse("Provide", types, "credo.Borrowed() is accepted on ProvideValue only: a value a "+
			"constructor builds belongs to the App")
	}
	if err := c.checkOverrideLocked("Provide", targetType, o); err != nil {
		return err
	}
	u := newBindingUnit(targetType, o)
	if reason := planUnit(u, o); reason != "" {
		return misuse("Provide", types, "%s", reason)
	}
	// The value does not exist yet: a name shared with a registration whose
	// resource is unknown is checked when the value is built.
	if err := c.nameConflictLocked(u, nil); err != nil {
		return misuse("Provide", types, "%v", err)
	}

	// Pre-create singleton entry for later lazy resolution.
	c.replaceAliasLocked("Provide", targetType)
	c.bindLocked(u, reg, &singletonEntry{})
	return nil
}

// checkOverrideLocked enforces that a second binding of one type is an
// override and that an override has a binding to replace. An alias is a
// binding of its interface: a direct binding after it is a duplicate, and an
// override replaces it.
func (c *Container) checkOverrideLocked(op string, t reflect.Type, o Options) error {
	override := o.Override
	_, exists := c.registrations[t]
	target, aliased := c.aliases[t]
	switch {
	case exists && !override:
		name := o.Name
		if old := c.unitOf[t]; name == "" && old != nil && old.nameGiven {
			name = old.name
		}
		return duplicateError(op, t, name)
	case aliased && !override:
		return misuse(op, []reflect.Type{t}, "%s is already bound through app.Alias[%s, %s](); give "+
			"credo.Override() to replace the alias, or drop the Alias call", t, t, target)
	case !exists && !aliased && override:
		return misuse(op, []reflect.Type{t}, "credo.Override() replaces an earlier binding, but %s has none; "+
			"bind %s first, or drop credo.Override()", t, t)
	}
	return nil
}

// replaceAliasLocked removes the alias of t that an override replaces. The
// type it pointed at stays registered until Seal, which drops it when nothing
// else reaches it. c.mu must be held.
func (c *Container) replaceAliasLocked(op string, t reflect.Type) {
	target, ok := c.aliases[t]
	if !ok {
		return
	}
	delete(c.aliases, t)
	c.noteUnreachedLocked(target, fmt.Sprintf("%s[%s] with credo.Override() replaced app.Alias[%s, %s]()",
		op, t, t, target))
}

// bindLocked publishes a binding unit. An override takes the replaced
// binding's place in registration order, and the value it replaces never
// becomes the App's. c.mu must be held.
func (c *Container) bindLocked(u *Unit, reg provider, entry *singletonEntry) {
	if old, ok := c.unitOf[u.t]; ok {
		u.index = old.index
		c.units[old.index] = u
		c.forgetHolderLocked(old)
		c.forgetNameLocked(old)
	} else {
		u.index = len(c.units)
		c.units = append(c.units, u)
		c.order = append(c.order, u.t)
	}
	c.unitOf[u.t] = u
	c.registrations[u.t] = reg
	c.singletons[u.t] = entry
	if u.nameGiven {
		c.names[u.name] = append(c.names[u.name], u)
	}
}

// newBindingUnit returns the unit of a binding of t, named by its
// credo.ComponentName name or its type.
func newBindingUnit(t reflect.Type, o Options) *Unit {
	u := &Unit{t: t, name: t.String()}
	if o.Name != "" {
		u.name, u.nameGiven = o.Name, true
	}
	return u
}

// MustProvide is like Provide but panics on error.
func (c *Container) MustProvide(constructor any) {
	if err := c.Provide(constructor); err != nil {
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
	if err := c.checkOverrideLocked(op, targetType, o); err != nil {
		return err
	}
	u := newBindingUnit(targetType, o)
	u.valueBinding = true
	if reason := planUnit(u, o); reason != "" {
		return misuse(op, types, "%s", reason)
	}
	if idErr != nil {
		return misuse(op, types, "%v", idErr)
	}
	if err := c.hiddenCapabilityErrorLocked(u, any(value), token, hasToken); err != nil {
		return misuse(op, types, "%v", err)
	}
	if o.Override {
		c.forgetHolderLocked(c.unitOf[targetType])
	}
	// The registration order the admission compares holders by: an override
	// takes the replaced binding's place.
	u.index = len(c.units)
	if old, ok := c.unitOf[targetType]; ok {
		u.index = old.index
	}
	if err := c.admitValueLocked(u, any(value), token, hasToken); err != nil {
		return misuse(op, types, "%v", err)
	}

	c.replaceAliasLocked(op, targetType)
	c.bindLocked(u, valueProvider{value: value}, &singletonEntry{state: entryBuilt, value: value})
	return nil
}

// duplicateError rejects a second binding of one type. Two instances of one
// type are bound as two wrapper types, so each consumer names the one it
// needs in its constructor. name is the credo.ComponentName name either
// registration carries, which reads as a DI key to some.
func duplicateError(op string, targetType reflect.Type, name string) error {
	if name != "" {
		return misuse(op, []reflect.Type{targetType}, "%s is already registered; credo.ComponentName(%q) "+
			"names a component in logs, /ready and lifecycle errors and is not a DI key; bind a second "+
			"instance under a wrapper type of its own", targetType, name)
	}
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
// extracts parameter/return type information. The result type is the
// constructor's first result type.
func inspectConstructor(constructor any) (*constructorProvider, error) {
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
		resultType:   ct.Out(0),
		returnsError: returnsError,
	}, nil
}

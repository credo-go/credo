// Adapted from github.com/samber/do (MIT License).

package di

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Resolve retrieves an instance of type T from the container.
//
//	svc, err := di.Resolve[MyService](c)
func Resolve[T any](c *Container) (T, error) {
	var zero T
	targetType := reflect.TypeFor[T]()

	result, err := c.resolve(targetType, nil)
	if err != nil {
		return zero, err
	}

	v, ok := result.(T)
	if !ok {
		return zero, fmt.Errorf("di: Resolve[%s]: type assertion failed", targetType)
	}
	return v, nil
}

// MustResolve is like Resolve but panics on error.
func MustResolve[T any](c *Container) T {
	v, err := Resolve[T](c)
	if err != nil {
		panic(err)
	}
	return v
}

// ResolveAll retrieves all singleton instances bound to interface type T via
// BindMany, preserving binding order. When no bindings exist, it returns an
// empty slice and nil error.
func ResolveAll[T any](c *Container) ([]T, error) {
	targetType := reflect.TypeFor[T]()
	if targetType.Kind() != reflect.Interface {
		return nil, fmt.Errorf("di: ResolveAll[%s]: type parameter must be an interface", targetType)
	}

	result, err := c.resolveMany(targetType, nil)
	if err != nil {
		return nil, err
	}

	v, ok := result.Interface().([]T)
	if !ok {
		return nil, fmt.Errorf("di: ResolveAll[%s]: type assertion failed", targetType)
	}
	return v, nil
}

// MustResolveAll is like ResolveAll but panics on error.
func MustResolveAll[T any](c *Container) []T {
	v, err := ResolveAll[T](c)
	if err != nil {
		panic(err)
	}
	return v
}

// resolve is the internal resolution engine.
func (c *Container) resolve(targetType reflect.Type, stack []reflect.Type) (any, error) {
	// If Seal was called and failed, reject all resolves. sealErr is read
	// under the lock because doSeal may write it concurrently (Resolve is
	// permitted before and after Seal, and Run seals implicitly).
	c.mu.RLock()
	sealErr := c.sealErr
	c.mu.RUnlock()
	if sealErr != nil {
		return nil, fmt.Errorf("di: Resolve[%s]: container seal failed: %w", targetType, sealErr)
	}

	reg, canonical, ok := c.findRegistration(targetType)
	if !ok {
		return nil, fmt.Errorf("di: Resolve[%s]: not registered", targetType)
	}

	// Cycle detection uses canonical (concrete) type so aliases don't
	// bypass the check.
	if slices.Contains(stack, canonical) {
		return nil, fmt.Errorf("di: circular dependency detected: %s", formatCycle(stack, canonical))
	}

	return c.resolveSingleton(reg, canonical, stack)
}

func (c *Container) resolveMany(targetType reflect.Type, stack []reflect.Type) (reflect.Value, error) {
	c.mu.RLock()
	sealErr := c.sealErr
	c.mu.RUnlock()
	if sealErr != nil {
		return reflect.Value{}, fmt.Errorf("di: ResolveAll[%s]: container seal failed: %w", targetType, sealErr)
	}

	if targetType.Kind() != reflect.Interface {
		return reflect.Value{}, fmt.Errorf("di: ResolveAll[%s]: type parameter must be an interface", targetType)
	}

	bindings := c.collectionBindings(targetType)
	result := reflect.MakeSlice(reflect.SliceOf(targetType), 0, len(bindings))
	for _, concreteType := range bindings {
		val, err := c.resolve(concreteType, stack)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("di: ResolveAll[%s]: binding %s: %w", targetType, concreteType, err)
		}

		rv := reflect.ValueOf(val)
		if !rv.Type().AssignableTo(targetType) {
			return reflect.Value{}, fmt.Errorf("di: ResolveAll[%s]: binding %s is not assignable to %s",
				targetType, concreteType, targetType)
		}

		result = reflect.Append(result, rv)
	}

	return result, nil
}

// resolveSingleton resolves a Singleton service. Per-type mutex ensures
// concurrent resolution of different singletons without contention.
func (c *Container) resolveSingleton(reg *registration, targetType reflect.Type, stack []reflect.Type) (any, error) {
	// Fast path: check if already resolved without locking the map.
	c.mu.RLock()
	entry, ok := c.singletons[targetType]
	c.mu.RUnlock()

	if ok && entry.done.Load() {
		return entry.value, entry.err
	}

	if !ok {
		// Lazy-create entry (shouldn't happen if Provide was called correctly).
		c.mu.Lock()
		entry, ok = c.singletons[targetType]
		if !ok {
			entry = &singletonEntry{}
			c.singletons[targetType] = entry
		}
		c.mu.Unlock()
	}

	// sync.Once ensures the constructor runs exactly once, even under
	// concurrent access. No data race on entry.value/err because Once
	// provides a happens-before guarantee.
	entry.once.Do(func() {
		entry.value, entry.err = c.construct(reg, append(stack, targetType))
		entry.done.Store(true)
	})

	return entry.value, entry.err
}

// construct builds a new instance from a registration.
func (c *Container) construct(reg *registration, stack []reflect.Type) (any, error) {
	if reg.isValue {
		return reg.value, nil
	}

	// ProvideFunc constructors are opaque: no parameter injection, and any
	// Resolve calls inside fn start a fresh cycle-detection stack.
	if reg.funcCtor != nil {
		instance, err := reg.funcCtor()
		if err != nil {
			return nil, fmt.Errorf("di: constructing %s: %w", reg.resultType, err)
		}
		return instance, nil
	}

	serviceName := deriveServiceName(reg.resultType)

	// Build parameters.
	params := make([]reflect.Value, len(reg.paramTypes))
	for i, pt := range reg.paramTypes {
		param, err := c.resolveParamValue(pt, serviceName, stack)
		if err != nil {
			return nil, fmt.Errorf("di: constructing %s: param %d (%s): %w", reg.resultType, i, pt, err)
		}
		params[i] = param
	}

	// Call constructor.
	results := reg.constructor.Call(params)

	// Extract result.
	instance := results[0].Interface()

	// Check error return.
	if reg.returnsError {
		if errVal := results[1].Interface(); errVal != nil {
			return nil, fmt.Errorf("di: constructing %s: %w", reg.resultType, errVal.(error))
		}
	}

	return instance, nil
}

func (c *Container) resolveParamValue(paramType reflect.Type, serviceName string, stack []reflect.Type) (reflect.Value, error) {
	if c.isInfraType(paramType) {
		infra := c.infraProvider.Factory(serviceName)
		return reflect.ValueOf(infra), nil
	}

	if isInterfaceSlice(paramType) && !c.hasDirectRegistration(paramType) {
		return c.resolveMany(paramType.Elem(), stack)
	}

	val, err := c.resolve(paramType, stack)
	if err != nil {
		return reflect.Value{}, err
	}

	return reflect.ValueOf(val), nil
}

// formatCycle produces a human-readable cycle description.
func formatCycle(stack []reflect.Type, target reflect.Type) string {
	var b strings.Builder
	started := false
	for _, t := range stack {
		if t == target {
			started = true
		}
		if started {
			b.WriteString(t.String())
			b.WriteString(" → ")
		}
	}
	b.WriteString(target.String())
	return b.String()
}

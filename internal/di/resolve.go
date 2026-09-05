// Adapted from github.com/samber/do (MIT License).

package di

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/credo-go/credo/internal/observe"
)

// panicStackSize bounds the stack captured into a PanicError.
const panicStackSize = 8192

// Resolve retrieves an instance of type T from the container. It is admitted
// only after Seal; during shutdown it returns an error wrapping [ErrClosed].
//
//	svc, err := c.Resolve[MyService]()
func (c *Container) Resolve[T any]() (T, error) {
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

// MustResolve is like Resolve but panics on error. A constructor panic is
// reported as a *PanicError, so the panic payload here is that error rather
// than the original value.
func (c *Container) MustResolve[T any]() T {
	v, err := c.Resolve[T]()
	if err != nil {
		panic(err)
	}
	return v
}

// ResolveAll retrieves all singleton instances bound to interface type T via
// BindMany, preserving binding order. When no bindings exist, it returns an
// empty slice and nil error.
func (c *Container) ResolveAll[T any]() ([]T, error) {
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
func (c *Container) MustResolveAll[T any]() []T {
	v, err := c.ResolveAll[T]()
	if err != nil {
		panic(err)
	}
	return v
}

// admitResolve applies the phase rules shared by every resolution entry:
// closing wins over everything, then Seal must have run, then a failed Seal
// rejects. op names the API for the error text.
func (c *Container) admitResolve(op string, targetType reflect.Type) error {
	c.mu.RLock()
	closing, sealed, sealErr := c.closing, c.sealed, c.sealErr
	c.mu.RUnlock()
	switch {
	case closing:
		return fmt.Errorf("di: %s[%s]: %w", op, targetType, ErrClosed)
	case !sealed:
		return fmt.Errorf("di: %s[%s]: container is not finalized (call Finalize before resolving)", op, targetType)
	case sealErr != nil:
		return fmt.Errorf("di: %s[%s]: container seal failed: %w", op, targetType, sealErr)
	}
	return nil
}

// resolve is the internal resolution engine.
func (c *Container) resolve(targetType reflect.Type, stack []reflect.Type) (any, error) {
	if err := c.admitResolve("Resolve", targetType); err != nil {
		return nil, err
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
	if err := c.admitResolve("ResolveAll", targetType); err != nil {
		return reflect.Value{}, err
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

// resolveSingleton resolves a Singleton service. Each type has one entry whose
// state moves unbuilt → building → built|failed exactly once; concurrent
// callers of a building entry wait for that single completion. State changes
// happen under c.mu, the constructor runs outside it.
func (c *Container) resolveSingleton(reg provider, targetType reflect.Type, stack []reflect.Type) (any, error) {
	c.mu.Lock()
	entry, ok := c.singletons[targetType]
	if !ok {
		// Lazy-create entry (shouldn't happen if Provide was called correctly).
		entry = &singletonEntry{}
		c.singletons[targetType] = entry
	}
	switch entry.state {
	case entryBuilt, entryFailed:
		v, err := c.deliverLocked(targetType, entry)
		c.mu.Unlock()
		return v, err
	case entryBuilding:
		done := entry.done
		c.mu.Unlock()
		<-done
		c.mu.Lock()
		v, err := c.deliverLocked(targetType, entry)
		c.mu.Unlock()
		return v, err
	}

	// entryUnbuilt: admit a build, unless the container is closing.
	if c.closing {
		c.mu.Unlock()
		return nil, fmt.Errorf("di: Resolve[%s]: %w", targetType, ErrClosed)
	}
	entry.state = entryBuilding
	entry.done = make(chan struct{})
	entry.buildStart = time.Now()
	c.mu.Unlock()

	value, err := c.build(reg, targetType, stack)

	c.mu.Lock()
	entry.buildDuration = time.Since(entry.buildStart)
	if err != nil {
		entry.state = entryFailed
		entry.err = err
	} else {
		entry.state = entryBuilt
		entry.value = value
	}
	// A completion after the shutdown context ended is no longer the shutdown
	// pass's to own: route it to the separate best-effort cleanup.
	late := c.closing && (c.teardownDone || c.shutdownCtx.Err() != nil)
	if late {
		entry.late = true
	}
	close(entry.done)
	c.notifyBuildDone()
	v, derr := c.deliverLocked(targetType, entry)
	c.mu.Unlock()

	if late {
		go c.lateCleanup(targetType, entry.state, value, err)
	}
	return v, derr
}

// deliverLocked returns an entry's terminal result to a caller, or the closing
// sentinel once shutdown has begun. A withheld instance is still tracked for
// cleanup; a withheld construction failure stays recorded for diagnostics.
func (c *Container) deliverLocked(targetType reflect.Type, entry *singletonEntry) (any, error) {
	if c.closing {
		return nil, fmt.Errorf("di: Resolve[%s]: %w", targetType, ErrClosed)
	}
	return entry.value, entry.err
}

// build runs the provider with panic recovery. A constructor panic becomes a
// terminal *PanicError carrying the original value and the stack of this
// goroutine; it is never retried.
func (c *Container) build(reg provider, targetType reflect.Type, stack []reflect.Type) (value any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			value = nil
			err = &PanicError{
				Type:  targetType,
				Phase: PhaseConstruction,
				Value: recovered,
				Stack: observe.StackTrace(panicStackSize),
			}
		}
	}()
	return reg.build(c, append(stack, targetType))
}

// notifyBuildDone wakes the shutdown pass without blocking the completing
// goroutine; a pending wake-up is coalesced.
func (c *Container) notifyBuildDone() {
	select {
	case c.buildDone <- struct{}{}:
	default:
	}
}

func (c *Container) resolveParamValue(paramType reflect.Type, serviceName string, stack []reflect.Type) (reflect.Value, error) {
	if fp, ok := c.frameworkProvider(paramType); ok {
		return reflect.ValueOf(fp.Factory(serviceName)), nil
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

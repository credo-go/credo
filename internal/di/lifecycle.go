// Adapted from github.com/samber/do (MIT License).

package di

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

var contextType = reflect.TypeFor[context.Context]()

// shutdowner is implemented by services that need cleanup on shutdown.
// This mirrors the public credo.Shutdowner interface. Structural typing
// ensures any type implementing credo.Shutdowner also satisfies this.
type shutdowner interface {
	Shutdown(ctx context.Context) error
}

// validate checks the container's dependency graph for errors:
//   - Missing dependencies (constructor param not registered)
//   - Circular dependencies (A → B → A)
//   - context.Context parameters (not allowed)
func (c *Container) validate() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var errs []error

	for t, reg := range c.registrations {
		if reg.isValue {
			continue // pre-built values have no constructor params
		}

		for i, pt := range reg.paramTypes {
			// context.Context is not allowed as a constructor parameter.
			if pt == contextType {
				errs = append(errs, fmt.Errorf(
					"di: Validate: %s (param %d): context.Context parameter is not allowed in constructors",
					t, i,
				))
				continue
			}

			// Infra type is framework-produced, not registered.
			if c.isInfraType(pt) {
				continue
			}

			// Slice-of-interface parameters can be populated from BindMany.
			// Empty collections are valid when no bindings exist, so the
			// parameter is always satisfiable regardless of whether the slice
			// type itself is registered.
			if isInterfaceSlice(pt) {
				continue
			}

			if _, ok := c.registrations[pt]; !ok {
				// Check aliases.
				if concrete, aliased := c.aliases[pt]; aliased {
					if _, ok := c.registrations[concrete]; ok {
						continue
					}
				}
				errs = append(errs, fmt.Errorf(
					"di: Validate: %s (param %d): dependency %s is not registered",
					t, i, pt,
				))
			}
		}
	}

	// Validate aliases: concrete types must be registered.
	for iface, concrete := range c.aliases {
		if _, ok := c.registrations[concrete]; !ok {
			errs = append(errs, fmt.Errorf(
				"di: Validate: alias %s → %s: concrete type is not registered",
				iface, concrete,
			))
		}
	}

	// Validate BindMany collections.
	for iface, concretes := range c.manyBindings {
		if iface.Kind() != reflect.Interface {
			errs = append(errs, fmt.Errorf(
				"di: Validate: BindMany target %s: target type must be an interface",
				iface,
			))
			continue
		}

		for _, concrete := range concretes {
			if concrete.Kind() == reflect.Interface {
				errs = append(errs, fmt.Errorf(
					"di: Validate: BindMany %s → %s: concrete type must not be an interface",
					iface, concrete,
				))
				continue
			}

			if !concrete.Implements(iface) {
				errs = append(errs, fmt.Errorf(
					"di: Validate: BindMany %s → %s: concrete type does not implement interface",
					iface, concrete,
				))
			}

			if _, ok := c.registrations[concrete]; !ok {
				errs = append(errs, fmt.Errorf(
					"di: Validate: BindMany %s → %s: concrete type is not registered",
					iface, concrete,
				))
			}
		}
	}

	// DFS cycle detection across the entire graph.
	if err := c.detectCycles(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// detectCycles performs DFS across all registrations to find cycles.
func (c *Container) detectCycles() error {
	const (
		white = 0 // unvisited
		gray  = 1 // in-progress
		black = 2 // done
	)

	colors := make(map[reflect.Type]int, len(c.registrations))
	var path []reflect.Type

	var visit func(t reflect.Type) error
	visit = func(t reflect.Type) error {
		colors[t] = gray
		path = append(path, t)

		reg, ok := c.registrations[t]
		if ok && !reg.isValue {
			for _, pt := range reg.paramTypes {
				if pt == contextType {
					continue
				}
				if c.isInfraType(pt) {
					continue
				}

				deps := c.cycleDependenciesForParam(pt)
				for _, dep := range deps {
					switch colors[dep] {
					case gray:
						return fmt.Errorf("di: Validate: circular dependency: %s", formatCycle(path, dep))
					case white:
						if err := visit(dep); err != nil {
							return err
						}
					}
				}
			}
		}

		path = path[:len(path)-1]
		colors[t] = black
		return nil
	}

	for t := range c.registrations {
		if colors[t] == white {
			if err := visit(t); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Container) cycleDependenciesForParam(paramType reflect.Type) []reflect.Type {
	if _, ok := c.registrations[paramType]; ok {
		return []reflect.Type{paramType}
	}

	if isInterfaceSlice(paramType) {
		bindings := c.manyBindings[paramType.Elem()]
		deps := make([]reflect.Type, 0, len(bindings))
		for _, concrete := range bindings {
			if _, ok := c.registrations[concrete]; ok {
				deps = append(deps, concrete)
			}
		}
		return deps
	}

	if concrete, aliased := c.aliases[paramType]; aliased {
		if _, ok := c.registrations[concrete]; ok {
			return []reflect.Type{concrete}
		}
	}

	return nil
}

// Shutdown gracefully shuts down all cached singletons that implement
// Shutdowner, in reverse registration order. The context carries the
// shutdown deadline — services should respect ctx.Done() for timely cleanup.
func (c *Container) Shutdown(ctx context.Context) error {
	c.mu.RLock()
	order := make([]reflect.Type, len(c.order))
	copy(order, c.order)
	c.mu.RUnlock()

	var errs []error

	// Reverse order.
	for i := len(order) - 1; i >= 0; i-- {
		// Once the shutdown deadline passes, calling further Shutdowners is
		// pointless at best (well-behaved ones fail fast on ctx) and can hang
		// the process at worst — stop and report what was skipped.
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("di: shutdown aborted with %d registration(s) left: %w", i+1, err))
			break
		}
		t := order[i]
		c.mu.RLock()
		entry, ok := c.singletons[t]
		c.mu.RUnlock()
		if !ok || !entry.done.Load() || entry.err != nil {
			continue
		}

		if s, ok := entry.value.(shutdowner); ok {
			if err := s.Shutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("di: shutting down %s: %w", t, err))
			}
		}
	}

	return errors.Join(errs...)
}

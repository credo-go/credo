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
//
// Aliases and BindMany collections are not checked again here: Alias and
// BindMany reject a binding whose types do not fit when it is made, and no
// registration is ever removed, so an alias always names a registered type
// and a collection holds registered implementations of its interface.
//
// Every walk follows registration order, never a map, so the same wiring
// yields the same report on every run: the errors in the order their
// subjects were registered, and of several cycles the one reached first from
// the earliest registration.
func (c *Container) validate() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var errs []error

	for _, t := range c.order {
		reg := c.registrations[t]
		for i, pt := range reg.deps() {
			// context.Context is not allowed as a constructor parameter.
			if pt == contextType {
				errs = append(errs, fmt.Errorf(
					"di: Validate: %s (param %d): context.Context parameter is not allowed in constructors",
					t, i,
				))
				continue
			}

			// Framework-produced parameters (credo.Infra) are not registered.
			if c.isFrameworkType(pt) {
				continue
			}

			// Slice-of-interface parameters can be populated from BindMany.
			// Empty collections are valid when no bindings exist, so the
			// parameter is always satisfiable regardless of whether the slice
			// type itself is registered.
			if isInterfaceSlice(pt) {
				continue
			}

			if _, ok := c.registrations[pt]; ok {
				continue
			}
			// An alias always names a registered type (see above).
			if _, aliased := c.aliases[pt]; aliased {
				continue
			}
			errs = append(errs, fmt.Errorf(
				"di: Validate: %s (param %d): dependency %s is not registered",
				t, i, pt,
			))
		}
	}

	// DFS cycle detection across the entire graph.
	if err := c.detectCycles(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// detectCycles performs DFS across all registrations to find cycles. It
// starts from the registrations in registration order: the start decides
// which cycle is found and at which member its text begins.
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

		if reg, ok := c.registrations[t]; ok {
			for _, pt := range reg.deps() {
				if pt == contextType {
					continue
				}
				if c.isFrameworkType(pt) {
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

	for _, t := range c.order {
		if colors[t] == white {
			if err := visit(t); err != nil {
				return err
			}
		}
	}
	return nil
}

// cycleDependenciesForParam resolves one constructor parameter to the
// registered types construction would use: the direct registration, the
// BindMany collection of an interface slice, or the alias target. The cycle
// scan and the teardown graph share it so validation and shutdown ordering
// never disagree about an edge.
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

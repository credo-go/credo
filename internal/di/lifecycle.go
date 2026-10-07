// Adapted from github.com/samber/do (MIT License).

package di

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

var contextType = reflect.TypeFor[context.Context]()

// shutdowner is implemented by services that need cleanup on shutdown.
// This mirrors the public credo.Shutdowner interface. Structural typing
// ensures any type implementing credo.Shutdowner also satisfies this.
type shutdowner interface {
	Shutdown(ctx context.Context) error
}

// validate checks the container's dependency graph and reports every problem
// it finds, not the first:
//   - a missing dependency (a constructor parameter that nothing registers),
//     with its whole path from the registration the walk started at;
//   - each circular dependency (A → B → A);
//   - context.Context parameters, which constructors cannot take.
//
// Aliases and BindMany collections are not checked again here: Alias and
// BindMany reject a binding whose types do not fit when it is made, and no
// registration is ever removed, so an alias always names a registered type
// and a collection holds registered implementations of its interface.
//
// The walk starts at the registrations nothing depends on, the graph's entry
// points, so a path reads from what the application asked for down to what is
// missing; registrations only a cycle reaches follow. Each walk follows
// registration order, never a map, and the findings are sorted by the
// registration they belong to — a path's first element, a cycle's earliest
// registered member, which its text starts at — so the same wiring yields the
// same report on every run.
func (c *Container) validate() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	index := make(map[reflect.Type]int, len(c.order))
	for i, t := range c.order {
		index[t] = i
	}

	// Entry points first, then what only a cycle reaches.
	depended := make(map[reflect.Type]bool, len(c.order))
	for _, t := range c.order {
		for _, pt := range c.registrations[t].deps() {
			if pt == contextType || c.isFrameworkType(pt) {
				continue
			}
			for _, dep := range c.cycleDependenciesForParam(pt) {
				depended[dep] = true
			}
		}
	}
	starts := make([]reflect.Type, 0, len(c.order))
	for _, t := range c.order {
		if !depended[t] {
			starts = append(starts, t)
		}
	}
	for _, t := range c.order {
		if depended[t] {
			starts = append(starts, t)
		}
	}

	type finding struct {
		at  int
		err error
	}
	var findings []finding

	const (
		white = 0 // unvisited
		gray  = 1 // on the current path
		black = 2 // done
	)
	colors := make(map[reflect.Type]int, len(c.order))
	var path []pathStep

	var visit func(step pathStep)
	visit = func(step pathStep) {
		colors[step.t] = gray
		path = append(path, step)

		for i, pt := range c.registrations[step.t].deps() {
			if pt == contextType {
				findings = append(findings, finding{index[path[0].t], fmt.Errorf(
					"di: %s (param %d): context.Context parameter is not allowed in constructors; "+
						"take the context in the methods that need it", step.t, i)})
				continue
			}
			// Framework-produced parameters (credo.Infra) are not registered.
			if c.isFrameworkType(pt) {
				continue
			}

			deps := c.cycleDependenciesForParam(pt)
			// A slice of an interface is satisfied by its BindMany
			// collection, which may be empty.
			if len(deps) == 0 && !isInterfaceSlice(pt) {
				findings = append(findings, finding{index[path[0].t], fmt.Errorf(
					"di: missing dependency: %s → %s (not registered); provide %s before Finalize",
					formatPath(path), pt, pt)})
				continue
			}
			for _, dep := range deps {
				next := pathStep{t: dep, via: pt}
				switch colors[dep] {
				case gray:
					members := cycleMembers(path, next)
					first := 0
					for k, m := range members {
						if index[m.t] < index[members[first].t] {
							first = k
						}
					}
					findings = append(findings, finding{index[members[first].t], fmt.Errorf(
						"di: circular dependency: %s; remove one of these constructor parameters",
						formatGraphCycle(members, first))})
				case white:
					visit(next)
				}
			}
		}

		path = path[:len(path)-1]
		colors[step.t] = black
	}

	for _, t := range starts {
		if colors[t] == white {
			visit(pathStep{t: t, via: t})
		}
	}

	slices.SortStableFunc(findings, func(a, b finding) int { return cmp.Compare(a.at, b.at) })
	// A constructor that takes one type twice finds its problem twice.
	errs := make([]error, 0, len(findings))
	seen := make(map[string]bool, len(findings))
	for _, f := range findings {
		if text := f.err.Error(); !seen[text] {
			seen[text] = true
			errs = append(errs, f.err)
		}
	}
	return errors.Join(errs...)
}

// pathStep is one registration on a dependency path, with the constructor
// parameter type that led to it: the registration's own type, an interface
// aliased to it, or the slice of a collection it belongs to.
type pathStep struct {
	t   reflect.Type
	via reflect.Type
}

// String names the registration as its consumer asked for it.
func (s pathStep) String() string {
	switch {
	case s.via == s.t:
		return s.t.String()
	case isInterfaceSlice(s.via):
		return s.t.String() + " (in " + s.via.String() + ")"
	default:
		return s.via.String() + " (alias of " + s.t.String() + ")"
	}
}

// formatPath renders a dependency path from its first registration on.
func formatPath(path []pathStep) string {
	parts := make([]string, len(path))
	for i, step := range path {
		parts[i] = step.String()
	}
	parts[0] = path[0].t.String()
	return strings.Join(parts, " → ")
}

// cycleMembers returns the registrations of the cycle that closes when the
// walk on path reaches back: from the member it reaches back to, which takes
// the closing step, to the end of the path.
func cycleMembers(path []pathStep, back pathStep) []pathStep {
	k := slices.IndexFunc(path, func(s pathStep) bool { return s.t == back.t })
	members := slices.Clone(path[k:])
	members[0] = back
	return members
}

// formatGraphCycle renders a cycle starting and ending at members[first]. Each
// step after the first is named as the constructor before it asked for it.
func formatGraphCycle(members []pathStep, first int) string {
	parts := make([]string, 0, len(members)+1)
	parts = append(parts, members[first].t.String())
	for k := 1; k <= len(members); k++ {
		parts = append(parts, members[(first+k)%len(members)].String())
	}
	return strings.Join(parts, " → ")
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

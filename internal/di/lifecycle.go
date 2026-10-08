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

// shutdowner is implemented by components: values whose teardown the App
// owns. This mirrors the public credo.Component interface. Structural typing
// ensures any type implementing credo.Component also satisfies this.
type shutdowner interface {
	Shutdown(ctx context.Context) error
}

// validate checks the container's dependency graph and reports every problem
// it finds, not the first:
//   - a missing dependency (a constructor parameter that nothing registers),
//     with its whole path from the registration the walk started at;
//   - each circular dependency (A → B → A);
//   - context.Context parameters, which constructors cannot take;
//   - an internal component that depends on an ingress component, directly or
//     through bindings that are not components.
//
// Aliases and BindMany collections are not checked again here: Alias and
// BindMany reject a binding whose types do not fit when it is made, and no
// registration is ever removed, so an alias always names a registered type
// and a collection holds registered implementations of its interface.
//
// The walk starts at the registrations nothing depends on, the graph's entry
// points — constructors handed to Manage among them — so a path reads from
// what the application asked for down to what is missing; registrations only
// a cycle reaches follow. Each walk follows registration order, never a map,
// and the findings are sorted by the registration they belong to — a path's
// first element, a cycle's earliest registered member, which its text starts
// at — so the same wiring yields the same report on every run.
func (c *Container) validate() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	index := make(map[reflect.Type]int, len(c.order))
	for _, t := range c.order {
		index[t] = c.unitOf[t].index
	}

	// Entry points first, then what only a cycle reaches.
	depended := make(map[reflect.Type]bool, len(c.order))
	for _, u := range c.units {
		for _, pt := range c.depsOfLocked(u) {
			if pt == contextType || c.isFrameworkType(pt) {
				continue
			}
			for _, dep := range c.cycleDependenciesForParam(pt) {
				depended[dep] = true
			}
		}
	}
	var starts []*Unit
	for _, u := range c.units {
		if u.managed || !depended[u.t] {
			starts = append(starts, u)
		}
	}
	for _, u := range c.units {
		if !u.managed && depended[u.t] {
			starts = append(starts, u)
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
	// visitDeps checks the parameters of the registration at the end of path.
	visitDeps := func(deps []reflect.Type) {
		at := path[len(path)-1]
		for i, pt := range deps {
			if pt == contextType {
				findings = append(findings, finding{path[0].index(index), fmt.Errorf(
					"di: %s (param %d): context.Context parameter is not allowed in constructors; "+
						"take the context in the methods that need it", at.head(), i)})
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
				findings = append(findings, finding{path[0].index(index), fmt.Errorf(
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
	}
	visit = func(step pathStep) {
		colors[step.t] = gray
		path = append(path, step)
		visitDeps(c.registrations[step.t].deps())
		path = path[:len(path)-1]
		colors[step.t] = black
	}

	for _, u := range starts {
		switch {
		case u.managed:
			path = append(path, pathStep{t: u.t, via: u.t, label: u.label(), at: u.index})
			visitDeps(u.prov.deps())
			path = path[:0]
		case colors[u.t] == white:
			visit(pathStep{t: u.t, via: u.t})
		}
	}

	// No dependency crosses the tiers backwards.
	for _, u := range c.units {
		if !u.component || u.tier != TierInternal {
			continue
		}
		p := c.ingressPathLocked(u)
		if p == nil {
			continue
		}
		dep := p[len(p)-1].t
		findings = append(findings, finding{u.index, fmt.Errorf(
			"di: internal component %s depends on the ingress component %s: %s; declare %s ingress with "+
				"credo.Ingress(), or split %s so that what internal components use is an internal part it "+
				"depends on", p[0].head(), dep, formatPath(p), p[0].head(), dep)})
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

// depsOfLocked returns the constructor parameter types of a unit; a value has
// none. c.mu must be held.
func (c *Container) depsOfLocked(u *Unit) []reflect.Type {
	if u.managed {
		return u.prov.deps()
	}
	if reg, ok := c.registrations[u.t]; ok {
		return reg.deps()
	}
	return nil
}

// depUnitsLocked returns the units a unit's constructor parameters resolve
// to, deduplicated, in parameter order. c.mu must be held.
func (c *Container) depUnitsLocked(u *Unit) []*Unit {
	var deps []*Unit
	seen := make(map[*Unit]bool)
	for _, pt := range c.depsOfLocked(u) {
		if pt == contextType || c.isFrameworkType(pt) {
			continue
		}
		for _, dt := range c.cycleDependenciesForParam(pt) {
			d := c.unitOf[dt]
			if d == nil || d == u || seen[d] {
				continue
			}
			seen[d] = true
			deps = append(deps, d)
		}
	}
	return deps
}

// ingressPathLocked returns the dependency path from u to the first ingress
// component it reaches through bindings that are not components, or nil. A
// component on the way ends the search along that branch: its own tier is
// checked from it. c.mu must be held.
func (c *Container) ingressPathLocked(u *Unit) []pathStep {
	visited := map[*Unit]bool{u: true}
	start := pathStep{t: u.t, via: u.t, at: u.index}
	if u.managed {
		start.label = u.label()
	}
	var walk func(from *Unit, path []pathStep) []pathStep
	walk = func(from *Unit, path []pathStep) []pathStep {
		for _, pt := range c.depsOfLocked(from) {
			if pt == contextType || c.isFrameworkType(pt) {
				continue
			}
			for _, dt := range c.cycleDependenciesForParam(pt) {
				d := c.unitOf[dt]
				if d == nil || visited[d] {
					continue
				}
				visited[d] = true
				next := append(slices.Clone(path), pathStep{t: dt, via: pt})
				if d.component {
					if d.tier == TierIngress {
						return next
					}
					continue
				}
				if found := walk(d, next); found != nil {
					return found
				}
			}
		}
		return nil
	}
	return walk(u, []pathStep{start})
}

// pathStep is one registration on a dependency path, with the constructor
// parameter type that led to it: the registration's own type, an interface
// aliased to it, or the slice of a collection it belongs to.
type pathStep struct {
	t   reflect.Type
	via reflect.Type
	// label names a managed component that starts a path, and at is its
	// registration index; a binding is named by its type.
	label string
	at    int
}

// head names the step that starts a path.
func (s pathStep) head() string {
	if s.label != "" {
		return s.label
	}
	return s.t.String()
}

// index is the registration index a finding at this step sorts by.
func (s pathStep) index(byType map[reflect.Type]int) int {
	if s.label != "" {
		return s.at
	}
	return byType[s.t]
}

// String names the registration as its consumer asked for it.
func (s pathStep) String() string {
	switch {
	case s.label != "":
		return s.label
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
	parts[0] = path[0].head()
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

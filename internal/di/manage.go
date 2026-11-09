package di

import (
	"reflect"
	"slices"
)

// Manage adds a unit that is not a binding — a value, or a constructor over
// DI parameters that the start walk builds and that never becomes a binding
// — and returns it. It must be a component, a value with a Shutdown method
// whose teardown the App owns, except the value a worker's constructor
// builds (o.Worker), whose teardown is a Shutdown found on it, if any. A
// function whose own type has Shutdown is a value, not a constructor. o.Name
// names it, defaulting to its type; o.Ingress places it in the ingress tier.
//
// Only a value claims what it holds. The value a constructor builds claims
// nothing: when it holds a resource a registration holds, it defers to that
// registration. Every rejection is a [*MisuseError].
func (c *Container) Manage(v any, o Options) (*Unit, error) {
	if v == nil {
		return nil, &MisuseError{Call: "Manage", Reason: "the component must not be nil"}
	}
	u := &Unit{managed: true, worker: o.Worker, ingressGiven: o.Ingress}
	var token any
	var hasToken bool
	// A function is a constructor unless its own type is a component — a
	// named func type with Shutdown is a value, since a constructor's type
	// carries no methods.
	if vt := reflect.TypeOf(v); vt.Kind() == reflect.Func && !vt.Implements(shutdownerType) {
		reg, err := inspectConstructor(v)
		if err != nil {
			return nil, misuse("Manage", []reflect.Type{reflect.TypeOf(v)}, "%v; want a component value, or "+
				"func(dependencies...) C or func(dependencies...) (C, error) for a component type C", err)
		}
		u.t = reg.resultType
		u.prov = reg
		u.entry = &singletonEntry{}
	} else {
		u.t = reflect.TypeOf(v)
		u.prov = valueProvider{value: v}
		u.entry = &singletonEntry{state: entryBuilt, value: v}
		u.claimant = true
		var err error
		if token, hasToken, err = identityToken(v); err != nil {
			return nil, misuse("Manage", []reflect.Type{u.t}, "%v", err)
		}
	}
	u.name = o.Name
	u.ingressRemedy = o.IngressRemedy
	if u.name == "" {
		u.name = u.t.String()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	types := []reflect.Type{u.t}
	if err := c.closedLocked("Manage", u.t); err != nil {
		return nil, err
	}
	u.nameGiven = o.Name != "" && o.Worker == ""
	if reason := planUnit(u, Options{Ingress: o.Ingress, OptionalReadiness: o.OptionalReadiness}); reason != "" {
		return nil, misuse("Manage", types, "%s", reason)
	}
	if !u.component && u.worker == "" {
		return nil, misuse("Manage", types, "%s has no Shutdown(context.Context) error "+
			"method, so there is no teardown for the App to own; a component is a value with Shutdown", u.t)
	}
	u.index = len(c.units)
	if u.entry.state == entryBuilt {
		if err := c.admitValueLocked(u, v, token, hasToken); err != nil {
			return nil, misuse("Manage", types, "%v", err)
		}
	} else if err := c.nameConflictLocked(u, nil); err != nil {
		return nil, misuse("Manage", types, "%v", err)
	}
	c.units = append(c.units, u)
	c.names[u.name] = append(c.names[u.name], u)
	return u, nil
}

// ManageNameFree reports, as the [*MisuseError] Manage would return, whether
// a constructor handed to Manage under name would be refused for the name: a
// registration that adds several units checks each name before it adds the
// first, so that a refusal adds none.
func (c *Container) ManageNameFree(name string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.nameConflictLocked(&Unit{managed: true, name: name}, nil); err != nil {
		return misuse("Manage", nil, "%v", err)
	}
	return nil
}

// AddEdge makes from depend on to, two units that are not bindings, beside
// what from's constructor parameters give it: the start walk builds and
// starts to before from, and the drain stops from before to. A worker's
// loop depends on the value its constructor builds this way. Every
// rejection is a [*MisuseError].
func (c *Container) AddEdge(from, to *Unit) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.closedLocked("AddEdge", from.t); err != nil {
		return err
	}
	switch {
	case !from.managed || !to.managed:
		return misuse("AddEdge", []reflect.Type{from.t, to.t}, "an edge joins two units that are not bindings; "+
			"a binding's edges are its constructor's parameters")
	case from == to:
		return misuse("AddEdge", []reflect.Type{from.t}, "a unit cannot depend on itself")
	case !slices.Contains(c.units, from) || !slices.Contains(c.units, to):
		return misuse("AddEdge", []reflect.Type{from.t, to.t}, "both units must be registered in this container")
	}
	if !slices.Contains(from.edges, to) {
		from.edges = append(from.edges, to)
	}
	return nil
}

// BuildUnit returns a unit's value, building it — and what it depends on —
// when it is still unbuilt. The start walk calls it for every component it
// starts or asks for readiness.
func (c *Container) BuildUnit(u *Unit) (any, error) {
	if !u.managed {
		return c.resolve(u.t, nil)
	}
	if err := c.admitResolve("Manage", u.t); err != nil {
		return nil, err
	}
	return c.resolveEntry(u.prov, u.t, u, u.entry, nil)
}

// UnitValue returns a unit's built value, and false while it is not built.
func (c *Container) UnitValue(u *Unit) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e := c.entryOf(u); e != nil && e.state == entryBuilt {
		return e.value, true
	}
	return nil, false
}

// Units returns the component registry in registration order.
func (c *Container) Units() []*Unit {
	c.mu.RLock()
	defer c.mu.RUnlock()
	units := make([]*Unit, len(c.units))
	copy(units, c.units)
	return units
}

// HasStartWork reports whether any unit is started by the start walk, asks
// for readiness, or is a managed constructor the walk builds.
func (c *Container) HasStartWork() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, u := range c.units {
		if u.plannedLocked() {
			return true
		}
	}
	return false
}

// plannedLocked reports whether the start walk visits the unit. c.mu must be
// held.
func (u *Unit) plannedLocked() bool {
	return u.starts || u.readies || (u.managed && u.entry.state != entryBuilt)
}

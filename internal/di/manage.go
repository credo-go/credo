package di

import (
	"fmt"
	"reflect"
)

// Manage adds a component that is not a binding: a value, or a constructor
// over DI parameters that the start walk builds and that never becomes a
// binding. It is a component only when it has a Shutdown method — the App
// owns its teardown. A function whose own type has Shutdown is a value, not
// a constructor. o.Name names it, defaulting to its type; o.Ingress
// places it in the ingress tier. Every rejection is a [*MisuseError].
func (c *Container) Manage(v any, o Options) error {
	if v == nil {
		return &MisuseError{Call: "Manage", Reason: "the component must not be nil"}
	}
	u := &Unit{managed: true}
	var token any
	var hasToken bool
	// A function is a constructor unless its own type is a component — a
	// named func type with Shutdown is a value, since a constructor's type
	// carries no methods.
	if vt := reflect.TypeOf(v); vt.Kind() == reflect.Func && !vt.Implements(shutdownerType) {
		reg, err := inspectManagedConstructor(v)
		if err != nil {
			return misuse("Manage", []reflect.Type{reflect.TypeOf(v)}, "%v; want a component value, or "+
				"func(dependencies...) C or func(dependencies...) (C, error) for a component type C", err)
		}
		u.t = reg.resultType
		u.prov = reg
		u.entry = &singletonEntry{}
	} else {
		u.t = reflect.TypeOf(v)
		u.prov = valueProvider{value: v}
		u.entry = &singletonEntry{state: entryBuilt, value: v}
		var err error
		if token, hasToken, err = identityToken(v); err != nil {
			return misuse("Manage", []reflect.Type{u.t}, "%v", err)
		}
	}
	u.name = o.Name
	if u.name == "" {
		u.name = u.t.String()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	types := []reflect.Type{u.t}
	if err := c.closedLocked("Manage", u.t); err != nil {
		return err
	}
	if _, dup := c.names[u.name]; dup {
		return misuse("Manage", types, "a component named %q is already managed; "+
			"give each component its own name with credo.Named", u.name)
	}
	if reason := planUnit(u, Options{Ingress: o.Ingress}); reason != "" {
		return misuse("Manage", types, "%s", reason)
	}
	if !u.component {
		return misuse("Manage", types, "%s has no Shutdown(context.Context) error "+
			"method, so there is no teardown for the App to own; a component is a value with Shutdown", u.t)
	}
	if u.entry.state == entryBuilt {
		if err := c.admitValueLocked(u, v, token, hasToken); err != nil {
			return misuse("Manage", types, "%v", err)
		}
	}
	u.index = len(c.units)
	c.units = append(c.units, u)
	c.names[u.name] = u
	return nil
}

// inspectManagedConstructor checks a constructor handed to Manage: any
// result type, with the same shape rules as Provide.
func inspectManagedConstructor(constructor any) (*constructorProvider, error) {
	ct := reflect.TypeOf(constructor)
	if ct.NumOut() == 0 {
		return nil, fmt.Errorf("constructor must return 1 or 2 values, got 0")
	}
	return inspectConstructor(constructor, ct.Out(0))
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
// for readiness, has a start step, or is a managed constructor the walk
// builds.
func (c *Container) HasStartWork() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.steps) > 0 {
		return true
	}
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
	return u.starts || u.readies || len(u.steps) > 0 || (u.managed && u.entry.state != entryBuilt)
}

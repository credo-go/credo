package di

import (
	"fmt"
	"reflect"
	"slices"
)

// noteUnreachedLocked records that an override left t without the alias that
// pointed at it. Seal drops t when nothing else reaches it. c.mu must be held.
func (c *Container) noteUnreachedLocked(t reflect.Type, how string) {
	if _, ok := c.unreached[t]; !ok {
		c.unreachedOrder = append(c.unreachedOrder, t)
	}
	c.unreached[t] = how
}

// dropUnreachedLocked removes, before validation, each type an override left
// unreached that no alias, BindMany entry, start step or constructor
// parameter of a unit that stays reaches. A dropped type is never built or
// started, and Resolve of it names the override. c.mu must be held.
func (c *Container) dropUnreachedLocked() {
	drop := make(map[reflect.Type]bool)
	for _, t := range c.unreachedOrder {
		if _, ok := c.registrations[t]; ok {
			drop[t] = true
		}
	}
	for changed := len(drop) > 0; changed; {
		changed = false
		reached := c.reachedLocked(drop)
		for t := range drop {
			if reached[t] {
				delete(drop, t)
				changed = true
			}
		}
	}
	for _, t := range c.unreachedOrder {
		if drop[t] {
			c.removeBindingLocked(t)
			c.dropped[t] = c.unreached[t]
		}
	}
}

// reachedLocked returns the registered types that an alias, a BindMany
// entry, a start step or a constructor parameter of a unit outside drop
// reaches. c.mu must be held.
func (c *Container) reachedLocked(drop map[reflect.Type]bool) map[reflect.Type]bool {
	reached := make(map[reflect.Type]bool)
	for _, target := range c.aliases {
		reached[target] = true
	}
	for _, bindings := range c.manyBindings {
		for _, t := range bindings {
			reached[t] = true
		}
	}
	for _, u := range c.units {
		if !u.managed && drop[u.t] {
			continue
		}
		for _, pt := range c.depsOfLocked(u) {
			for _, dep := range c.cycleDependenciesForParam(pt) {
				reached[dep] = true
			}
		}
	}
	return reached
}

// removeBindingLocked removes the binding of t, which nothing has built for
// the App: the value a ProvideValue binding holds never becomes the App's.
// c.mu must be held.
func (c *Container) removeBindingLocked(t reflect.Type) {
	u := c.unitOf[t]
	delete(c.registrations, t)
	delete(c.singletons, t)
	delete(c.unitOf, t)
	c.order = slices.DeleteFunc(c.order, func(x reflect.Type) bool { return x == t })
	if u == nil {
		return
	}
	c.forgetHolderLocked(u)
	c.forgetNameLocked(u)
	c.units = slices.DeleteFunc(c.units, func(x *Unit) bool { return x == u })
	for i, x := range c.units {
		x.index = i
	}
}

// droppedError reports a resolution of a type Seal dropped. c.mu must be
// held for reading.
func (c *Container) droppedErrorLocked(op string, t reflect.Type) error {
	how, ok := c.dropped[t]
	if !ok {
		return nil
	}
	return fmt.Errorf("di: %s[%s]: not registered: Finalize dropped it, since %s and nothing else uses %s; "+
		"resolve the interface the alias named, or keep the alias", op, t, how, t)
}

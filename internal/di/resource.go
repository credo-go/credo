package di

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/credo-go/credo/internal/resourceid"
)

// identityToken returns the resource identity a value carries: the token its
// ResourceIdentity method returns, otherwise the value itself when it is
// comparable — a pointer, or a struct over one. ok is false when the value
// has neither and is therefore its holder's own. A ResourceIdentity that
// panics or returns a token that is nil, not comparable or not equal to
// itself is an error. It runs user code, so callers hold no lock.
func identityToken(value any) (token any, ok bool, err error) {
	if value == nil {
		return nil, false, nil
	}
	if _, provides := value.(identifier); provides {
		id, idErr := resourceid.Of(value)
		if idErr != nil {
			return nil, false, idErr
		}
		return id, true, nil
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Chan, reflect.UnsafePointer:
		if rv.IsNil() {
			return nil, false, nil
		}
	case reflect.Struct:
		if !rv.Comparable() {
			return nil, false, nil
		}
	default:
		return nil, false, nil
	}
	if id, idErr := resourceid.Of(value); idErr == nil {
		return id, true, nil
	}
	// A comparable value that is not equal to itself (a NaN field) is its
	// holder's own rather than an error: it named no identity.
	return nil, false, nil
}

// entryOf returns the singleton entry of a unit. c.mu must be held.
func (c *Container) entryOf(u *Unit) *singletonEntry {
	if u.managed {
		return u.entry
	}
	return c.singletons[u.t]
}

// valueOf returns a unit's built value, or nil. c.mu must be held.
func (c *Container) valueOf(u *Unit) any {
	if e := c.entryOf(u); e != nil && e.state == entryBuilt {
		return e.value
	}
	return nil
}

// teardownValueOf returns what the drain releases for u: its built value, or
// the value its failed construction left to the App. c.mu must be held.
func (c *Container) teardownValueOf(u *Unit) any {
	e := c.entryOf(u)
	switch {
	case e == nil:
		return nil
	case e.state == entryBuilt:
		return e.value
	case e.state == entryFailed:
		return e.rejected
	}
	return nil
}

// admitValueLocked checks a holder's newly known value against the other
// holders of its resource and records it. Values that share an identity are
// one resource with one teardown, so their holders must agree on who owns it
// and how it is released. c.mu must be held.
func (c *Container) admitValueLocked(u *Unit, value any, token any, hasToken bool) error {
	if !hasToken {
		u.key = u
		return nil
	}
	holders := c.resources[token]
	for _, h := range holders {
		switch {
		case u.managed && h.managed:
			return fmt.Errorf("this resource was already handed to Manage as %s; hand each resource to "+
				"Manage once", h.name)
		case u.managed && h.valueBinding:
			return fmt.Errorf("the resource is already managed through DI by the ProvideValue binding of %s; "+
				"drop the Manage call, since that binding is already the component", h.t)
		case u.valueBinding && h.managed:
			return fmt.Errorf("the resource was already handed to Manage as %s; drop the ProvideValue binding's "+
				"teardown claim or the Manage call, so that one of them owns it", h.name)
		}
	}
	all := append(slices.Clone(holders), u)
	if err := c.ownerConflict(all, u, value); err != nil {
		return err
	}
	if err := c.kindConflict(all, u, value); err != nil {
		return err
	}
	u.key = token
	c.resources[token] = all
	return nil
}

// holderValue is a holder's built value, or v for the holder being admitted.
func (c *Container) holderValue(h, admitted *Unit, v any) any {
	if h == admitted {
		return v
	}
	return c.valueOf(h)
}

// ownerConflict refuses a resource that one holder borrows while another
// claims its teardown.
func (c *Container) ownerConflict(holders []*Unit, admitted *Unit, v any) error {
	var borrowed, claimant *Unit
	for _, h := range holders {
		switch {
		case h.borrowed && borrowed == nil:
			borrowed = h
		case h.claims(c.holderValue(h, admitted, v)) && claimant == nil:
			claimant = h
		}
	}
	if borrowed == nil || claimant == nil {
		return nil
	}
	return fmt.Errorf("%s and %s hold one resource but disagree on its owner: %s is bound with "+
		"credo.Borrowed(), so the caller shuts it down, and %s claims its teardown; drop credo.Borrowed() "+
		"so the App owns the resource, or bind %s without claiming its teardown",
		borrowed.label(), claimant.label(), borrowed.label(), claimant.label(), claimant.label())
}

// kindConflict refuses holders of one resource that release it differently:
// one through credo.Closer(), another through Shutdown. Among holders of one
// kind the one registered first runs the teardown, so kinds are all that is
// compared.
func (c *Container) kindConflict(holders []*Unit, admitted *Unit, v any) error {
	for _, h := range holders {
		if h.borrowed {
			return nil
		}
	}
	var closer, shutdown *Unit
	for _, h := range holders {
		switch h.teardownOf(c.holderValue(h, admitted, v)) {
		case teardownClose:
			if closer == nil {
				closer = h
			}
		case teardownShutdown:
			if shutdown == nil {
				shutdown = h
			}
		}
	}
	if closer == nil || shutdown == nil {
		return nil
	}
	return fmt.Errorf("%s and %s hold one resource but disagree on its teardown: %s has credo.Closer() "+
		"and %s has Shutdown; drop credo.Closer(), or bind %s under a type that shows Close and not "+
		"Shutdown, with credo.Closer()",
		closer.label(), shutdown.label(), closer.label(), shutdown.label(), shutdown.label())
}

// forgetHolderLocked removes a holder whose binding an override replaced:
// the replaced value never becomes the App's. c.mu must be held.
func (c *Container) forgetHolderLocked(u *Unit) {
	if u == nil || u.key == nil {
		return
	}
	if _, own := u.key.(*Unit); own {
		return
	}
	holders := slices.DeleteFunc(slices.Clone(c.resources[u.key]), func(h *Unit) bool { return h == u })
	if len(holders) == 0 {
		delete(c.resources, u.key)
		return
	}
	c.resources[u.key] = holders
}

// admitBuiltLocked checks a value a constructor has just built: a component
// found only on its value is internal, so it must not depend on an ingress
// component, and its resource's holders must agree. c.mu must be held.
func (c *Container) admitBuiltLocked(u *Unit, value any, token any, hasToken bool) error {
	if u.foundOnValue(value) {
		if path := c.ingressPathLocked(u); path != nil {
			return fmt.Errorf("%s is an internal component — its Shutdown is found only on its built value — "+
				"and depends on the ingress component %s: %s; provide the concrete type with credo.Ingress() "+
				"and Alias the interface, or split %s so that what internal components use is an internal part",
				u.t, path[len(path)-1].t, formatPath(path), path[len(path)-1].t)
		}
	}
	return c.admitValueLocked(u, value, token, hasToken)
}

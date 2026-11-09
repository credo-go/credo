package di

import (
	"cmp"
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
// one resource with one teardown and one readiness entry, so their holders
// must agree on who owns it, how it is released, its tier and the options
// that shape its entry. c.mu must be held.
func (c *Container) admitValueLocked(u *Unit, value any, token any, hasToken bool) error {
	key := any(u)
	if hasToken {
		key = token
	}
	if err := c.nameConflictLocked(u, key); err != nil {
		return err
	}
	if !hasToken {
		u.key = u
		return nil
	}
	holders := c.resources[token]
	for _, h := range holders {
		switch {
		case u.claimant && h.claimant:
			return fmt.Errorf("this resource was already handed to Manage as %s; hand each resource to "+
				"Manage once", h.name)
		case u.claimant && h.valueBinding:
			return fmt.Errorf("the resource is already managed through DI by the ProvideValue binding of %s; "+
				"drop the Manage call, since that binding is already the component", h.t)
		case u.valueBinding && h.claimant:
			return fmt.Errorf("the resource was already handed to Manage as %s; drop the ProvideValue binding's "+
				"teardown claim or the Manage call, so that one of them owns it", h.name)
		}
	}
	all := append(slices.Clone(holders), u)
	slices.SortStableFunc(all, func(a, b *Unit) int { return cmp.Compare(a.index, b.index) })
	if err := deferringOptionsConflict(all); err != nil {
		return err
	}
	if err := c.ownerConflict(all, u, value); err != nil {
		return err
	}
	if err := c.kindConflict(all, u, value); err != nil {
		return err
	}
	if err := tierConflict(all); err != nil {
		return err
	}
	if err := c.entryConflict(all, u, value); err != nil {
		return err
	}
	u.key = token
	c.resources[token] = all
	// The resource has one name, which every holder that takes part in it
	// carries: its readiness entry, its teardown and the scope of each call
	// made on it. A value a constructor that is not a binding builds keeps
	// its own name, under which its build is reported.
	for _, h := range all {
		if h.nameGiven {
			for _, other := range all {
				if other.registration() {
					other.name = h.name
				}
			}
			break
		}
	}
	return nil
}

// holderOfRecord returns the holder of record among the holders of one
// resource, given in registration order: the first registration — a binding,
// or a value handed to Manage — that holds it, preferring one that is a
// component by its type; without one, the first holder. A value a
// constructor that is not a binding builds — a worker's, or one from a
// constructor handed to Manage — claims nothing, so it is the holder of
// record only of a resource no registration holds.
func holderOfRecord(holders []*Unit) *Unit {
	var first *Unit
	for _, h := range holders {
		if !h.registration() {
			continue
		}
		if h.component {
			return h
		}
		if first == nil {
			first = h
		}
	}
	if first != nil || len(holders) == 0 {
		return first
	}
	return holders[0]
}

// takesPart reports whether holder h of a resource takes part in its
// readiness entry, its teardown and the agreements on them: every
// registration that holds it, and the holder of record of a resource no
// registration holds. A holder that takes no part defers to the holder of
// record: it adds no readiness entry and never runs the teardown.
func takesPart(h *Unit, holders []*Unit) bool {
	return h.registration() || h == holderOfRecord(holders)
}

// deferringOptionsConflict refuses a constructor handed to Manage with
// credo.ComponentName, credo.Ingress() or credo.OptionalReadiness() whose
// value defers to its resource's holder of record: the value has no
// readiness entry and no teardown of its own for the option to shape.
func deferringOptionsConflict(holders []*Unit) error {
	for _, h := range holders {
		if !h.managed || h.worker != "" || takesPart(h, holders) {
			continue
		}
		var given []string
		if h.nameGiven {
			given = append(given, "credo.ComponentName")
		}
		if h.ingressGiven {
			given = append(given, "credo.Ingress()")
		}
		if h.optional {
			given = append(given, "credo.OptionalReadiness()")
		}
		if len(given) == 0 {
			continue
		}
		record := holderOfRecord(holders)
		what, noun := record.registrationPhrase()
		return fmt.Errorf("%s shares the resource of %s, whose readiness and shutdown follow that %s; give %s "+
			"to that %s, or hold the %s in a named field", h.t, what, noun, joinAnd(given), noun, record.t)
	}
	return nil
}

// registrationPhrase names the registration u is in the application's
// terms, and the noun a remedy refers to it by.
func (u *Unit) registrationPhrase() (what, noun string) {
	switch {
	case u.valueBinding:
		return "the ProvideValue binding of " + u.t.String(), "binding"
	case !u.managed:
		return "the Provide binding of " + u.t.String(), "binding"
	case u.claimant:
		return "the value handed to Manage as " + u.name, "Manage call"
	case u.worker != "":
		return "the value the constructor of " + u.worker + " builds", "worker"
	}
	return "the value the constructor handed to Manage as " + u.name + " builds", "Manage call"
}

// tierConflict refuses registrations of one resource that place it in
// different tiers: its one teardown runs in one tier. The registrations
// that are components by their type take part; a borrowed resource is
// exempt, since the App neither starts nor tears it down, and a value a
// constructor that is not a binding builds takes no part.
func tierConflict(holders []*Unit) error {
	var first *Unit
	for _, h := range holders {
		if h.borrowed {
			return nil
		}
	}
	for _, h := range holders {
		if !h.registration() || !h.component {
			continue
		}
		if first == nil {
			first = h
			continue
		}
		if h.tier != first.tier {
			return fmt.Errorf("%s and %s hold one resource, which has one teardown, but are placed in different "+
				"tiers: %s is %s and %s is %s; give them one tier, or, when one of them has a teardown of its "+
				"own to run in its tier, hold the handle in a named field, so that it is a resource of its own",
				first.label(), h.label(), first.label(), first.Tier(), h.label(), h.Tier())
		}
	}
	return nil
}

// nameConflictLocked refuses a credo.ComponentName name — or a managed
// component's name — another registration already carries, unless both
// hold one resource. key is u's resource, nil while its value does not
// exist; a registration whose resource is not yet known is checked when its
// value is built. c.mu must be held.
func (c *Container) nameConflictLocked(u *Unit, key any) error {
	if !u.nameGiven && !u.managed {
		return nil
	}
	for _, o := range c.names[u.name] {
		if o == u {
			continue
		}
		conflict := false
		switch {
		case (u.managed && key == nil) || (o.managed && o.key == nil):
			// A constructor handed to Manage shares its name with no one.
			conflict = true
		case key == nil || o.key == nil:
			// A binding's constructor has not run yet: checked once its
			// value is built.
		default:
			_, own := o.key.(*Unit)
			conflict = own || o.key != key
		}
		if conflict {
			return fmt.Errorf("a component named %q is already registered, as %s; give each component its own "+
				"name with credo.ComponentName", u.name, o.label())
		}
	}
	return nil
}

// forgetNameLocked removes a unit an override or Finalize removed from the
// name index. c.mu must be held.
func (c *Container) forgetNameLocked(u *Unit) {
	if u == nil {
		return
	}
	names := slices.DeleteFunc(slices.Clone(c.names[u.name]), func(x *Unit) bool { return x == u })
	if len(names) == 0 {
		delete(c.names, u.name)
		return
	}
	c.names[u.name] = names
}

// entryConflict refuses holders of one resource that disagree on the
// options that shape its one readiness entry and its teardown: every holder
// that plans a readiness entry gives credo.OptionalReadiness(), or none
// does, and the names given with credo.ComponentName are one name.
func (c *Container) entryConflict(holders []*Unit, admitted *Unit, v any) error {
	var optional, gating, named *Unit
	for _, h := range holders {
		if !takesPart(h, holders) {
			continue
		}
		if h.nameGiven {
			if named != nil && named.name != h.name {
				return fmt.Errorf("%s and %s hold one resource, which has one readiness entry and one "+
					"teardown, but name it differently with credo.ComponentName, %q and %q; give both the same "+
					"options, or hold the handle in a named field so that each is a resource of its own",
					named.label(), h.label(), named.name, h.name)
			}
			named = h
		}
		if !h.readies || !h.RunsCapabilities(c.holderValue(h, admitted, v)) {
			continue
		}
		switch {
		case h.optional && optional == nil:
			optional = h
		case !h.optional && gating == nil:
			gating = h
		}
	}
	if optional == nil || gating == nil {
		return nil
	}
	return fmt.Errorf("%s and %s hold one resource, which has one readiness entry, but disagree on "+
		"credo.OptionalReadiness(): %s has it and %s does not; give both the same options, or hold the handle "+
		"in a named field so that each is a resource of its own",
		optional.label(), gating.label(), optional.label(), gating.label())
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
// one through credo.CloseOnShutdown(), another through Shutdown. The holder
// of record runs the teardown (teardownHolderLocked), so kinds are all that
// is compared.
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
	return fmt.Errorf("%s and %s hold one resource but disagree on its teardown: %s has credo.CloseOnShutdown() "+
		"and %s has Shutdown; drop credo.CloseOnShutdown(), or bind %s under a type that shows Close and not "+
		"Shutdown, with credo.CloseOnShutdown()",
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

// admitBuiltLocked checks a value a constructor has just built: its type
// must not hide the Start or Ready of a component, a component found only on
// its value is internal, so it must not depend on an ingress
// component, and its resource's holders must agree. c.mu must be held.
func (c *Container) admitBuiltLocked(u *Unit, value any, token any, hasToken bool) error {
	if err := c.hiddenCapabilityErrorLocked(u, value, token, hasToken); err != nil {
		return err
	}
	if u.foundOnValue(value) {
		if path := c.ingressPathLocked(u); path != nil {
			dep := path[len(path)-1].t
			if u.worker != "" {
				return workerValueIngressError(u, dep, reflect.TypeOf(value), path)
			}
			return fmt.Errorf("%s is an internal component — its Shutdown is found only on its built value — "+
				"and depends on the ingress component %s: %s; provide the concrete type with credo.Ingress() "+
				"and Alias the interface, or split %s so that what internal components use is an internal part",
				u.t, dep, formatPath(path), dep)
		}
	}
	return c.admitValueLocked(u, value, token, hasToken)
}

// workerValueIngressError reports the value a worker's constructor builds,
// internal and with a teardown of its own, that depends on the ingress
// component dep along path; built is the value's type, or nil before it
// exists. When dep is the binding the constructor returns there is no part
// to split, and the remedy is to bind it in the internal tier.
func workerValueIngressError(u *Unit, dep, built reflect.Type, path []pathStep) error {
	if dep == u.t || dep == built {
		return fmt.Errorf("the constructor of %s returns the ingress component %s, and the value a worker's "+
			"constructor builds is torn down in the internal tier, after the HTTP drain: %s; bind %s without "+
			"credo.Ingress()", u.worker, dep, formatPath(path), dep)
	}
	return fmt.Errorf("the value the constructor of %s builds is internal and has a teardown of its own, but "+
		"depends on the ingress component %s: %s; split the worker, so that the part with the teardown does "+
		"not use %s", u.worker, dep, formatPath(path), dep)
}

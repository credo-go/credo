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

// ownershipErrorLocked is the rule of resource ownership. A resource has one
// owner: the first registration whose value claims it (claims) — a value with
// Shutdown, a credo.CloseOnShutdown() binding, a credo.Borrowed() binding. A
// later registration whose value claims the same identity is refused unless
// it is a view. A registration that claims nothing, such as a second binding
// of a value with Close beside the one that carries credo.CloseOnShutdown(),
// is a plain holder and declares nothing. A view must carry an identity for
// an owner to own. The check records nothing, so the callers run it before
// the checks of the value's own capabilities: a second holder is told to
// declare itself before it is told what its type hides. replaced is the
// binding an override of u replaces, which owns nothing u is checked
// against, or nil. c.mu must be held.
func (c *Container) ownershipErrorLocked(u *Unit, value any, token any, hasToken bool, replaced *Unit) error {
	switch {
	case !hasToken && u.view:
		held := "its value is nil"
		if value != nil {
			held = fmt.Sprintf("its value, a %T, carries no resource identity: it is neither a non-nil pointer, a "+
				"comparable struct nor a credo.ResourceIdentifier", value)
		}
		return fmt.Errorf("credo.View() declares %s a view of a resource another registration owns, but %s, so "+
			"no registration can own what it holds; drop credo.View()", u.t, held)
	case !hasToken || !u.claims(value):
		return nil
	}
	if owner := c.owners[token]; owner != nil && owner != u && owner != replaced {
		return c.secondClaimError(u, owner, value, token)
	}
	return nil
}

// admitValueLocked records a holder's newly known value, which
// ownershipErrorLocked accepted: the first registration that claims an
// identity becomes the owner of its resource, through which alone the App
// starts it, reports its readiness and tears it down. c.mu must be held.
func (c *Container) admitValueLocked(u *Unit, value any, token any, hasToken bool) {
	if !hasToken {
		u.key = u
		return
	}
	if u.claims(value) && c.owners[token] == nil {
		c.owners[token] = u
	}
	u.key = token
	c.resources[token] = append(c.resources[token], u)
}

// secondClaimError refuses u, whose value claims the resource that owner
// owns. It names the owner and the remedies that apply to the call that
// registered u: a view for a binding; for a value a constructor builds, the
// method expression or one Manage call when it is the owner's own value and
// a named field when it is a wrapper; one Manage call for a value handed in.
// token is the resource's identity. c.mu must be held.
func (c *Container) secondClaimError(u, owner *Unit, value, token any) error {
	what, noun := owner.registrationPhrase()
	borrowed := ""
	if owner.borrowed {
		borrowed = " for its caller (credo.Borrowed())"
	}
	vt := reflect.TypeOf(value)
	ownerValue := c.valueOf(owner)
	ownerType := reflect.TypeOf(ownerValue)
	// same reports that u holds the owner's own value rather than a wrapper
	// that embeds it, or the handle a wrapper embeds.
	same := vt == ownerType
	held := func(subject string) string {
		return fmt.Sprintf("%s holds the resource that %s owns%s, and a resource has one owner", subject, what,
			borrowed)
	}
	// The named field goes into whichever of the two values wraps the other:
	// u's, unless u's value is the handle itself and the owner's wraps it.
	wrapper, inner := vt, ownerType
	if !wraps(value, token) && wraps(ownerValue, token) {
		wrapper, inner = ownerType, vt
	}
	namedField := fmt.Sprintf("hold the %s in a named field of %s, so that it is a resource of its own", inner,
		wrapper)
	switch {
	case u.handedIn && owner.handedIn:
		return fmt.Errorf("%s; hand each resource to Manage once", held(u.label()))
	case u.handedIn && same:
		return fmt.Errorf("%s; drop the Manage call, since that %s already owns it", held(u.label()), noun)
	case u.handedIn:
		return fmt.Errorf("%s; %s, or drop the Manage call", held(u.label()), namedField)
	case u.worker != "" && same:
		remedy := fmt.Sprintf("register the worker's run function, the method expression (%s).Run, in place of "+
			"a constructor that returns that %s's value", vt, noun)
		if owner.managed {
			remedy = fmt.Sprintf("bind the %s in place of %s, and register each worker that runs it by its run "+
				"function, the method expression (%s).Run", vt, owner.label(), vt)
		}
		return fmt.Errorf("%s; %s", held(fmt.Sprintf("the value %s builds, a %s,", u.worker, vt)), remedy)
	case u.worker != "":
		return fmt.Errorf("%s; %s", held(fmt.Sprintf("the value %s builds, a %s,", u.worker, vt)), namedField)
	case u.managed:
		subject := fmt.Sprintf("the value the constructor handed to %s builds, a %s,", u.label(), vt)
		if same {
			return fmt.Errorf("%s; drop the Manage call, since that %s already owns the value", held(subject), noun)
		}
		return fmt.Errorf("%s; %s", held(subject), namedField)
	case same && u.t.Kind() == reflect.Interface && !owner.managed && owner.t.Implements(u.t):
		return fmt.Errorf("%s; declare %s a view: bind it with app.Alias[%s, %s](), or %s", held(u.t.String()),
			u.t, u.t, owner.t, viewRemedy(u))
	case same:
		return fmt.Errorf("%s; declare %s a view: %s", held(u.t.String()), u.t, viewRemedy(u))
	}
	return fmt.Errorf("%s; declare %s a view: %s, or, when %s releases state of its own, %s", held(u.t.String()),
		u.t, viewRemedy(u), wrapper, namedField)
}

// wraps reports whether value holds the resource token names rather than
// being it: a wrapper whose identity is a handle it embeds.
func wraps(value, token any) bool {
	id, ok := token.(resourceid.Identity)
	return !ok || !id.IsSelf(value)
}

// viewRemedy tells a binding how to declare itself a view, naming the
// options it carries that a view refuses, since they are its owner's.
func viewRemedy(u *Unit) string {
	var given []string
	for _, opt := range []struct {
		on   bool
		name string
	}{
		{u.ingressGiven, "credo.Ingress()"},
		{u.nameGiven, "credo.ComponentName"},
		{u.optional, "credo.OptionalReadiness()"},
		{u.closer != closeNone, "credo.CloseOnShutdown()"},
		{u.borrowed, "credo.Borrowed()"},
	} {
		if opt.on {
			given = append(given, opt.name)
		}
	}
	if len(given) == 0 {
		return "give credo.View() to its registration"
	}
	return fmt.Sprintf("give credo.View() to its registration in place of %s, which only the owner takes",
		joinAnd(given))
}

// registrationPhrase names the registration u is in the application's
// terms, and the noun a remedy refers to it by.
func (u *Unit) registrationPhrase() (what, noun string) {
	switch {
	case u.valueBinding:
		return "the ProvideValue binding of " + u.t.String(), "binding"
	case !u.managed:
		return "the Provide binding of " + u.t.String(), "binding"
	case u.handedIn:
		return "the value handed to Manage as " + u.name, "Manage call"
	case u.worker != "":
		return "the value the constructor of " + u.worker + " builds", "worker"
	}
	return "the value the constructor handed to Manage as " + u.name + " builds", "Manage call"
}

// nameConflictLocked refuses a credo.ComponentName name — or a managed
// component's name — another registration already carries. A name is its
// component's alone: a resource's is its owner's, and a view takes none, so
// the check needs no value and runs at the call. replaced is the binding an
// override of u replaces, whose name u may take, or nil. c.mu must be held.
func (c *Container) nameConflictLocked(u, replaced *Unit) error {
	if !u.nameGiven && !u.managed {
		return nil
	}
	for _, o := range c.names[u.name] {
		if o != u && o != replaced {
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

// forgetHolderLocked removes a holder whose binding an override replaced:
// the replaced value never becomes the App's. When it owned its resource,
// the resource has no owner until another registration claims it, and a
// view of it is refused once its value exists — at Finalize for a value bound
// with ProvideValue, at its build for one a constructor builds — naming the
// override. c.mu must be held.
func (c *Container) forgetHolderLocked(u *Unit) {
	if u == nil || u.key == nil {
		return
	}
	if _, own := u.key.(*Unit); own {
		return
	}
	if c.owners[u.key] == u {
		delete(c.owners, u.key)
		c.replaced[u.key] = u
	}
	holders := slices.DeleteFunc(slices.Clone(c.resources[u.key]), func(h *Unit) bool { return h == u })
	if len(holders) == 0 {
		delete(c.resources, u.key)
		return
	}
	c.resources[u.key] = holders
}

// admitBuiltLocked checks a value a constructor has just built and records
// it: it must not claim a resource another registration owns, a view's owner
// must exist before it and an owner must not come after a view of its
// resource, its type must not hide the Start or Ready of a component, and a
// component found only on its value is internal, so it must not depend on an
// ingress component. c.mu must be held.
func (c *Container) admitBuiltLocked(u *Unit, value any, token any, hasToken bool) error {
	if err := c.ownershipErrorLocked(u, value, token, hasToken, nil); err != nil {
		return err
	}
	if err := c.viewOrderErrorLocked(u, value, token, hasToken); err != nil {
		return err
	}
	if err := hiddenCapabilityError(u, value); err != nil {
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
	c.admitValueLocked(u, value, token, hasToken)
	return nil
}

// viewOrderErrorLocked enforces, for a value a constructor has just built,
// that a view's owner exists before the view's value. A view whose value has
// Shutdown and whose resource no registration owns yet is refused, since
// nothing would start that resource or shut it down; a value that claims a
// resource a view already holds while no registration owns it is refused,
// since the view came first. A value handed in is admitted before anything
// is built, so these hold for it in either registration order; Finalize
// checks the views bound with ProvideValue. c.mu must be held.
func (c *Container) viewOrderErrorLocked(u *Unit, value, token any, hasToken bool) error {
	switch {
	case !hasToken || c.owners[token] != nil:
		return nil
	case u.view:
		if _, ok := value.(shutdowner); ok {
			return c.unownedViewError(u, value, token, true)
		}
	case u.claims(value):
		for _, h := range c.resources[token] {
			if h.view {
				return lateOwnerError(u, h, value, token, c.valueOf(h))
			}
		}
	}
	return nil
}

// unownedViewError refuses view u, whose value has Shutdown, when no
// registration owns its resource once that value exists. built is true for
// a value a constructor has just built and false for a value bound with
// ProvideValue, which Finalize checks against the values handed in. A view
// whose owner an override replaced names the override. c.mu must be held.
func (c *Container) unownedViewError(u *Unit, value, token any, built bool) error {
	if old := c.replaced[token]; old != nil {
		what, _ := old.registrationPhrase()
		return fmt.Errorf("credo.View() declares %s a view of a resource another registration owns, but its "+
			"owner, %s, was replaced with credo.Override(), and a replaced value is not the App's; override %s "+
			"too, so that it holds the replacing value, or %s, so that it follows the override", u.t, what, u.t,
			deriveViewRemedy(u, value, token, old.t, built))
	}
	why := fmt.Sprintf("no value handed in owns it, and its value, a %T, has Shutdown: a view's owner exists "+
		"before the view's value, and a value bound with ProvideValue exists before anything is built, so only a "+
		"value handed in can own its resource", value)
	if built {
		why = fmt.Sprintf("no registration owns it when %s is built, and its value, a %T, has Shutdown: a view's "+
			"owner exists before the view's value", u.t, value)
	}
	return fmt.Errorf("credo.View() declares %s a view of a resource another registration owns, but %s; give the "+
		"owner as a value, with ProvideValue or Manage, or %s, so that the owner exists first, or drop credo.View() "+
		"from %s, so that it owns the resource", u.t, why, deriveViewRemedy(u, value, token, nil, built), u.t)
}

// lateOwnerError refuses u, a value a constructor has just built that
// claims a resource view already holds while no registration owns it: a
// view's owner exists before the view's value. viewValue is the view's value
// and token the resource's identity.
func lateOwnerError(u, view *Unit, value, token, viewValue any) error {
	vt := reflect.TypeOf(value)
	viewWhat, _ := view.registrationPhrase()
	held := func(subject string) string {
		return fmt.Sprintf("%s claims the resource that %s, a view, already holds, and a view's owner exists "+
			"before the view's value", subject, viewWhat)
	}
	switch {
	case u.worker != "":
		return fmt.Errorf("%s; bind the %s in place of the worker's constructor — with ProvideValue, or with "+
			"Provide and %s — and register the worker by its run function, the method expression (%s).Run, so "+
			"that the owner exists first", held(fmt.Sprintf("the value %s builds, a %s,", u.worker, vt)), vt,
			deriveViewRemedy(view, viewValue, token, vt, !view.valueBinding), vt)
	case u.managed:
		return fmt.Errorf("%s; hand the %s to Manage as a value, or bind it in place of the Manage call — with "+
			"ProvideValue, or with Provide and %s — so that the owner exists first",
			held(fmt.Sprintf("the value the constructor handed to %s builds, a %s,", u.label(), vt)), vt,
			deriveViewRemedy(view, viewValue, token, vt, !view.valueBinding))
	}
	return fmt.Errorf("%s; bind the %s with ProvideValue, or %s, so that the owner exists first",
		held("the Provide binding of "+u.t.String()), u.t, deriveViewRemedy(view, viewValue, token, u.t,
			!view.valueBinding))
}

// deriveViewRemedy tells view u how to come from its owner's binding, so
// that the owner exists before it: an interface over the resource itself is
// an Alias of the owner's type, and anything else, a wrapper, is built by a
// constructor that takes the owner's binding. ownerT is the owner's type
// when it is known, else the view's value stands for it. built is true for
// a view a constructor builds, false for one bound with ProvideValue.
func deriveViewRemedy(u *Unit, value, token any, ownerT reflect.Type, built bool) string {
	in := "ProvideValue"
	if built {
		in = "its constructor"
	}
	if u.t.Kind() == reflect.Interface && !wraps(value, token) {
		if ownerT == nil {
			ownerT = reflect.TypeOf(value)
		}
		return fmt.Sprintf("bind %s with app.Alias[%s, %s]() in place of %s", u.t, u.t, ownerT, in)
	}
	binding := "the owner's binding"
	if ownerT != nil {
		binding = "the " + ownerT.String()
	}
	if built {
		return fmt.Sprintf("have the constructor of %s take %s", u.t, binding)
	}
	return fmt.Sprintf("build %s with a constructor that takes %s, with credo.View(), in place of ProvideValue", u.t,
		binding)
}

// workerValueIngressError reports the value a worker's constructor builds,
// internal and with a teardown of its own, that depends on the ingress
// component dep along path; built is the value's type, or nil before it
// exists. When dep is the binding the constructor returns there is no part
// to split: the value is the binding's, which the worker runs through its
// run function.
func workerValueIngressError(u *Unit, dep, built reflect.Type, path []pathStep) error {
	if dep == u.t || dep == built {
		return fmt.Errorf("the constructor of %s returns the ingress component %s, the value of a binding that "+
			"owns it: %s; register the worker's run function, the method expression (%s).Run, in place of the "+
			"constructor", u.worker, dep, formatPath(path), dep)
	}
	return fmt.Errorf("the value the constructor of %s builds is internal and has a teardown of its own, but "+
		"depends on the ingress component %s: %s; split the worker, so that the part with the teardown does "+
		"not use %s", u.worker, dep, formatPath(path), dep)
}

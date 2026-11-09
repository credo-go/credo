package di

import (
	"context"
	"fmt"
	"reflect"
	"strings"
)

// Tier is the drain tier a component belongs to. The zero value is unset and
// plans as internal.
type Tier uint8

const (
	// TierUnset is the zero Tier: the registration's default.
	TierUnset Tier = iota
	// TierIngress is where work enters the process; it stops first,
	// concurrently with the HTTP drain.
	TierIngress
	// TierInternal is everything else; it stops after the HTTP drain, in
	// reverse dependency order.
	TierInternal
)

// String returns "ingress", "internal" or "unset".
func (t Tier) String() string {
	switch t {
	case TierIngress:
		return "ingress"
	case TierInternal:
		return "internal"
	default:
		return "unset"
	}
}

// Options are the registration options of one binding or managed component,
// already checked by the root for the call they were passed to.
type Options struct {
	// Ingress places the component in the ingress tier.
	Ingress bool
	// Borrowed keeps the binding and its readiness contribution and leaves
	// starting and shutting the value down to the caller (ProvideValue only).
	Borrowed bool
	// CloseOnShutdown makes a binding whose type has a Close method a
	// component the App closes after its consumers.
	CloseOnShutdown bool
	// Override replaces an earlier binding of the same type.
	Override bool
	// Name is the credo.ComponentName name of a component: a managed one or
	// a binding's; empty means its type name.
	Name string
	// OptionalReadiness reports the component's readiness entry without
	// letting it gate readiness.
	OptionalReadiness bool
	// IngressRemedy is how the registration that added a managed component
	// places it in the ingress tier, for the finding of an internal component
	// that depends on an ingress one; empty means "credo.Ingress()".
	IngressRemedy string
}

// closeKind is the shape of the Close method a credo.CloseOnShutdown() binding calls.
type closeKind uint8

const (
	closeNone closeKind = iota
	closeErr            // Close() error
	closeVoid           // Close()
	closeCtx            // Close(ctx) error
)

// teardownKind is how a holder releases its resource.
type teardownKind uint8

const (
	teardownNone teardownKind = iota
	teardownShutdown
	teardownClose
)

func (k teardownKind) String() string {
	switch k {
	case teardownShutdown:
		return "Shutdown"
	case teardownClose:
		return "credo.CloseOnShutdown()"
	default:
		return "none"
	}
}

var (
	shutdownerType = reflect.TypeFor[shutdowner]()
	starterType    = reflect.TypeFor[starter]()
	readierType    = reflect.TypeFor[readier]()
	ingressType    = reflect.TypeFor[ingressComponent]()
)

// starter, readier and identifier mirror the root's Starter, Readier and
// ResourceIdentifier capabilities; structural typing makes them the same.
type starter interface {
	Start(ctx context.Context) error
}

type readier interface {
	Ready(ctx context.Context) error
}

type identifier interface {
	ResourceIdentity() any
}

// ingressComponent mirrors the root's IngressComponent: a component type
// that declares the ingress tier.
type ingressComponent interface {
	shutdowner
	IngressTier()
}

// Unit is one entry of the component registry: a DI binding, or a component
// handed to Manage that never becomes a binding. Its plan — whether it is a
// component, its tier, whether the App starts it or asks it for readiness —
// is decided at registration from the type the binding shows, before any
// value exists. Fields are written under Container.mu.
type Unit struct {
	index int
	// t is the binding type, or the type of a managed value or of what a
	// managed constructor returns.
	t    reflect.Type
	name string
	// nameGiven marks a name given with credo.ComponentName, which the
	// registrations of one resource agree on.
	nameGiven bool

	managed bool
	// prov and entry belong to a managed unit; a binding's live in
	// Container.registrations and Container.singletons.
	prov  provider
	entry *singletonEntry
	// valueBinding marks a binding made with a value (ProvideValue).
	valueBinding bool

	component bool
	tier      Tier
	closer    closeKind
	borrowed  bool
	starts    bool
	readies   bool
	// optional reports the unit's readiness entry without letting it gate.
	optional bool
	// ingressRemedy names how to declare the unit ingress; see
	// Options.IngressRemedy.
	ingressRemedy string

	// key is the resource identity of the built value: a comparable token,
	// or the unit itself. nil until the value exists.
	key any
}

// Name is the unit's name in reports: the credo.ComponentName name of the
// unit or of another registration of its resource, otherwise its type.
func (u *Unit) Name() string { return u.name }

// Type is the binding type, or the type of a managed value or constructor
// result.
func (u *Unit) Type() reflect.Type { return u.t }

// Tier is the planned tier; a unit that is not a component plans as internal.
func (u *Unit) Tier() Tier {
	if u.tier == TierIngress {
		return TierIngress
	}
	return TierInternal
}

// Starts reports whether the start walk calls the unit's Start.
func (u *Unit) Starts() bool { return u.starts }

// Readies reports whether the unit's Ready contributes to readiness.
func (u *Unit) Readies() bool { return u.readies }

// Managed reports whether the unit was handed to Manage.
func (u *Unit) Managed() bool { return u.managed }

// label names the unit in a misuse or conflict message.
func (u *Unit) label() string {
	if u.managed {
		return fmt.Sprintf("Manage(%s)", u.name)
	}
	return u.t.String()
}

// planUnit decides, from the type a binding shows, whether it is a
// component, its tier, and whether the App starts it and asks it for
// readiness. It returns the reason an option cannot apply, or "".
func planUnit(u *Unit, o Options) string {
	showsShutdown := u.t.Implements(shutdownerType)
	if o.CloseOnShutdown {
		switch {
		case o.Borrowed:
			return "credo.CloseOnShutdown() and credo.Borrowed() contradict each other: a borrowed value is " +
				"closed by its owner"
		case showsShutdown:
			return fmt.Sprintf("credo.CloseOnShutdown() on %s, which is already a component (it has Shutdown); "+
				"its Shutdown is its teardown", u.t)
		}
		u.closer = closeShape(u.t)
		if u.closer == closeNone {
			return fmt.Sprintf("credo.CloseOnShutdown() on %s, which has no Close() error, Close() or "+
				"Close(context.Context) error method", u.t)
		}
	}
	u.borrowed = o.Borrowed
	u.component = showsShutdown || u.closer != closeNone
	if o.Ingress && !u.component {
		return fmt.Sprintf("credo.Ingress() on %s, which is not a component: it shows no Shutdown method "+
			"and has no credo.CloseOnShutdown(), and the tier is planned before the value exists; give %s a "+
			"Shutdown method, or provide the concrete type that has one", u.t, u.t)
	}
	if o.Name != "" && !u.component {
		return fmt.Sprintf("credo.ComponentName(%q) on %s, which is not a component: it shows no Shutdown "+
			"method and has no credo.CloseOnShutdown(), and the name is planned before the value exists; give "+
			"%s a Shutdown method, or provide the concrete type that has one", o.Name, u.t, u.t)
	}
	if o.OptionalReadiness {
		switch {
		case !u.t.Implements(readierType):
			return fmt.Sprintf("credo.OptionalReadiness() on %s, which plans no readiness entry: its type shows "+
				"no Ready method; give the option to a registration whose type shows Ready", u.t)
		case !u.component && !u.borrowed:
			return fmt.Sprintf("credo.OptionalReadiness() on %s, which plans no readiness entry: it is neither a "+
				"component nor borrowed; give the option to a registration whose type shows Ready and "+
				"Shutdown, or to a borrowed one", u.t)
		}
		u.optional = true
	}
	u.tier = TierInternal
	if o.Ingress || u.t.Implements(ingressType) {
		u.tier = TierIngress
	}
	// A borrowed value is never started; its Ready still contributes. A type
	// that shows Start or Ready but no Shutdown plans them for a value whose
	// Shutdown the type hides: that value is a component, and the walk calls
	// them once it is built and found to be one (RunsCapabilities).
	u.starts = !u.borrowed && u.t.Implements(starterType)
	u.readies = u.t.Implements(readierType)
	return ""
}

// RunsCapabilities reports whether the App calls the Start and Ready the
// unit plans on value, its built value: the unit is a component or
// borrowed, or the value has a Shutdown the binding's type hides. A value of
// a type that shows only Start or Ready, with no Shutdown, is not a
// component, and nothing is called on it.
func (u *Unit) RunsCapabilities(value any) bool {
	return u.component || u.borrowed || u.teardownOf(value) == teardownShutdown
}

// closeShape reports which of the three Close shapes t has.
func closeShape(t reflect.Type) closeKind {
	m, ok := t.MethodByName("Close")
	if !ok {
		return closeNone
	}
	ft := m.Type
	// A method of an interface type has no receiver parameter; one of a
	// concrete type has it first.
	in := 0
	if t.Kind() != reflect.Interface {
		in = 1
	}
	params := ft.NumIn() - in
	switch {
	case params == 0 && ft.NumOut() == 1 && ft.Out(0) == errorType:
		return closeErr
	case params == 0 && ft.NumOut() == 0:
		return closeVoid
	case params == 1 && ft.In(in) == contextType && ft.NumOut() == 1 && ft.Out(0) == errorType:
		return closeCtx
	}
	return closeNone
}

// teardownOf is how holder u releases its resource once its value is
// built: through credo.CloseOnShutdown(), through a Shutdown its value has, or not at
// all. A borrowed holder has no teardown.
func (u *Unit) teardownOf(value any) teardownKind {
	switch {
	case u.borrowed:
		return teardownNone
	case u.closer != closeNone:
		return teardownClose
	}
	if _, ok := value.(shutdowner); ok {
		return teardownShutdown
	}
	return teardownNone
}

// claims reports whether holder u claims its resource's teardown: a
// credo.CloseOnShutdown() binding, a value bound with a teardown, or anything handed
// to Manage. A Shutdown found on a value a constructor built is no claim.
func (u *Unit) claims(value any) bool {
	if u.borrowed {
		return false
	}
	if u.managed || u.closer != closeNone {
		return true
	}
	return u.valueBinding && u.teardownOf(value) != teardownNone
}

// foundOnValue reports whether u became a component only through its built
// value: its binding's type shows no Shutdown, but the value has one.
func (u *Unit) foundOnValue(value any) bool {
	return !u.component && !u.borrowed && u.teardownOf(value) == teardownShutdown
}

// close runs the Close method of a credo.CloseOnShutdown() holder.
func (u *Unit) close(ctx context.Context, value any) error {
	m := reflect.ValueOf(value).MethodByName("Close")
	switch u.closer {
	case closeErr:
		out := m.Call(nil)
		err, _ := out[0].Interface().(error)
		return err
	case closeVoid:
		m.Call(nil)
		return nil
	case closeCtx:
		out := m.Call([]reflect.Value{reflect.ValueOf(&ctx).Elem()})
		err, _ := out[0].Interface().(error)
		return err
	}
	return nil
}

// hiddenCapabilityErrorLocked refuses a component whose binding's type hides
// its value's Start, Ready or declared ingress tier: the plan is taken from
// the type, so the App would never call them or would place the value in
// the internal tier. A borrowed value is its caller's to start, a value
// that is not a component has nothing the App would call, and a capability
// another holder of the same resource plans is called through that holder.
// c.mu must be held.
func (c *Container) hiddenCapabilityErrorLocked(u *Unit, value any, token any, hasToken bool) error {
	if value == nil || u.teardownOf(value) == teardownNone {
		return nil
	}
	_, starts := value.(starter)
	_, readies := value.(readier)
	_, ingress := value.(ingressComponent)
	if hasToken {
		for _, h := range c.resources[token] {
			if h != u && !h.borrowed {
				starts = starts && !h.starts
				readies = readies && !h.readies
				ingress = ingress && h.tier != TierIngress
			}
		}
	}
	if (!starts || u.starts) && (!readies || u.readies) && (!ingress || u.tier == TierIngress) {
		return nil
	}
	vt := reflect.TypeOf(value)
	var hidden []string
	for _, m := range []struct {
		name string
		t    reflect.Type
	}{{"Shutdown", shutdownerType}, {"Start", starterType}, {"Ready", readierType}, {"IngressTier", ingressType}} {
		if vt.Implements(m.t) && !u.t.Implements(m.t) {
			hidden = append(hidden, m.name)
		}
	}
	methods := joinAnd(hidden)
	switch {
	case u.valueBinding:
		return fmt.Errorf("the binding's type %s hides %s of its value, a %s, and the App plans Start, "+
			"Ready and the ingress tier only from the binding's type; bind the value as %s and the interface with "+
			"app.Alias[%s, %s](), or give credo.Borrowed() when the caller starts and stops it",
			u.t, methods, vt, vt, u.t, vt)
	case u.managed:
		return fmt.Errorf("the constructor handed to %s returns a %s, whose %s its result type %s hides, "+
			"and the App plans Start, Ready and the ingress tier only from that type; declare the constructor "+
			"to return %s", u.label(), vt, methods, u.t, vt)
	default:
		return fmt.Errorf("the constructor of %s returns a %s, whose %s the binding's type hides, and the "+
			"App plans Start, Ready and the ingress tier only from the binding's type; declare the constructor "+
			"to return %s and bind %s with app.Alias[%s, %s]()", u.t, vt, methods, vt, u.t, u.t, vt)
	}
}

// joinAnd joins names as "a", "a and b" or "a, b and c".
func joinAnd(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

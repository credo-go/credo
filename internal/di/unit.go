package di

import (
	"context"
	"fmt"
	"reflect"
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
	// Closer makes a binding whose type has a Close method a component the
	// App closes after its consumers.
	Closer bool
	// Override replaces an earlier binding of the same type.
	Override bool
	// Name names a managed component; empty means its type name.
	Name string
	// IngressRemedy is how the registration that added a managed component
	// places it in the ingress tier, for the finding of an internal component
	// that depends on an ingress one; empty means "credo.Ingress()".
	IngressRemedy string
}

// closeKind is the shape of the Close method a credo.Closer() binding calls.
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
		return "credo.Closer()"
	default:
		return "none"
	}
}

var (
	shutdownerType = reflect.TypeFor[shutdowner]()
	starterType    = reflect.TypeFor[starter]()
	readierType    = reflect.TypeFor[readier]()
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
	// ingressRemedy names how to declare the unit ingress; see
	// Options.IngressRemedy.
	ingressRemedy string

	// key is the resource identity of the built value: a comparable token,
	// or the unit itself. nil until the value exists.
	key any

	// steps run on the built value before Start; Seal attaches them.
	steps []StartStep
}

// Name is the unit's name in reports: a managed component's credo.Named
// name, otherwise its type.
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
	if o.Closer {
		switch {
		case o.Borrowed:
			return "credo.Closer() and credo.Borrowed() contradict each other: a borrowed value is " +
				"closed by its owner"
		case showsShutdown:
			return fmt.Sprintf("credo.Closer() on %s, which is already a component (it has Shutdown); "+
				"its Shutdown is its teardown", u.t)
		}
		u.closer = closeShape(u.t)
		if u.closer == closeNone {
			return fmt.Sprintf("credo.Closer() on %s, which has no Close() error, Close() or "+
				"Close(context.Context) error method", u.t)
		}
	}
	u.borrowed = o.Borrowed
	u.component = showsShutdown || u.closer != closeNone
	if o.Ingress && !u.component {
		return fmt.Sprintf("credo.Ingress() on %s, which is not a component: it shows no Shutdown method "+
			"and has no credo.Closer(), and the tier is planned before the value exists; give %s a "+
			"Shutdown method, or provide the concrete type that has one", u.t, u.t)
	}
	u.tier = TierInternal
	if o.Ingress {
		u.tier = TierIngress
	}
	// A borrowed value is never started; its Ready still contributes.
	u.starts = u.component && !u.borrowed && u.t.Implements(starterType)
	u.readies = (u.component || u.borrowed) && u.t.Implements(readierType)
	return ""
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
// built: through credo.Closer(), through a Shutdown its value has, or not at
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
// credo.Closer() binding, a value bound with a teardown, or anything handed
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

// close runs the Close method of a credo.Closer() holder.
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

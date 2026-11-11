package credo

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/credo-go/credo/internal/di"
	internalhealth "github.com/credo-go/credo/internal/health"
)

// Component is a value whose teardown the App owns. A DI singleton is a
// component when it has Shutdown — its binding's type shows the method at
// registration, or its built value has it — and so is a value or constructor
// handed to [App.Manage]. The App shuts components down after their
// consumers, in two tiers ([Tier]), and starts and asks for readiness those
// whose binding's type shows the optional capabilities [Starter] and
// [Readier].
//
// The context carries the drain deadline. A Shutdown that has not returned
// at the deadline is abandoned: the components it depends on stay open, and
// the App reports it in a [*LifecycleError].
//
// Component never gains a method; it grows through optional capabilities.
type Component interface {
	Shutdown(ctx context.Context) error
}

// Starter is the capability of a component the App starts. The start walk
// calls Start once, after the Start of every component it depends on and
// before the App accepts requests. Start returns once the component is
// usable; work that outlives it runs on a goroutine whose context derives
// from context.WithoutCancel(ctx), stopped by the component's Shutdown. The
// context ends when Start returns, or earlier when a shutdown is requested
// during the start phase.
//
// Start is planned from the binding's type, and a component whose binding's
// type hides its value's Start or Ready is refused — a ProvideValue at the
// call, a built value at the start — unless the binding is a [View] or
// [Borrowed]; bind the concrete type and the interface with [App.Alias]. A
// type with Start but no Shutdown is not a component and is never started. A
// resource is started once, through its owner ([ResourceIdentifier]), and a
// borrowed resource is not started.
// A Start that returns an error has released what it opened; the App does not
// shut it down.
type Starter interface {
	Start(ctx context.Context) error
}

// Readier is the capability of a component that answers readiness. The
// readiness endpoint aggregates the Ready of every component whose binding's
// type shows it, under the component's name, with the request's context. A
// borrowed value's Ready contributes too.
type Readier interface {
	Ready(ctx context.Context) error
}

// IngressComponent is the capability of a component type whose tier follows
// from its nature: a server or a consumer, where work enters the process. A
// registration whose type implements it is planned in the ingress tier
// without [Ingress], which stays accepted and redundant. IngressTier is
// never called; the check needs no value, so Finalize validates the tiers
// before anything is built. A type declares the tier only when no
// registration could rightly place it internal: there is no way down. A
// binding whose type hides the method of its value, such as an interface
// view, is refused, as a hidden Start is.
type IngressComponent interface {
	Component
	IngressTier()
}

// ResourceIdentifier names the resource a value holds. Values that share an
// identity are one resource with one owner: the first registration whose
// value claims it — a value with Shutdown, a binding with [CloseOnShutdown]
// or [Borrowed] — through which alone the App starts it, reports its
// readiness and tears it down, after the consumers of every registration
// that holds it; its tier, name and readiness options are the owner's. A
// value handed in claims at its call; a constructor claims when its value is
// built, so of two constructors the first whose value is built owns the
// resource, whatever their registration order. A later registration that
// claims the same resource panics, or fails its build when a constructor
// makes it, unless it is a view ([View], or an [App.Alias]); one that claims
// nothing, such as a second binding of a value with Close beside the one
// that carries [CloseOnShutdown], is a plain holder. Without the method a
// comparable value — a pointer, or a struct over one — is its own identity.
// A value that releases state of its own must not share an identity: a
// wrapper that does holds the handle in a named field instead of embedding
// it.
type ResourceIdentifier interface {
	ResourceIdentity() any
}

// Tier is the drain tier of a component. The ingress tier — where work enters
// the process: servers, consumers, scheduled workers — stops first,
// concurrently with the HTTP drain; the internal tier stops after it, in
// reverse dependency order. The zero Tier is unset and means the
// registration's default. TierIngress and TierInternal encode as the text
// "ingress" and "internal".
type Tier uint8

const (
	// TierIngress is the tier that stops first, with the HTTP drain.
	TierIngress Tier = Tier(di.TierIngress)
	// TierInternal is the default tier, stopped after the HTTP drain.
	TierInternal Tier = Tier(di.TierInternal)
)

// String returns "ingress", "internal", or "" for the unset Tier.
func (t Tier) String() string {
	switch t {
	case TierIngress:
		return "ingress"
	case TierInternal:
		return "internal"
	case 0:
		return ""
	default:
		return fmt.Sprintf("Tier(%d)", uint8(t))
	}
}

// MarshalText encodes the Tier as "ingress", "internal", or "" when unset.
func (t Tier) MarshalText() ([]byte, error) {
	switch t {
	case 0, TierIngress, TierInternal:
		return []byte(t.String()), nil
	}
	return nil, fmt.Errorf("credo: invalid Tier %d", uint8(t))
}

// UnmarshalText decodes "ingress", "internal", or "" (unset).
func (t *Tier) UnmarshalText(text []byte) error {
	switch string(text) {
	case "ingress":
		*t = TierIngress
	case "internal":
		*t = TierInternal
	case "":
		*t = 0
	default:
		return fmt.Errorf("credo: invalid tier %q; want \"ingress\" or \"internal\"", text)
	}
	return nil
}

// RegistrationOption is an option of a component registration: [Ingress],
// [Borrowed], [CloseOnShutdown], [Override], [ComponentName],
// [OptionalReadiness] or [View]. Each call accepts the options that apply to
// it and panics on any other.
type RegistrationOption struct {
	kind optionKind
	name string
}

type optionKind uint8

const (
	optIngress optionKind = iota + 1
	optBorrowed
	optCloseOnShutdown
	optOverride
	optComponentName
	optOptionalReadiness
	optView
)

func (k optionKind) String() string {
	switch k {
	case optIngress:
		return "credo.Ingress()"
	case optBorrowed:
		return "credo.Borrowed()"
	case optCloseOnShutdown:
		return "credo.CloseOnShutdown()"
	case optOverride:
		return "credo.Override()"
	case optComponentName:
		return "credo.ComponentName()"
	case optOptionalReadiness:
		return "credo.OptionalReadiness()"
	case optView:
		return "credo.View()"
	}
	return "a zero RegistrationOption"
}

// Ingress places a component — or a start or stop hook — in the ingress
// tier; a type that implements [IngressComponent] is there without it. On Provide and ProvideValue it panics on a binding that is not a
// component at registration: its type shows no Shutdown and it carries no
// [CloseOnShutdown], and the tier is planned before the value exists.
func Ingress() RegistrationOption { return RegistrationOption{kind: optIngress} }

// Borrowed, on ProvideValue only, keeps the binding and the value's readiness
// contribution and leaves starting and shutting the value down to the caller:
// a pool two Apps in one process share, or a fixture a test suite reuses. The
// borrowed binding owns the resource for its caller ([ResourceIdentifier]),
// so a later registration that holds it is a [View] or panics, and the App
// never starts or shuts it down through either.
func Borrowed() RegistrationOption { return RegistrationOption{kind: optBorrowed} }

// View, on Provide and ProvideValue, declares a binding a view of a resource
// another registration owns ([ResourceIdentifier]): the same pointer under a
// second type, the interface an adapter's constructor returns, or a wrapper
// beside the handle it embeds. The App starts the resource, reports its
// readiness and tears it down through the owner alone, so a view takes none
// of [Ingress], [ComponentName], [OptionalReadiness], [CloseOnShutdown] and
// [Borrowed], and panics at the call with any of them. A view's owner exists
// before the view's value: an owner handed in, with ProvideValue or
// [App.Manage], may be registered before the view or after it, while an
// owner a constructor builds needs the view built from it — an [App.Alias],
// or a constructor that takes the owner's binding, with View. A consumer of
// the view starts after the owner and stops before it, and an internal
// component that uses the view of an ingress one fails [App.Finalize] as if
// it used the owner. A view whose value has Shutdown and whose resource no
// registration owns once that value exists is refused, since nothing would
// start or shut it down — by [App.Finalize] for a value bound with
// ProvideValue, which only a value handed in can own, and by its build for
// one a constructor builds — and so is a value a constructor builds that
// claims a resource a view already holds while no registration owns it. A
// view whose value carries no identity is refused at the call. An interface
// view of a bound concrete type is better written [App.Alias], which
// follows an override of its target.
//
//	app.ProvideValue(db, credo.ComponentName("primary"))
//	app.ProvideValue(PrimaryDB{db}, credo.View())
func View() RegistrationOption { return RegistrationOption{kind: optView} }

// CloseOnShutdown makes a binding whose type has a Close method — Close()
// error, Close(), or Close(ctx) error, which receives the drain deadline — a
// component the App closes on shutdown, after its consumers. It panics on a
// type with none of the three, on a type that is already a component, and
// beside [Borrowed]. Close is never discovered without it: a handle that
// embeds another and closes it, such as bun's over its *sql.DB, would close
// the pool while a sibling component still uses it.
func CloseOnShutdown() RegistrationOption { return RegistrationOption{kind: optCloseOnShutdown} }

// Override, on Provide, ProvideValue and Alias, replaces an earlier binding
// of the same type before Finalize — a direct binding, or an alias of an
// interface — and panics when there is none, so an override that no longer
// matches the wiring fails instead of adding a binding nothing resolves. The
// replaced value never becomes the App's: whoever built it releases it, and
// the type a replaced alias pointed at is dropped at Finalize when nothing
// else uses it.
func Override() RegistrationOption { return RegistrationOption{kind: optOverride} }

// ComponentName names a component — a binding made with [App.Provide] or
// [App.ProvideValue], or a value handed to [App.Manage] — in logs, readiness
// and lifecycle errors; the default is its type as the lifecycle prints it,
// such as *sqldb.DB. The name follows the rules of a health check name. Each
// component has its own name, checked at the call; a resource several
// registrations hold has its owner's, which its readiness entry and its
// teardown carry, and a [View] takes none. The name labels the component and
// is not a DI key: a second value of one type is bound under a wrapper type
// of its own. On a binding it panics when the binding is not a component at
// registration, since the name is planned before the value exists.
func ComponentName(name string) RegistrationOption {
	return RegistrationOption{kind: optComponentName, name: name}
}

// OptionalReadiness reports a component in /ready without letting it gate
// readiness: its Ready is asked under the same deadline, its entry shows its
// own status and latency, and a failure is logged like any failing check,
// while /ready answers 200 with the status "degraded" when every gating
// entry is up. Start and Shutdown are unchanged, so a component whose Start
// fails still fails the start. It applies to [App.Provide], [App.ProvideValue]
// and [App.Manage] wherever the registration plans a readiness entry — its
// type shows Ready, and it is a component or borrowed — and panics elsewhere
// at registration; [App.AddReadinessCheck] takes it too. A resource several
// registrations hold has its owner's entry, so a [View] takes none.
func OptionalReadiness() RegistrationOption { return RegistrationOption{kind: optOptionalReadiness} }

// registrationOptions checks opts against the options call accepts and
// converts them. It panics, naming call, on any other option, a zero option,
// a repeated one, or an empty name.
func registrationOptions(call string, opts []RegistrationOption, accepted ...optionKind) di.Options {
	var o di.Options
	seen := make(map[optionKind]bool, len(opts))
	for _, opt := range opts {
		switch {
		case opt.kind == 0:
			panic("credo: " + call + ": a zero RegistrationOption; use credo.Ingress, credo.Borrowed, " +
				"credo.CloseOnShutdown, credo.Override, credo.ComponentName, credo.OptionalReadiness or credo.View")
		case !containsKind(accepted, opt.kind):
			panic(fmt.Sprintf("credo: %s does not accept %s; it accepts %s", call, opt.kind, kindList(accepted)))
		case seen[opt.kind]:
			panic(fmt.Sprintf("credo: %s: %s given twice", call, opt.kind))
		}
		seen[opt.kind] = true
		switch opt.kind {
		case optIngress:
			o.Ingress = true
		case optBorrowed:
			o.Borrowed = true
		case optCloseOnShutdown:
			o.CloseOnShutdown = true
		case optOverride:
			o.Override = true
		case optComponentName:
			if opt.name == "" {
				panic("credo: " + call + ": credo.ComponentName needs a non-empty name")
			}
			if err := internalhealth.ValidateName(opt.name); err != nil {
				panic(fmt.Sprintf("credo: %s: credo.ComponentName(%q): %v", call, opt.name, err))
			}
			o.Name = opt.name
		case optOptionalReadiness:
			o.OptionalReadiness = true
		case optView:
			o.View = true
		}
	}
	return o
}

// kindList names options as "a", "a or b" or "a, b or c".
func kindList(kinds []optionKind) string {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = k.String()
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

func containsKind(kinds []optionKind, k optionKind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// Manage adds a component that is not a DI binding: a value that has
// Shutdown, or a constructor over DI parameters —
// func(deps...) C or func(deps...) (C, error) — that the start walk builds and
// that never becomes a binding. A function whose own type has Shutdown is a
// value, not a constructor. [ComponentName] names it (default: its type name);
// [Ingress] places it in the ingress tier; [OptionalReadiness] keeps its
// readiness entry from gating. A value has no edges and stops in
// reverse registration order within its tier; a constructor has its
// parameters' edges. A mounted child App is handed to Manage so that its
// start phase runs in the parent's.
//
// A resource has one owner ([ResourceIdentifier]). A value handed to Manage
// owns what it holds, or panics when another registration already owns it.
// The value a constructor builds is checked once it exists: when it holds a
// resource another registration owns, such as a wrapper that embeds a bound
// handle, the start fails, naming the remedy — drop the Manage call when the
// constructor returns that registration's own value, or hold the handle in a
// named field so that the value is a resource of its own.
//
// Manage panics on a value without Shutdown, on a duplicate name, on a
// resource another registration owns, and after Finalize or shutdown.
func (app *App) Manage(v any, opts ...RegistrationOption) {
	o := registrationOptions("App.Manage", opts, optIngress, optComponentName, optOptionalReadiness)
	if v != nil && reflect.TypeOf(v) == reflect.TypeFor[*App]() && v.(*App) == app {
		panic("credo: App.Manage: an App cannot manage itself")
	}
	_, err := app.container.Manage(v, o)
	panicOnMisuse(err)
}

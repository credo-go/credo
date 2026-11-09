package credo

import (
	"context"
	"errors"
	"log/slog"
	"reflect"

	"github.com/credo-go/credo/internal/di"
)

// Provide registers a constructor in the application's DI container. The
// binding's type is the constructor's first result type: func(deps...) T or
// func(deps...) (T, error) binds T. Its parameters are themselves resolved
// from the container. It runs at most once, on the first resolution after
// [App.Finalize].
//
//	app.Provide(NewUserService) // binds *UserService
//
// A constructor declared to return a concrete type binds that type; the
// interfaces it serves are bound with [App.Alias]. One declared to return an
// interface binds the interface.
//
// Because Go cannot express "a function with arbitrary parameters" in the
// type system, constructor is typed any: a signature mistake (not a
// function, no result, a second result that is not an error) panics here, at
// the call, not at compile time. So do a nil constructor, a second binding of
// T — a direct one, or an alias of T — without [Override], and a call after
// [App.Finalize] or after shutdown began; each is misuse known at the call
// site (see "Panics and Errors" in the package documentation). The dependency
// graph itself is validated at [App.Finalize]. A constructor that captures
// app and calls [App.Resolve] inside its body is unsupported: such a
// dependency is invisible to graph validation, cycle detection and
// dependency-ordered shutdown.
//
// A binding whose type shows Shutdown is a component: the App shuts its value
// down after its consumers, and starts it and asks it for readiness when the
// type also shows Start or Ready ([Component]). A value whose Shutdown only
// the built value shows is shut down too, in the internal tier. A component
// whose built value has a Start or Ready the binding's type hides fails the
// start once it is built: declare the constructor to return the concrete
// type, and bind the interface with [App.Alias]. The options are [Ingress],
// [Closer] and [Override]; each panics on misuse at the call.
//
// Registration is sequential: call Provide from the goroutine that builds the
// App, before it runs.
func (app *App) Provide(constructor any, opts ...RegistrationOption) {
	o := registrationOptions("App.Provide", opts, optIngress, optCloser, optOverride)
	panicOnMisuse(app.container.ProvideWith(constructor, o))
}

// ProvideValue registers a pre-built value for type T as a Singleton. The App
// owns the value from then on: if it is a [Component] it is shut down during
// the drain, unless [Borrowed] leaves it to the caller. The options are
// [Ingress], [Borrowed], [Closer] and [Override]; [Override] is how a
// composition root or a test swaps an earlier binding — a direct one, or an
// alias of T — for a stub, and the value it replaces never becomes the App's.
// A second binding of T without [Override], a call after [App.Finalize] or
// after shutdown began, a value whose resource another holder owns
// differently, and a component whose Start or Ready the type T hides panic,
// like [App.Provide]; a hidden capability's remedies are binding the
// concrete type with an [App.Alias] of T, or [Borrowed] when the caller
// starts and stops the value.
//
//	app.ProvideValue[*Logger](logger)
func (app *App) ProvideValue[T any](value T, opts ...RegistrationOption) {
	o := registrationOptions("App.ProvideValue", opts, optIngress, optBorrowed, optCloser, optOverride)
	panicOnMisuse(app.container.ProvideValueWith[T](value, o))
}

// Has reports whether type T is registered, directly or through [App.Alias].
// It is the one non-resolving presence probe: it never runs a constructor and
// makes no claim that the instance is healthy or usable, and the result is a
// snapshot of the registrations made so far, not a reservation for a later
// one. Use it in a composition root to ask whether an optional module was
// wired.
func (app *App) Has[T any]() bool {
	return app.container.Has[T]()
}

// Resolve retrieves an instance of type T from the application's DI
// container. It is admitted only after [App.Finalize] (Run and ServeHTTP
// finalize implicitly) and panics when called before it: constructors run at
// first resolution, exactly once, and a constructor panic is returned as a
// [DIPanicError] to every caller. After a failed Finalize, Resolve returns the
// Finalize error. Once shutdown has reached DI teardown, Resolve returns an
// error wrapping [ErrDIClosed]. After Finalize, Resolve is safe for concurrent
// use.
//
// Resolve is primarily intended for bootstrap/composition-root code after
// Finalize; runtime calls remain available, but Credo's recommended
// application pattern is constructor injection. Stop hooks ([App.OnStop])
// must not resolve: take their dependencies at registration time instead.
// [App.OnStart] hooks run after Finalize and before traffic, and may
// resolve.
//
//	svc, err := app.Resolve[*UserService]()
func (app *App) Resolve[T any]() (T, error) {
	app.noteResolveDuringDrain(reflect.TypeFor[T]())
	v, err := app.container.Resolve[T]()
	panicOnMisuse(err)
	return v, err
}

// MustResolve is like [App.Resolve] but panics on error. It is primarily
// intended for bootstrap/composition-root code. A constructor panic surfaces
// here as a panic whose value is the [DIPanicError], not the original value.
func (app *App) MustResolve[T any]() T {
	v, err := app.Resolve[T]()
	if err != nil {
		panic(err)
	}
	return v
}

// ResolveAll retrieves all singletons bound to interface type T via
// [App.BindMany], preserving bind order. When no bindings exist, it returns an
// empty slice and nil error. The same phase rules as [App.Resolve] apply.
func (app *App) ResolveAll[T any]() ([]T, error) {
	app.noteResolveDuringDrain(reflect.TypeFor[T]())
	v, err := app.container.ResolveAll[T]()
	panicOnMisuse(err)
	return v, err
}

// MustResolveAll is like [App.ResolveAll] but panics on error.
func (app *App) MustResolveAll[T any]() []T {
	v, err := app.ResolveAll[T]()
	if err != nil {
		panic(err)
	}
	return v
}

// panicOnMisuse panics with err when the container rejected a call because of
// how it was made — out of phase or with arguments it cannot accept — naming
// the App method that was called. Any other error, and nil, pass through.
func panicOnMisuse(err error) {
	if m, ok := errors.AsType[*di.MisuseError](err); ok {
		panic("credo: App." + m.Call + ": " + m.Reason)
	}
}

// noteResolveDuringDrain emits a Debug diagnostic when a resolution happens
// while the App is stopping. It is not a hook violation: an in-flight request
// past the HTTP drain may legitimately resolve. It is cheap enough for the
// resolve path (one atomic load) and helps locate hooks that resolve during
// teardown, which is unsupported.
func (app *App) noteResolveDuringDrain(t reflect.Type) {
	if app.lifecycle.currentState() != stateStopping {
		return
	}
	app.Logger().LogAttrs(context.Background(), slog.LevelDebug,
		"credo: Resolve during drain", slog.String("type", t.String()))
}

// Alias creates a type alias so that resolving interface I via [App.Resolve]
// returns the singleton registered for concrete type T. An alias is a binding
// of I: a later direct binding of I panics unless it carries [Override],
// which replaces the alias. I must be an interface, T must implement I and
// be registered already, and I must have neither a binding nor an alias;
// otherwise Alias panics, as it does after [App.Finalize] or after shutdown
// began. [Override], its one option, points I at T in place of its earlier
// binding or alias.
//
// The type an overridden alias pointed at stays registered until
// [App.Finalize], which drops it when no other alias, [App.BindMany] entry or
// constructor parameter reaches it: it is never built or started, and
// resolving it reports the override.
//
//	app.Provide(NewPgUserRepo) // binds *PgUserRepo
//	app.Alias[UserRepo, *PgUserRepo]()
func (app *App) Alias[I, T any](opts ...RegistrationOption) {
	o := registrationOptions("App.Alias", opts, optOverride)
	panicOnMisuse(app.container.AliasWith[I, T](o))
}

// BindMany adds concrete type T to the ordered collection for interface I.
// I must be an interface, T must be a registered concrete type that
// implements I, and the pair must be new; otherwise BindMany panics, as it
// does after [App.Finalize] or after shutdown began.
func (app *App) BindMany[I, T any]() {
	panicOnMisuse(app.container.BindMany[I, T]())
}

// Finalize freezes the DI container and validates the dependency graph.
// After Finalize, a Provide, ProvideValue, Alias or BindMany call panics, and
// [App.Resolve] becomes available. Finalize is DI-only: routes, hooks,
// renderers and other HTTP registrations stay open until the App prepares to
// serve, so controllers built from resolved services can still be wired
// afterwards.
//
// Finalize returns what only the whole graph reveals, joined in registration
// order with the same text on every run: each missing dependency with its
// whole path from the registration that needs it, for example
//
//	di: missing dependency: *app.OrderService → *app.PaymentClient → *http.Client (not registered); ...
//
// each circular dependency, constructors that take a context.Context, and
// each internal component that depends on an ingress one, with the path and
// both remedies.
//
// Finalize is idempotent. If not called explicitly, the Run* entry points and
// the first [App.ServeHTTP] call it implicitly.
//
//	if err := app.Finalize(); err != nil {
//		log.Fatal(err)
//	}
func (app *App) Finalize() error {
	return app.container.Seal()
}

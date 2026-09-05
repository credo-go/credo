package credo

import (
	"context"
	"log/slog"
	"reflect"
)

// Provide registers a constructor for type T in the application's DI
// container. The constructor can accept any number of parameters that are
// themselves registered, and must return T or (T, error). It runs at most
// once, on the first resolution after [App.Finalize].
//
//	app.Provide[*UserService](NewUserService)
//
// Because Go cannot express "a function with arbitrary parameters returning
// T" in the type system, constructor is typed any: signature mistakes (wrong
// return type, not a function) are reported as an error at registration time,
// not at compile time. The dependency graph itself is validated at
// [App.Finalize]. A constructor that captures app and calls [App.Resolve]
// inside its body is unsupported: such a dependency is invisible to graph
// validation, cycle detection and dependency-ordered shutdown.
func (app *App) Provide[T any](constructor any) error {
	return app.container.Provide[T](constructor)
}

// MustProvide is like [App.Provide] but panics on error.
func (app *App) MustProvide[T any](constructor any) {
	app.container.MustProvide[T](constructor)
}

// ProvideValue registers a pre-built value for type T as a Singleton. The
// container owns the value from then on: if it implements [Shutdowner] it is
// closed during teardown, unless a later successful [App.Replace] hands it
// back to the caller.
//
//	app.ProvideValue[*Logger](logger)
func (app *App) ProvideValue[T any](value T) error {
	return app.container.ProvideValue[T](value)
}

// ProvideProtectedValue registers a pre-built singleton whose binding cannot
// later be overwritten through [App.Replace]. It is intended for integrations
// that publish a value together with external lifecycle or health state and
// therefore cannot safely allow the DI binding to diverge afterward.
func (app *App) ProvideProtectedValue[T any](value T) error {
	return app.container.ProvideProtectedValue[T](value)
}

// ProtectBinding prevents [App.Replace] from overwriting the existing direct
// registration for T. It is idempotent and rejected after Finalize. The method
// does not resolve or otherwise instantiate T. When one expected value is
// supplied, protection is a compare-and-protect operation: it succeeds only if
// the bound prebuilt value is still that same comparable value. Integrations
// that need to read, validate and protect in one step use [App.AdoptValue].
func (app *App) ProtectBinding[T any](expected ...T) error {
	return app.container.ProtectBinding[T](expected...)
}

// CanProvideValue reports whether [App.ProvideValue] could currently register
// type T. It checks only whether the DI container is finalized or T already has
// a direct registration, and does not mutate or reserve the registration.
//
// The result is a point-in-time preflight. A later ProvideValue call can still
// fail if another registration or finalization occurs in between.
func (app *App) CanProvideValue[T any]() error {
	return app.container.CanProvideValue[T]()
}

// MustProvideValue is like [App.ProvideValue] but panics on error.
func (app *App) MustProvideValue[T any](value T) {
	app.container.MustProvideValue[T](value)
}

// Has reports whether type T is registered, directly or through [App.Alias].
// It never constructs, adopts or protects anything and makes no claim that
// the instance is healthy or usable; the result is a snapshot, not a
// reservation for a later registration.
func (app *App) Has[T any]() bool {
	return app.container.Has[T]()
}

// AdoptValue reads the pre-built value bound to T during the registration
// phase, validates it, and atomically protects that same binding against
// [App.Replace] before returning it. It is the one registration-time read
// Credo supports: constructors run only after [App.Finalize], so a constructor
// binding for T is rejected with an explanatory error without being invoked.
//
// A nil validate accepts every value. Validation failure leaves the binding
// unprotected and repairable through Replace; a Replace or Finalize that
// wins while validation runs makes the adoption fail rather than protecting
// or returning a stale instance. Framework integrations (store, worker) use
// it to take ownership of a value the composition root supplied ahead of
// them. Use [App.Has] for a plain existence check.
func (app *App) AdoptValue[T any](validate func(T) error) (T, error) {
	return app.container.AdoptValue[T](validate)
}

// Replace registers a pre-built value for type T, overwriting any existing
// unprotected registration. Unlike [App.ProvideValue], a duplicate T is
// normally replaced. Bindings published by [App.ProvideProtectedValue], locked
// through [App.ProtectBinding] or adopted through [App.AdoptValue] reject
// replacement because external lifecycle state depends on their identity.
//
// On success the container owns the new value and no longer tracks the
// superseded instance: Replace returns that instance with existed == true when
// a previously created instance existed (a pre-built value), and the caller
// assumes its cleanup responsibility. A superseded constructor binding that
// never ran yields the zero value and false; Replace never constructs an old
// provider merely to return it. When the returned instance implements
// [Shutdowner] a Warn log names the type, as a reminder that the container
// will not close it. A rejected replacement changes neither the binding nor
// ownership.
//
// Replace is intended for composition-root overrides and tests where a real
// binding is swapped for a stub or fake. Because the replacement is a value,
// it carries no dependencies and stays valid during [App.Finalize]. Replace is
// rejected after the container is finalized or shut down.
//
// In tests, the github.com/credo-go/credo/testutil package builds on Replace
// through its WithOverride option.
//
//	old, existed, err := app.Replace[UserRepo](mockRepo)
func (app *App) Replace[T any](value T) (old T, existed bool, err error) {
	old, existed, err = app.container.Replace[T](value)
	if err == nil && existed {
		app.noteReplacedShutdowner(reflect.TypeFor[T](), any(old))
	}
	return old, existed, err
}

// MustReplace is like [App.Replace] but panics on error. It returns the same
// previous-instance information.
func (app *App) MustReplace[T any](value T) (old T, existed bool) {
	old, existed, err := app.Replace[T](value)
	if err != nil {
		panic(err)
	}
	return old, existed
}

// noteReplacedShutdowner logs the ownership transfer of a superseded instance
// that has a Shutdown method. It is a diagnostic, not the transfer mechanism.
func (app *App) noteReplacedShutdowner(t reflect.Type, old any) {
	if _, ok := old.(Shutdowner); !ok {
		return
	}
	app.Logger().LogAttrs(context.Background(), slog.LevelWarn,
		"credo: Replace superseded a Shutdowner; the caller now owns its cleanup",
		slog.String("type", t.String()))
}

// Resolve retrieves an instance of type T from the application's DI
// container. It is admitted only after [App.Finalize] (Run and ServeHTTP
// finalize implicitly): constructors run at first resolution, exactly once,
// and a constructor panic is returned as a [DIPanicError] to every caller.
// Once shutdown has reached DI teardown, Resolve returns an error wrapping
// [ErrDIClosed].
//
// Resolve is primarily intended for bootstrap/composition-root code after
// Finalize; runtime calls remain available, but Credo's recommended
// application pattern is constructor injection. Lifecycle hooks must not
// resolve: take dependencies at registration time instead.
//
//	svc, err := app.Resolve[*UserService]()
func (app *App) Resolve[T any]() (T, error) {
	app.noteResolveDuringDrain(reflect.TypeFor[T]())
	return app.container.Resolve[T]()
}

// MustResolve is like [App.Resolve] but panics on error. It is primarily
// intended for bootstrap/composition-root code. A constructor panic surfaces
// here as a panic whose value is the [DIPanicError], not the original value.
func (app *App) MustResolve[T any]() T {
	app.noteResolveDuringDrain(reflect.TypeFor[T]())
	return app.container.MustResolve[T]()
}

// ResolveAll retrieves all singletons bound to interface type T via
// [App.BindMany], preserving bind order. When no bindings exist, it returns an
// empty slice and nil error. The same phase rules as [App.Resolve] apply.
func (app *App) ResolveAll[T any]() ([]T, error) {
	app.noteResolveDuringDrain(reflect.TypeFor[T]())
	return app.container.ResolveAll[T]()
}

// MustResolveAll is like [App.ResolveAll] but panics on error.
func (app *App) MustResolveAll[T any]() []T {
	app.noteResolveDuringDrain(reflect.TypeFor[T]())
	return app.container.MustResolveAll[T]()
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
// returns the singleton registered for concrete type T. I must be an
// interface, T must implement I, and T must already be registered.
//
//	app.Alias[UserRepo, *PgUserRepo]()
func (app *App) Alias[I, T any]() error {
	return app.container.Alias[I, T]()
}

// MustAlias is like [App.Alias] but panics on error.
func (app *App) MustAlias[I, T any]() {
	app.container.MustAlias[I, T]()
}

// BindMany adds concrete type T to the ordered collection for interface I.
// I must be an interface, T must be a registered concrete type, and T must
// implement I.
func (app *App) BindMany[I, T any]() error {
	return app.container.BindMany[I, T]()
}

// MustBindMany is like [App.BindMany] but panics on error.
func (app *App) MustBindMany[I, T any]() {
	app.container.MustBindMany[I, T]()
}

// Finalize freezes the DI container and validates the dependency graph.
// After Finalize, no more Provide, ProvideValue, ProvideProtectedValue,
// ProtectBinding, AdoptValue, Replace, Alias, or BindMany calls are allowed,
// and [App.Resolve] becomes available. Finalize is DI-only: routes, hooks,
// renderers and other HTTP registrations stay open until the App prepares to
// serve, so controllers built from resolved services can still be wired
// afterwards.
//
// Finalize is idempotent. If not called explicitly, the Run* entry points and
// the first [App.ServeHTTP] call it implicitly.
//
//	app.Finalize()
func (app *App) Finalize() error {
	return app.container.Seal()
}

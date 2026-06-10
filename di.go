package credo

import "github.com/credo-go/credo/internal/di"

// Provide registers a constructor for type T in the application's DI
// container. The constructor can accept any number of parameters that are
// themselves registered, and must return T or (T, error).
//
//	credo.Provide[*UserService](app, NewUserService)
//
// Because Go cannot express "a function with arbitrary parameters returning
// T" in the type system, constructor is typed any: signature mistakes (wrong
// return type, not a function) are reported as an error at registration time,
// not at compile time. The dependency graph itself is still validated at
// [Finalize]. For a constructor checked entirely by the compiler, see
// [ProvideFunc].
func Provide[T any](app *App, constructor any) error {
	return di.Provide[T](app.container, constructor)
}

// MustProvide is like [Provide] but panics on error.
func MustProvide[T any](app *App, constructor any) {
	di.MustProvide[T](app.container, constructor)
}

// ProvideFunc registers a compile-time-checked constructor for type T.
// Unlike [Provide], whose constructor parameter is typed any and inspected at
// registration time, fn's signature is enforced by the compiler — and T is
// inferred from it. fn receives the App and resolves its own dependencies:
//
//	credo.ProvideFunc(app, func(app *credo.App) (*UserService, error) {
//		repo, err := credo.Resolve[*UserRepository](app)
//		if err != nil {
//			return nil, err
//		}
//		return NewUserService(app.NewInfra("UserService"), repo), nil
//	})
//
// Like [Provide], fn runs lazily on first resolution, exactly once, and the
// instance participates in reverse-order shutdown.
//
// Trade-offs versus [Provide]:
//   - fn is opaque to the container. Dependencies resolved inside fn are not
//     part of [Finalize]'s graph validation — a missing dependency surfaces
//     at first resolution instead. Cycles entered through fn are likewise
//     invisible to the resolver's cycle detection (the same holds for any
//     constructor closure that captures app and calls [Resolve]).
//   - [Infra] is not auto-injected; use [App.NewInfra] inside fn as shown.
func ProvideFunc[T any](app *App, fn func(*App) (T, error)) error {
	if fn == nil {
		// Normalize to the internal error message without calling fn.
		return di.ProvideFunc[T](app.container, nil)
	}
	return di.ProvideFunc[T](app.container, func() (T, error) { return fn(app) })
}

// MustProvideFunc is like [ProvideFunc] but panics on error.
func MustProvideFunc[T any](app *App, fn func(*App) (T, error)) {
	if err := ProvideFunc[T](app, fn); err != nil {
		panic(err)
	}
}

// ProvideValue registers a pre-built value for type T as a Singleton.
//
//	credo.ProvideValue[*Logger](app, logger)
func ProvideValue[T any](app *App, value T) error {
	return di.ProvideValue[T](app.container, value)
}

// MustProvideValue is like [ProvideValue] but panics on error.
func MustProvideValue[T any](app *App, value T) {
	di.MustProvideValue[T](app.container, value)
}

// Replace registers a pre-built value for type T, overwriting any existing
// registration. Unlike [ProvideValue], it does not return an error when T is
// already registered: it replaces the binding and discards any cached
// singleton.
//
// Replace is intended for composition-root overrides and tests where a real
// binding is swapped for a stub or fake. Because the replacement is a value,
// it carries no dependencies and stays valid during [Finalize]. Replace is
// rejected after the container is finalized.
//
// In tests, the github.com/credo-go/credo/testutil package builds on Replace
// through its WithOverride option.
//
//	credo.Replace[UserRepo](app, mockRepo)
func Replace[T any](app *App, value T) error {
	return di.Replace[T](app.container, value)
}

// MustReplace is like [Replace] but panics on error.
func MustReplace[T any](app *App, value T) {
	di.MustReplace[T](app.container, value)
}

// Resolve retrieves an instance of type T from the application's DI
// container. Resolve is primarily intended for bootstrap/composition-root
// code; runtime calls remain available, but Credo's recommended application
// pattern is constructor injection.
//
//	svc, err := credo.Resolve[*UserService](app)
func Resolve[T any](app *App) (T, error) {
	return di.Resolve[T](app.container)
}

// MustResolve is like [Resolve] but panics on error. It is primarily intended
// for bootstrap/composition-root code.
func MustResolve[T any](app *App) T {
	return di.MustResolve[T](app.container)
}

// ResolveAll retrieves all singletons bound to interface type T via
// [BindMany], preserving bind order. When no bindings exist, it returns an
// empty slice and nil error.
func ResolveAll[T any](app *App) ([]T, error) {
	return di.ResolveAll[T](app.container)
}

// MustResolveAll is like [ResolveAll] but panics on error.
func MustResolveAll[T any](app *App) []T {
	return di.MustResolveAll[T](app.container)
}

// Alias creates a type alias so that Resolve[I] returns the singleton
// registered for concrete type T. I must be an interface, T must implement
// I, and T must already be registered.
//
//	credo.Alias[UserRepo, *PgUserRepo](app)
func Alias[I, T any](app *App) error {
	return di.Alias[I, T](app.container)
}

// MustAlias is like [Alias] but panics on error.
func MustAlias[I, T any](app *App) {
	di.MustAlias[I, T](app.container)
}

// BindMany adds concrete type T to the ordered collection for interface I.
// I must be an interface, T must be a registered concrete type, and T must
// implement I.
func BindMany[I, T any](app *App) error {
	return di.BindMany[I, T](app.container)
}

// MustBindMany is like [BindMany] but panics on error.
func MustBindMany[I, T any](app *App) {
	di.MustBindMany[I, T](app.container)
}

// Finalize freezes the DI container and validates the dependency graph.
// After Finalize, no more Provide, ProvideFunc, ProvideValue, Replace, Alias,
// or BindMany calls are allowed.
// Finalize is idempotent. If not called explicitly, Run and RunTLS call it
// implicitly.
//
//	credo.Finalize(app)
func Finalize(app *App) error {
	return app.container.Seal()
}

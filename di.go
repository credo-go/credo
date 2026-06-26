package credo

import "github.com/credo-go/credo/internal/di"

// Provide registers a constructor for type T in the application's DI
// container. The constructor can accept any number of parameters that are
// themselves registered, and must return T or (T, error).
//
//	app.Provide[*UserService](NewUserService)
//
// Because Go cannot express "a function with arbitrary parameters returning
// T" in the type system, constructor is typed any: signature mistakes (wrong
// return type, not a function) are reported as an error at registration time,
// not at compile time. The dependency graph itself is still validated at
// [App.Finalize]. For a factory checked entirely by the compiler, see
// [App.ProvideFactory].
func (app *App) Provide[T any](constructor any) error {
	return di.Provide[T](app.container, constructor)
}

// MustProvide is like [App.Provide] but panics on error.
func (app *App) MustProvide[T any](constructor any) {
	di.MustProvide[T](app.container, constructor)
}

// ProvideFactory registers a compile-time-checked factory for type T.
// Unlike [App.Provide], whose constructor parameter is typed any and inspected
// at registration time, fn's signature is enforced by the compiler — and T is
// inferred from it. fn receives the App and resolves its own dependencies:
//
//	app.ProvideFactory(func(app *credo.App) (*UserService, error) {
//		repo, err := app.Resolve[*UserRepository]()
//		if err != nil {
//			return nil, err
//		}
//		return NewUserService(app.NewInfra("UserService"), repo), nil
//	})
//
// Like [App.Provide], fn runs lazily on first resolution, exactly once, and the
// instance participates in reverse-order shutdown.
//
// Trade-offs versus [App.Provide]:
//   - fn is opaque to the container. Dependencies resolved inside fn are not
//     part of [App.Finalize]'s graph validation — a missing dependency surfaces
//     at first resolution instead. Cycles entered through fn are likewise
//     invisible to the resolver's cycle detection (the same holds for any
//     constructor closure that captures app and calls [App.Resolve]).
//   - [Infra] is not auto-injected; use [App.NewInfra] inside fn as shown.
func (app *App) ProvideFactory[T any](fn func(*App) (T, error)) error {
	if fn == nil {
		// Normalize to the internal error message without calling fn.
		return di.ProvideFactory[T](app.container, nil)
	}
	return di.ProvideFactory[T](app.container, func() (T, error) { return fn(app) })
}

// MustProvideFactory is like [App.ProvideFactory] but panics on error.
func (app *App) MustProvideFactory[T any](fn func(*App) (T, error)) {
	if err := app.ProvideFactory[T](fn); err != nil {
		panic(err)
	}
}

// ProvideValue registers a pre-built value for type T as a Singleton.
//
//	app.ProvideValue[*Logger](logger)
func (app *App) ProvideValue[T any](value T) error {
	return di.ProvideValue[T](app.container, value)
}

// MustProvideValue is like [App.ProvideValue] but panics on error.
func (app *App) MustProvideValue[T any](value T) {
	di.MustProvideValue[T](app.container, value)
}

// Replace registers a pre-built value for type T, overwriting any existing
// registration. Unlike [App.ProvideValue], it does not return an error when T
// is already registered: it replaces the binding and discards any cached
// singleton.
//
// Replace is intended for composition-root overrides and tests where a real
// binding is swapped for a stub or fake. Because the replacement is a value,
// it carries no dependencies and stays valid during [App.Finalize]. Replace is
// rejected after the container is finalized.
//
// In tests, the github.com/credo-go/credo/testutil package builds on Replace
// through its WithOverride option.
//
//	app.Replace[UserRepo](mockRepo)
func (app *App) Replace[T any](value T) error {
	return di.Replace[T](app.container, value)
}

// MustReplace is like [App.Replace] but panics on error.
func (app *App) MustReplace[T any](value T) {
	di.MustReplace[T](app.container, value)
}

// Resolve retrieves an instance of type T from the application's DI
// container. Resolve is primarily intended for bootstrap/composition-root
// code; runtime calls remain available, but Credo's recommended application
// pattern is constructor injection.
//
//	svc, err := app.Resolve[*UserService]()
func (app *App) Resolve[T any]() (T, error) {
	return di.Resolve[T](app.container)
}

// MustResolve is like [App.Resolve] but panics on error. It is primarily
// intended for bootstrap/composition-root code.
func (app *App) MustResolve[T any]() T {
	return di.MustResolve[T](app.container)
}

// ResolveAll retrieves all singletons bound to interface type T via
// [App.BindMany], preserving bind order. When no bindings exist, it returns an
// empty slice and nil error.
func (app *App) ResolveAll[T any]() ([]T, error) {
	return di.ResolveAll[T](app.container)
}

// MustResolveAll is like [App.ResolveAll] but panics on error.
func (app *App) MustResolveAll[T any]() []T {
	return di.MustResolveAll[T](app.container)
}

// Alias creates a type alias so that resolving interface I via [App.Resolve]
// returns the singleton registered for concrete type T. I must be an
// interface, T must implement I, and T must already be registered.
//
//	app.Alias[UserRepo, *PgUserRepo]()
func (app *App) Alias[I, T any]() error {
	return di.Alias[I, T](app.container)
}

// MustAlias is like [App.Alias] but panics on error.
func (app *App) MustAlias[I, T any]() {
	di.MustAlias[I, T](app.container)
}

// BindMany adds concrete type T to the ordered collection for interface I.
// I must be an interface, T must be a registered concrete type, and T must
// implement I.
func (app *App) BindMany[I, T any]() error {
	return di.BindMany[I, T](app.container)
}

// MustBindMany is like [App.BindMany] but panics on error.
func (app *App) MustBindMany[I, T any]() {
	di.MustBindMany[I, T](app.container)
}

// Finalize freezes the DI container and validates the dependency graph.
// After Finalize, no more Provide, ProvideFactory, ProvideValue, Replace,
// Alias, or BindMany calls are allowed.
// Finalize is idempotent. If not called explicitly, the Run* entry points call
// it implicitly.
//
//	app.Finalize()
func (app *App) Finalize() error {
	return app.container.Seal()
}

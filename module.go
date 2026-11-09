package credo

// Module is a part of an application: its services, then its handlers.
// [App.UseModules] runs every module's Services before any module's
// Handlers, so the first Resolve inside a Handlers method comes after every
// registration and the implicit Finalize it triggers is safe.
//
// Both methods are required; a module with nothing for one phase leaves that
// method empty. A module composed of others calls their methods from its own.
type Module interface {
	// Services registers the module's services: Provide, ProvideValue,
	// Manage, workers and the other registrations of the container spec's
	// documented order before resolving.
	Services(app *App)

	// Handlers registers the module's entry points — routes, and anything
	// else that registers through the App as routes do — and may Resolve
	// the services they close over.
	Handlers(app *App)
}

// UseModules runs every module's Services in order, then every module's
// Handlers in order:
//
//	app.UseModules(users.Module{}, billing.Module{})
//
// The services of a later module are therefore registered before the handlers
// of an earlier one resolve. A registration from a Handlers method after the
// first Resolve panics as every late registration does, naming the call that
// finalized the container.
//
// UseModules is called once: a second call would register its services after
// the first call's handlers had finalized the container, so it panics, naming
// the remedy of passing every module to one call. It panics too, like every
// Use* registration, when called after preparation or shutdown.
func (app *App) UseModules(mods ...Module) {
	app.checkFrozen("App.UseModules")
	if app.modulesUsed {
		panic("credo: App.UseModules called a second time; pass every module to one " +
			"UseModules call, which registers all their services before any of their " +
			"handlers resolve")
	}
	app.modulesUsed = true
	for _, m := range mods {
		m.Services(app)
	}
	for _, m := range mods {
		m.Handlers(app)
	}
}

package credo

import (
	"context"
	"log/slog"
	"net"
)

// State returns the current lifecycle state as a string.
func (app *App) State() string {
	return app.lifecycle.currentState().String()
}

// IsRunning reports whether the server is in the running state.
func (app *App) IsRunning() bool {
	return app.lifecycle.currentState() == stateRunning
}

// OnStart registers a start hook: an anonymous component of a tier, internal
// unless [Ingress] is passed. Start hooks run FIFO after their tier's
// components have started — so a hook can use what they provide — and before
// the App accepts requests. Under managed serving the listener is already
// bound, so [App.Addr] is available.
//
// The ctx ends when the hook returns, or earlier when a shutdown is requested
// during the start phase; work that outlives the hook belongs to a component
// with Start and Shutdown. A hook that returns an error or panics fails the
// start: nothing further starts, what was built is rolled back, and the entry
// point returns a *LifecycleError. A hook is a leaf action: a resource with
// its own teardown is a component — handed to [App.Manage], or bound with
// [Closer].
//
// Typical uses include cache warm-up. The store/sqldb migration wrapper plugs
// in directly as app.OnStart(db.Migrate) for development and deliberate
// single-replica deployments; multi-replica production should use one
// deadline-bounded pre-deploy migration job instead.
//
// Must be called before the App is prepared; panics for a nil hook, an option
// other than Ingress, or after the App is frozen.
func (app *App) OnStart(fn func(ctx context.Context) error, opts ...RegistrationOption) {
	app.registerStageHook("App.OnStart", fn, opts, &app.lifecycle.onStart)
}

// OnStop registers a stop hook: an anonymous component of a tier, internal
// unless [Ingress] is passed. Stop hooks run LIFO before their tier's
// components stop, so a hook can still use them: the ingress tier's hooks
// beside the HTTP drain, the internal tier's after it. The ctx carries the
// drain deadline; a hook that has not returned by then is abandoned and
// reported, and an error or a panic is reported without stopping the drain.
//
// Stop hooks run on every teardown — the drain, bootstrap teardown, and the
// rollback of a failed or interrupted start — so a hook must tolerate a start
// that never reached its counterpart. Process-level cleanup that must outlive
// every component belongs after Run returns.
//
// Must be called before the App is prepared; panics for a nil hook, an option
// other than Ingress, or after the App is frozen.
func (app *App) OnStop(fn func(ctx context.Context) error, opts ...RegistrationOption) {
	app.registerStageHook("App.OnStop", fn, opts, &app.lifecycle.onStop)
}

// registerStageHook checks a start or stop hook and appends it with its tier.
func (app *App) registerStageHook(
	label string, fn func(ctx context.Context) error, opts []RegistrationOption, hooks *[]stageHook,
) {
	app.checkHookRegistration(label, fn == nil)
	tier := TierInternal
	if registrationOptions(label, opts, optIngress).Ingress {
		tier = TierIngress
	}
	*hooks = append(*hooks, stageHook{index: len(*hooks), tier: tier, fn: fn})
}

// checkHookRegistration is the shared guard for every On* registration: the
// pre-compile window and a non-nil hook, so a programming error surfaces at
// registration rather than as a nil-function call during startup, reload, or
// teardown. label names the API as Type.Method.
func (app *App) checkHookRegistration(label string, fnIsNil bool) {
	app.checkFrozen(label)
	if fnIsNil {
		panic("credo: " + label + ": hook must not be nil")
	}
}

// registerHook appends a plain context hook after checkHookRegistration.
func (app *App) registerHook(label string, fn func(ctx context.Context) error, hooks *[]func(ctx context.Context) error) {
	app.checkHookRegistration(label, fn == nil)
	*hooks = append(*hooks, fn)
}

// Addr returns the actual network address the server is listening on.
// This is particularly useful when the server was started with port 0,
// as the OS assigns an ephemeral port.
// Returns nil before Run or after the server stops.
func (app *App) Addr() net.Addr {
	lm := app.lifecycle
	lm.serverMu.Lock()
	addr := lm.boundAddr
	lm.serverMu.Unlock()
	return addr
}

// Logger returns the application-level logger used by framework internals.
// Worker and other integration packages use this accessor to derive
// framework-scoped loggers without exposing raw logger registration in DI.
func (app *App) Logger() *slog.Logger {
	if app == nil || app.logger == nil {
		return defaultLogger
	}
	return app.logger
}

// IsDebug reports whether the application is running in debug mode.
// Debug mode enables development-time warnings (e.g., bind targets that
// do not implement Validatable). Activated via [WithDebug] or the
// server.debug config key.
func (app *App) IsDebug() bool {
	return app != nil && app.debug
}

// checkFrozen panics if the app has been frozen — prepared for serving or
// shut down during bootstrap. Used to guard against late registration of
// routes, middleware, hooks, and renderers. what names the API as Type.Method
// (for example "App.GET", "Group.SetMeta") so the panic text reads uniformly.
func (app *App) checkFrozen(what string) {
	if app.frozen.Load() {
		panic("credo: " + what + " called after app was compiled or shut down")
	}
}

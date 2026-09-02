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

// OnShutdown registers a function to be called during graceful shutdown.
// Hooks are called in LIFO order (last registered, first called).
// The ctx passed to each hook carries the shutdown deadline from Shutdown(ctx).
//
// Hooks run on every teardown, including a failed startup (an OnStart hook
// erroring after an earlier one ran). OnShutdown is the session teardown point,
// not an OnStart mirror, so hooks must be idempotent and must not assume any
// particular OnStart hook completed.
//
// Must be called before Run; panics for a nil hook or after the App is frozen.
func (app *App) OnShutdown(fn func(ctx context.Context) error) {
	app.registerHook("App.OnShutdown", fn, &app.lifecycle.onShutdown)
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

// OnPreDrain registers an early subsystem drain hook that runs after shutdown
// begins and readiness is withdrawn, but before the application lifecycle
// context is cancelled. Hooks run concurrently with one another; registration
// order is identity only, not execution order. The ctx carries the same
// absolute shutdown deadline used by every later teardown phase.
//
// A successful hook must stop its subsystem's admission and finish work that
// still depends on live background workers or DI infrastructure. Hooks also run
// during teardown after an OnStart failure, so they must be idempotent and must
// tolerate partial startup. If a hook is still running when ctx ends, Credo
// logs a waiting diagnostic but keeps the lifecycle context and infrastructure
// live until the hook actually returns. Its completion timestamp determines
// the final identified incomplete error. Later teardown then continues with
// the same, possibly expired context rather than racing the pre-drain work.
//
// Most subsystems should use [App.OnDrain], which runs after lifecycle
// cancellation and concurrently with HTTP drain. OnPreDrain is for the narrower
// case where cancellation itself would tear down a dependency too early.
//
// Must be called before Run; panics for a nil hook or after the App is frozen.
func (app *App) OnPreDrain(fn func(ctx context.Context) error) {
	app.checkHookRegistration("App.OnPreDrain", fn == nil)
	app.lifecycle.onPreDrain = append(
		app.lifecycle.onPreDrain,
		newDrainHook(len(app.lifecycle.onPreDrain), fn),
	)
}

// OnDrain registers a subsystem drain hook that runs after the application
// lifecycle context is cancelled and before DI infrastructure is shut down.
// Hooks run concurrently with one another and with HTTP server draining; no
// ordering between hooks is guaranteed. The ctx carries the same absolute
// shutdown deadline used by the HTTP drain.
//
// A successful hook must ensure its subsystem can no longer run handlers that
// depend on application infrastructure. Hooks also run during teardown after
// an OnStart failure, so they must be idempotent and must not assume their
// corresponding startup work completed. A hook that ignores ctx may outlive
// the shutdown budget; Credo reports it as incomplete and proceeds with the
// expired-context teardown contract rather than claiming graceful success.
//
// Must be called before Run; panics for a nil hook or after the App is frozen.
func (app *App) OnDrain(fn func(ctx context.Context) error) {
	app.checkHookRegistration("App.OnDrain", fn == nil)
	app.lifecycle.onDrain = append(
		app.lifecycle.onDrain,
		newDrainHook(len(app.lifecycle.onDrain), fn),
	)
}

// OnStart registers a function to be called during startup, after the port
// is bound but before the server starts accepting connections. Hooks are
// called in FIFO order (first registered, first called).
// The ctx passed to each hook is the lifecycle context (created at Run time).
// It is derived independently from any ctx passed to RunContext, so cancelling
// that ctx during startup does not abort a running hook; the cancellation is
// observed only after all hooks complete.
// If any hook returns an error, the server does not start: remaining hooks are
// skipped, the App runs the full teardown chain (the same as graceful shutdown,
// including DI shutdown and OnShutdown hooks), and Run returns the error. The
// App ends terminally stopped — a session that began tears down rather than
// rolling back, so it cannot be run again (create a new App).
// Typical uses include cache warm-up. The store/sqldb migration wrapper plugs
// in directly as app.OnStart(db.Migrate) for development and deliberate
// single-replica deployments; multi-replica production should use one
// deadline-bounded pre-deploy migration job instead.
// Must be called before Run; panics for a nil hook or after the App is frozen.
func (app *App) OnStart(fn func(ctx context.Context) error) {
	app.registerHook("App.OnStart", fn, &app.lifecycle.onStart)
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

// checkFrozen panics if the app has been compiled (frozen). Used to guard
// against late registration of routes, middleware, hooks, and renderers. what
// names the API as Type.Method (for example "App.GET", "Group.SetMeta") so the
// panic text reads uniformly.
func (app *App) checkFrozen(what string) {
	if app.frozen.Load() {
		panic("credo: " + what + " called after app was compiled")
	}
}

package credo

import (
	"context"
	"fmt"
	"log/slog"
	"net"
)

// State is a lifecycle state of an App, as [App.State] reports it. An App is
// single-use and moves forward through the states: building, starting,
// running, stopping, stopped. The one step back is a start that fails before
// anything was started, which returns from starting to building.
//
// The constants' values are the state names in lower case, so a State
// compares with a string literal and prints as one.
type State string

const (
	// StateBuilding is the initial state: registration is open and nothing
	// has started. Shutdown in this state is bootstrap teardown.
	StateBuilding State = "building"

	// StateStarting is the start phase: preparation, the listener bind under
	// managed serving, then the start walk. A shutdown request interrupts it.
	StateStarting State = "starting"

	// StateRunning means the start walk succeeded. Under managed serving the
	// server is accepting; after [App.Start] the App's owner serves it.
	StateRunning State = "running"

	// StateStopping means the drain in tiers, or the rollback of a failed or
	// interrupted start, is running.
	StateStopping State = "stopping"

	// StateStopped is the terminal state: the App has fully stopped and
	// cannot run again.
	StateStopped State = "stopped"
)

// State returns the current lifecycle state. It is a snapshot: another
// goroutine may move the App on before the caller acts on it. [App.IsRunning]
// asks the question most callers have.
func (app *App) State() State {
	return app.lifecycle.currentState().public()
}

// IsRunning reports whether the App is in [StateRunning].
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
// [CloseOnShutdown].
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

// warnUnclosedBindings warns in debug mode, once the drain has begun, of
// each binding whose value has a Close the App never calls: its type shows
// Close() error, Close() or Close(ctx) error, it claims nothing — no
// Shutdown, no [CloseOnShutdown], no [Borrowed] — it is not a [View], and no
// registration owns its resource. Close is never discovered (ADR-024), so
// such a binding stays open at shutdown; the warning names it and the
// remedies, one record per binding, carrying component=<name>.
func (app *App) warnUnclosedBindings() {
	if !app.debug {
		return
	}
	for _, b := range app.container.UnclosedBindings() {
		remedies := "credo.CloseOnShutdown() when the App should close it or its Close releases nothing, or " +
			"credo.View() when another registration owns what it holds"
		if b.ValueBinding {
			remedies = "credo.CloseOnShutdown() when the App should close it or its Close releases nothing, " +
				"credo.View() when another registration owns what it holds, or credo.Borrowed() when the caller " +
				"closes it"
		}
		app.logger.LogAttrs(context.Background(), slog.LevelWarn,
			fmt.Sprintf("credo: %s has %s, and the App does not close it at shutdown; give it %s", b.Registration,
				b.Close, remedies),
			slog.String("component", b.Name))
	}
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

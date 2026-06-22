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
// Must be called before Run; panics if called after compile.
func (app *App) OnShutdown(fn func(ctx context.Context) error) {
	app.checkFrozen("OnShutdown")
	app.lifecycle.onShutdown = append(app.lifecycle.onShutdown, fn)
}

// OnStart registers a function to be called during startup, after the port
// is bound but before the server starts accepting connections. Hooks are
// called in FIFO order (first registered, first called).
// The ctx passed to each hook is the app context (created at Run time). It is
// derived independently from any ctx passed to RunContext, so cancelling that
// ctx during startup does not abort a running hook; the cancellation is observed
// only after all hooks complete.
// If any hook returns an error, the server does not start: remaining hooks are
// skipped, the App runs the full teardown chain (the same as graceful shutdown,
// including DI shutdown and OnShutdown hooks), and Run returns the error. The
// App ends terminally stopped — a session that began tears down rather than
// rolling back, so it cannot be run again (create a new App).
// Typical uses are cache warm-up and database migrations — the store/sqldb
// migration wrapper plugs in directly: app.OnStart(db.Migrate).
// Must be called before Run; panics if called after compile.
func (app *App) OnStart(fn func(ctx context.Context) error) {
	app.checkFrozen("OnStart")
	app.lifecycle.onStart = append(app.lifecycle.onStart, fn)
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

// checkFrozen panics if the app has been compiled (frozen).
// Used to guard against late registration of routes, middleware, etc.
func (app *App) checkFrozen(what string) {
	if app.frozen.Load() {
		panic("credo: " + what + " called after app was compiled")
	}
}

package credo

import (
	"context"
	"log/slog"
	"net"
)

// appState represents the lifecycle state of an App.
type appState uint32

const (
	// stateBuilding is the initial state. Route and middleware registration is allowed.
	stateBuilding appState = iota

	// stateStarting is a transient state entered after the state CAS but before
	// the app context and *http.Server are written. Shutdown cannot operate in this
	// state, closing the race window between the CAS and the serverMu writes.
	stateStarting

	// stateRunning means the server is listening. Registration is frozen.
	// Shutdown may only be called in this state.
	stateRunning

	// stateStopping means the server is draining in-flight requests.
	stateStopping

	// stateStopped means the server has fully stopped.
	stateStopped
)

// String returns a human-readable name for the state.
func (s appState) String() string {
	switch s {
	case stateBuilding:
		return "building"
	case stateStarting:
		return "starting"
	case stateRunning:
		return "running"
	case stateStopping:
		return "stopping"
	case stateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// State returns the current lifecycle state as a string.
func (app *App) State() string {
	return appState(app.state.Load()).String()
}

// IsRunning reports whether the server is in the running state.
func (app *App) IsRunning() bool {
	return appState(app.state.Load()) == stateRunning
}

// Context returns the app-level context. This context is created when Run
// is called and cancelled at the beginning of Shutdown. Background services
// (cron jobs, pub/sub subscribers, gRPC servers) should select on
// ctx.Done() to detect graceful shutdown.
//
// WARNING: Context must be called after Run. Before Run returns (or from
// another goroutine before Run is invoked), Context returns
// context.Background(), which is never cancelled. Goroutines that capture
// this pre-run value will not receive the shutdown signal.
func (app *App) Context() context.Context {
	app.serverMu.Lock()
	ctx := app.ctx
	app.serverMu.Unlock()
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

// OnShutdown registers a function to be called during graceful shutdown.
// Hooks are called in LIFO order (last registered, first called).
// The ctx passed to each hook carries the shutdown deadline from Shutdown(ctx).
// Must be called before Run; panics if called after compile.
func (app *App) OnShutdown(fn func(ctx context.Context) error) {
	app.checkFrozen("OnShutdown")
	app.onShutdown = append(app.onShutdown, fn)
}

// OnStart registers a function to be called during startup, after the port
// is bound but before the server starts accepting connections. Hooks are
// called in FIFO order (first registered, first called).
// The ctx passed to each hook is the app context (created at Run time).
// If any hook returns an error, the server does not start and Run returns
// the error.
// Typical uses are cache warm-up and database migrations — the store/sqldb
// migration wrapper plugs in directly: app.OnStart(db.Migrate).
// Must be called before Run; panics if called after compile.
func (app *App) OnStart(fn func(ctx context.Context) error) {
	app.checkFrozen("OnStart")
	app.onStart = append(app.onStart, fn)
}

// Addr returns the actual network address the server is listening on.
// This is particularly useful when the server was started with port 0,
// as the OS assigns an ephemeral port.
// Returns nil before Run or after the server stops.
func (app *App) Addr() net.Addr {
	app.serverMu.Lock()
	addr := app.boundAddr
	app.serverMu.Unlock()
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

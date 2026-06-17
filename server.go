package credo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// ServeHTTP implements http.Handler. It compiles the handler chain on
// first call using sync.Once for thread safety.
func (app *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app.handlerOnce.Do(app.compile)
	if app.serverCfg.MaxBodyBytes > 0 && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, app.serverCfg.MaxBodyBytes)
	}
	c := app.ctxPool.get()
	c.reset(w, r)
	// Errors are handled inside the compiled handler chain by
	// builtinErrorHandler (non-panic) and builtinRecover (panic).
	// The chain always returns nil.
	_ = app.compiledHandler(c)
	app.ctxPool.put(c)
}

// Run starts the HTTP server and blocks until an interrupt (Ctrl+C) or
// SIGTERM is received, then performs graceful shutdown bounded by
// [WithShutdownTimeout]. A second signal during shutdown force-kills the
// process. Returns nil on graceful shutdown.
//
// Run is the safe default for a process whose lifetime is the server's. For
// explicit lifecycle control — tests, embedding, or caller-driven
// cancellation — use [App.RunContext].
func (app *App) Run() error {
	lm := app.lifecycle
	return lm.runSignal(func(ctx context.Context) error {
		return lm.serve(ctx, "Run", nil, tcpListen, plainServe)
	})
}

// RunContext starts the HTTP server and blocks until ctx is cancelled, the
// server stops, or a programmatic [App.Shutdown]. Unlike [App.Run] it installs
// no signal handler; cancellation is entirely the caller's. On ctx
// cancellation the drain keeps ctx's values but drops its cancellation
// (so an already-cancelled ctx still drains), bounded by [WithShutdownTimeout].
// Returns nil on graceful shutdown.
func (app *App) RunContext(ctx context.Context) error {
	return app.lifecycle.serve(ctx, "RunContext", nil, tcpListen, plainServe)
}

// RunTLS is the TLS sibling of [App.Run]: it serves HTTPS and blocks until a
// signal arrives, then shuts down gracefully. The certificate and key are
// validated before the server starts accepting connections, so a bad key pair
// fails fast. Returns nil on graceful shutdown.
func (app *App) RunTLS(certFile, keyFile string) error {
	lm := app.lifecycle
	return lm.runSignal(func(ctx context.Context) error {
		return lm.serve(ctx, "RunTLS", tlsPreflight(certFile, keyFile), tcpListen, tlsServe(certFile, keyFile))
	})
}

// RunTLSContext is the TLS sibling of [App.RunContext]: caller-driven
// cancellation, no signal handler. The certificate and key are validated
// before stateRunning, with the same fail-fast rollback as a listen error.
// Returns nil on graceful shutdown.
func (app *App) RunTLSContext(ctx context.Context, certFile, keyFile string) error {
	return app.lifecycle.serve(ctx, "RunTLSContext", tlsPreflight(certFile, keyFile), tcpListen, tlsServe(certFile, keyFile))
}

// ServeContext serves on a caller-provided listener, sharing the same
// lifecycle as [App.RunContext]. It is the escape hatch for listeners the
// framework does not create itself — Unix sockets, a preconfigured test
// listener, H2C, or an externally managed listener.
//
// ServeContext takes ownership of l: it is closed when the server stops,
// matching net/http.Server.Serve semantics. Returns nil on graceful shutdown.
func (app *App) ServeContext(ctx context.Context, l net.Listener) error {
	if l == nil {
		return errors.New("credo: ServeContext: nil listener")
	}
	return app.lifecycle.serve(ctx, "ServeContext", nil,
		func(*http.Server) (net.Listener, error) { return l, nil },
		plainServe,
	)
}

// Shutdown gracefully shuts down the server: it cancels the app context,
// drains in-flight requests, tears down DI singletons (reverse order), then
// runs OnShutdown hooks (LIFO). The caller's ctx carries the deadline; unlike
// signal/cancellation-triggered shutdown it is not bounded by
// [WithShutdownTimeout]. Returns an error if the server is not running, or if
// any shutdown step fails (joined via errors.Join).
func (app *App) Shutdown(ctx context.Context) error {
	lm := app.lifecycle
	err := lm.initiateShutdown(ctx)
	if errors.Is(err, errShutdownNotRunning) {
		return fmt.Errorf("credo: Shutdown: server in state %q, expected %q",
			lm.currentState(), stateRunning)
	}
	return err
}

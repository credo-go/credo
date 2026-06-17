package credo

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
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
	return app.runSignal(func(ctx context.Context) error {
		return app.serve(ctx, "Run", nil, tcpListen, plainServe)
	})
}

// RunContext starts the HTTP server and blocks until ctx is cancelled, the
// server stops, or a programmatic [App.Shutdown]. Unlike [App.Run] it installs
// no signal handler; cancellation is entirely the caller's. On ctx
// cancellation the drain keeps ctx's values but drops its cancellation
// (so an already-cancelled ctx still drains), bounded by [WithShutdownTimeout].
// Returns nil on graceful shutdown.
func (app *App) RunContext(ctx context.Context) error {
	return app.serve(ctx, "RunContext", nil, tcpListen, plainServe)
}

// RunTLS is the TLS sibling of [App.Run]: it serves HTTPS and blocks until a
// signal arrives, then shuts down gracefully. The certificate and key are
// validated before the server starts accepting connections, so a bad key pair
// fails fast. Returns nil on graceful shutdown.
func (app *App) RunTLS(certFile, keyFile string) error {
	return app.runSignal(func(ctx context.Context) error {
		return app.serve(ctx, "RunTLS", tlsPreflight(certFile, keyFile), tcpListen, tlsServe(certFile, keyFile))
	})
}

// RunTLSContext is the TLS sibling of [App.RunContext]: caller-driven
// cancellation, no signal handler. The certificate and key are validated
// before stateRunning, with the same fail-fast rollback as a listen error.
// Returns nil on graceful shutdown.
func (app *App) RunTLSContext(ctx context.Context, certFile, keyFile string) error {
	return app.serve(ctx, "RunTLSContext", tlsPreflight(certFile, keyFile), tcpListen, tlsServe(certFile, keyFile))
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
	return app.serve(ctx, "ServeContext", nil,
		func(*http.Server) (net.Listener, error) { return l, nil },
		plainServe,
	)
}

// tcpListen binds a TCP listener on the server's configured address.
func tcpListen(srv *http.Server) (net.Listener, error) {
	return net.Listen("tcp", srv.Addr)
}

// plainServe serves plaintext HTTP on l.
func plainServe(srv *http.Server, l net.Listener) error {
	return srv.Serve(l)
}

// tlsServe returns a serve function that serves HTTPS using the given files.
func tlsServe(certFile, keyFile string) func(*http.Server, net.Listener) error {
	return func(srv *http.Server, l net.Listener) error {
		return srv.ServeTLS(l, certFile, keyFile)
	}
}

// tlsPreflight returns a preflight that loads the key pair so an invalid
// certificate or key fails before the server reaches the running state.
func tlsPreflight(certFile, keyFile string) func() error {
	return func() error {
		if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			return fmt.Errorf("load TLS key pair: %w", err)
		}
		return nil
	}
}

// runSignal runs a context-aware run function under SIGINT/SIGTERM handling.
//
// The signal handler — not the run function — decides when to reset signal
// delivery and trigger shutdown. When the first signal arrives, stop() runs
// *before* the drain begins, so a second signal falls through to Go's default
// handler and force-kills the process (the standard two-stage Ctrl+C UX).
// This is why Run is not a naive `defer stop(); return RunContext(ctx)`
// wrapper: there, stop() would not run until after the drain, swallowing the
// second signal.
func (app *App) runSignal(run func(context.Context) error) error {
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// runCtx drives the run function's shutdown; we cancel it ourselves so the
	// drain context derives from Background (no signal cancellation leaks in).
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	errCh := make(chan error, 1)
	go func() { errCh <- run(runCtx) }()

	select {
	case err := <-errCh:
		// Server returned on its own: a serve error, a startup failure, or a
		// programmatic Shutdown from another goroutine. Nothing to drain here.
		return err
	case <-sigCtx.Done():
		stop()      // reset first: a second signal now force-kills the process
		cancelRun() // trigger the run function's graceful-drain path
		return <-errCh
	}
}

// errShutdownNotRunning is returned by initiateShutdown when the running →
// stopping CAS fails (the server is not running). Callers map it to either a
// user-facing error (Shutdown) or a no-op (a drain that lost the race).
var errShutdownNotRunning = errors.New("credo: shutdown: server not running")

// serve contains the shared lifecycle for every entry point: compile, DI
// finalize, single-use state claim, optional preflight, listen, startup hooks,
// serve, and graceful drain on context cancellation.
//
// State machine: building → starting → running → stopping → stopped
//
//	↘ (preflight/listen/OnStart/serve error) → building
//
// Race safety: stateStarting prevents Shutdown from reading nil ctx/server.
// stateRunning is stored only after the listener is bound and OnStart hooks
// pass, so IsRunning() truly means "accepting connections".
func (app *App) serve(
	ctx context.Context,
	label string,
	preflight func() error,
	listen func(*http.Server) (net.Listener, error),
	serveFn func(*http.Server, net.Listener) error,
) error {
	app.handlerOnce.Do(app.compile)

	// Implicit DI finalize (idempotent).
	if err := Finalize(app); err != nil {
		return fmt.Errorf("credo: %s: DI finalize: %w", label, err)
	}

	// Phase 1: claim the start slot. An App is single-use — once it has shut
	// down it cannot run again; callers must create a fresh App.
	if !app.state.CompareAndSwap(uint32(stateBuilding), uint32(stateStarting)) {
		switch appState(app.state.Load()) {
		case stateStopping, stateStopped:
			return fmt.Errorf("credo: %s: app cannot be run after shutdown; create a new App", label)
		default:
			return fmt.Errorf("credo: %s: server already in state %q", label, appState(app.state.Load()))
		}
	}

	// Phase 2: preflight checks that must fail before stateRunning (e.g. TLS
	// key-pair load), rolling back from stateStarting like a listen error.
	if preflight != nil {
		if err := preflight(); err != nil {
			app.state.CompareAndSwap(uint32(stateStarting), uint32(stateBuilding))
			return fmt.Errorf("credo: %s: %w", label, err)
		}
	}

	// Phase 3: build the server and publish ctx/cancel/server under serverMu
	// while Shutdown cannot proceed (stateStarting blocks it).
	appCtx, appCancel := context.WithCancel(context.Background())
	srv := buildServer(app.serverCfg, app)
	app.serverMu.Lock()
	app.ctx = appCtx
	app.cancel = appCancel
	app.server = srv
	app.serverMu.Unlock()

	// cleanup rolls back ctx and server fields on failure.
	cleanup := func() {
		appCancel()
		app.serverMu.Lock()
		app.ctx, app.cancel, app.server, app.boundAddr = nil, nil, nil, nil
		app.serverMu.Unlock()
	}

	// Phase 4: obtain the listener. Fail fast before stateRunning so a listen
	// error rolls back from stateStarting and Shutdown is never given a
	// partially-initialised server.
	l, err := listen(srv)
	if err != nil {
		cleanup()
		app.state.CompareAndSwap(uint32(stateStarting), uint32(stateBuilding))
		return err
	}
	defer l.Close() // safety net; Serve/ServeTLS close l themselves on return

	app.serverMu.Lock()
	app.boundAddr = l.Addr()
	app.serverMu.Unlock()

	// Phase 5: startup hooks (FIFO), before stateRunning to avoid racing
	// Shutdown. Hooks receive the app context, cancelled when shutdown begins.
	for i, fn := range app.onStart {
		if startErr := fn(app.ctx); startErr != nil {
			cleanup()
			app.state.CompareAndSwap(uint32(stateStarting), uint32(stateBuilding))
			return fmt.Errorf("credo: %s: OnStart hook [%d]: %w", label, i, startErr)
		}
	}

	// Phase 6: open the Shutdown gate. IsRunning() is now true.
	app.state.Store(uint32(stateRunning))
	app.logger.Info("credo: server started", "label", label, "addr", l.Addr().String())

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- serveFn(srv, l) }()

	select {
	case serveErr := <-serveErrCh:
		// Serve returned without us triggering shutdown. ErrServerClosed means
		// a programmatic Shutdown (from another goroutine) owns the drain and
		// the state transition — report graceful. Any other error is a serve
		// failure: roll back to building.
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		cleanup()
		app.state.CompareAndSwap(uint32(stateRunning), uint32(stateBuilding))
		return serveErr
	case <-ctx.Done():
		// Caller cancelled (or a signal, via runSignal). We own the drain. The
		// drain context drops the trigger's cancellation but keeps its values,
		// bounded by the configured shutdown timeout.
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), app.shutdownTimeout())
		defer cancel()
		shutErr := app.initiateShutdown(drainCtx)
		<-serveErrCh // Serve unwinds once initiateShutdown closes the server.
		if errors.Is(shutErr, errShutdownNotRunning) {
			// A programmatic Shutdown raced us and owns the result.
			return nil
		}
		return shutErr
	}
}

// Shutdown gracefully shuts down the server: it cancels the app context,
// drains in-flight requests, tears down DI singletons (reverse order), then
// runs OnShutdown hooks (LIFO). The caller's ctx carries the deadline; unlike
// signal/cancellation-triggered shutdown it is not bounded by
// [WithShutdownTimeout]. Returns an error if the server is not running, or if
// any shutdown step fails (joined via errors.Join).
func (app *App) Shutdown(ctx context.Context) error {
	err := app.initiateShutdown(ctx)
	if errors.Is(err, errShutdownNotRunning) {
		return fmt.Errorf("credo: Shutdown: server in state %q, expected %q",
			appState(app.state.Load()), stateRunning)
	}
	return err
}

// initiateShutdown is the single drain sequence shared by Shutdown and the
// context-cancellation path of serve. The running → stopping CAS makes it
// idempotent and is the sole source of truth for shutdown-once: concurrent
// callers (a cancelled context racing a programmatic Shutdown) cannot run the
// sequence twice. The loser receives errShutdownNotRunning.
func (app *App) initiateShutdown(ctx context.Context) error {
	if !app.state.CompareAndSwap(uint32(stateRunning), uint32(stateStopping)) {
		return errShutdownNotRunning
	}

	// Phase 0: stop reporting ready so load balancers drain this instance
	// before it stops accepting connections. Liveness stays up — the process
	// is alive, just no longer taking new work.
	app.draining.Store(true)

	var errs []error

	// Read cancel and server under the same lock that serve() wrote them.
	app.serverMu.Lock()
	cancelFn := app.cancel
	srv := app.server
	app.serverMu.Unlock()

	// 1. Cancel the app context — signals background services, and the context
	// handed to OnStart hooks, to begin stopping.
	if cancelFn != nil {
		cancelFn()
	}

	// 2. Drain in-flight HTTP requests (stop accepting, wait for handlers).
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("credo: server drain: %w", err))
		}
	}

	// 3. Tear down infrastructure — reverse-order DI singleton cleanup.
	if err := app.container.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}

	// 4. User shutdown hooks (LIFO) — ctx carries the drain deadline.
	for i := len(app.onShutdown) - 1; i >= 0; i-- {
		if err := app.onShutdown[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}

	app.serverMu.Lock()
	app.boundAddr = nil
	app.serverMu.Unlock()

	app.state.Store(uint32(stateStopped))
	return errors.Join(errs...)
}

// shutdownTimeout returns the configured graceful-shutdown budget, falling
// back to the default if unset (e.g. a zero-value App in a test).
func (app *App) shutdownTimeout() time.Duration {
	if app.serverCfg.ShutdownTimeout > 0 {
		return app.serverCfg.ShutdownTimeout
	}
	return defaultShutdownTimeout
}

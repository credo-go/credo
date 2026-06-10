package credo

import (
	"context"
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
	app.compiledHandler(c)
	app.ctxPool.put(c)
}

// Run starts the HTTP server using the address from server config.
// Returns nil if the server was shut down gracefully via Shutdown.
// Returns an error if the server is already running or fails to start.
func (app *App) Run() error {
	return app.runWith("Run",
		func(srv *http.Server) (net.Listener, error) {
			return net.Listen("tcp", srv.Addr)
		},
		func(srv *http.Server, l net.Listener) error {
			return srv.Serve(l)
		},
	)
}

// RunTLS starts the HTTPS server using the address from server config.
// Returns nil if the server was shut down gracefully via Shutdown.
// Returns an error if the server is already running or fails to start.
func (app *App) RunTLS(certFile, keyFile string) error {
	return app.runWith("RunTLS",
		func(srv *http.Server) (net.Listener, error) {
			return net.Listen("tcp", srv.Addr)
		},
		func(srv *http.Server, l net.Listener) error {
			return srv.ServeTLS(l, certFile, keyFile)
		},
	)
}

// runWith contains the shared Run/RunTLS logic: compile, CAS state
// transitions, server creation, listen, and rollback on failure.
//
// State machine: building → starting → running → stopping → stopped
//
//	↘ (listen/serve error) → building
//
// Race safety: stateStarting prevents Shutdown from reading nil ctx/server.
// stateRunning is stored only after net.Listen succeeds, so IsRunning()
// truly means "the port is bound and the server is accepting connections".
func (app *App) runWith(
	label string,
	listenFn func(*http.Server) (net.Listener, error),
	serveFn func(*http.Server, net.Listener) error,
) error {
	app.handlerOnce.Do(app.compile)

	// Implicit DI finalize (idempotent).
	if err := Finalize(app); err != nil {
		return fmt.Errorf("credo: %s: DI finalize: %w", label, err)
	}

	// Phase 1: claim the start slot. stateStarting blocks Shutdown from proceeding.
	if !app.state.CompareAndSwap(uint32(stateBuilding), uint32(stateStarting)) {
		return fmt.Errorf("credo: %s: server already in state %q", label, appState(app.state.Load()))
	}

	// Phase 2: write ctx and server under serverMu while Shutdown cannot proceed.
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

	// Phase 3: bind the port. Fail fast before claiming stateRunning so a
	// listen error rolls back from stateStarting rather than stateRunning,
	// and Shutdown is never given a chance to see a partially-initialised server.
	l, err := listenFn(srv)
	if err != nil {
		cleanup()
		app.state.CompareAndSwap(uint32(stateStarting), uint32(stateBuilding))
		return err
	}
	defer l.Close() // safety net; Serve/ServeTLS close l themselves on return

	// Phase 4a: store bound address and run startup hooks (FIFO).
	// Hooks run before stateRunning to avoid a race with Shutdown().
	app.serverMu.Lock()
	app.boundAddr = l.Addr()
	app.serverMu.Unlock()

	for i, fn := range app.onStart {
		if err := fn(appCtx); err != nil {
			cleanup()
			app.state.CompareAndSwap(uint32(stateStarting), uint32(stateBuilding))
			return fmt.Errorf("credo: %s: OnStart hook [%d]: %w", label, i, err)
		}
	}

	// Phase 4: port is bound — open the Shutdown gate. IsRunning() is now true.
	app.state.Store(uint32(stateRunning))

	app.logger.Info("credo: server started", "label", label, "addr", l.Addr().String())
	err = serveFn(srv, l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	// Serve failed unexpectedly — roll back. If Shutdown already ran (state ==
	// stopped), the CAS is a no-op and we leave the state as stopped, which
	// is correct: the server did shut down, even if there was also a serve error.
	cleanup()
	app.state.CompareAndSwap(uint32(stateRunning), uint32(stateBuilding))
	return err
}

// Shutdown gracefully shuts down the server. It drains in-flight requests,
// then calls registered OnShutdown hooks in LIFO order.
// Returns an error if the server is not in the running state, or if any
// shutdown step fails (all errors are collected via errors.Join).
func (app *App) Shutdown(ctx context.Context) error {
	if !app.state.CompareAndSwap(uint32(stateRunning), uint32(stateStopping)) {
		return fmt.Errorf("credo: Shutdown: server in state %q, expected %q",
			appState(app.state.Load()), stateRunning)
	}

	var errs []error

	// 1. Read cancel and server under the same lock (written by runWith).
	app.serverMu.Lock()
	cancelFn := app.cancel
	srv := app.server
	app.serverMu.Unlock()

	// 2. Cancel app context — signals background services to shut down.
	if cancelFn != nil {
		cancelFn()
	}

	// 3. HTTP server drain.
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("credo: server drain: %w", err))
		}
	}

	// 3.5. Container shutdown — reverse-order singleton cleanup.
	if err := app.container.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}

	// 4. Shutdown hooks (LIFO) — pass ctx for deadline awareness.
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

// RunWithSignals starts the HTTP server and blocks until a signal is received,
// then initiates graceful shutdown with the given timeout. If no signals are
// specified, defaults to [os.Interrupt] and [syscall.SIGTERM].
//
// This is a convenience wrapper around [App.Run] + [App.Shutdown] for the
// common case. For TLS, use [RunTLSWithSignals].
func RunWithSignals(app *App, timeout time.Duration, signals ...os.Signal) error {
	return runWithSignals(app.Run, app.Shutdown, timeout, signals...)
}

// RunTLSWithSignals starts the HTTPS server and blocks until a signal is
// received, then initiates graceful shutdown with the given timeout. If no
// signals are specified, defaults to [os.Interrupt] and [syscall.SIGTERM].
func RunTLSWithSignals(app *App, certFile, keyFile string, timeout time.Duration, signals ...os.Signal) error {
	return runWithSignals(
		func() error { return app.RunTLS(certFile, keyFile) },
		app.Shutdown, timeout, signals...,
	)
}

// runWithSignals is the shared implementation for RunWithSignals and
// RunTLSWithSignals. It starts runFn in a goroutine, waits for a signal
// or a run error, and calls shutdown with a deadline context on signal.
func runWithSignals(
	runFn func() error,
	shutdown func(context.Context) error,
	timeout time.Duration,
	signals ...os.Signal,
) error {
	if len(signals) == 0 {
		signals = []os.Signal{os.Interrupt, syscall.SIGTERM}
	}

	ctx, stop := signal.NotifyContext(context.Background(), signals...)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- runFn() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		stop() // Reset so second signal terminates process.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return shutdown(shutdownCtx)
	}
}

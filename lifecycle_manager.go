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
	"sync"
	"sync/atomic"
	"syscall"
	"time"
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

// lifecycleManager owns the App's server-session lifecycle: the state machine,
// the bound *http.Server and app context, the start/shutdown hooks, and the
// graceful-drain sequence shared by every Run* entry point and Shutdown.
//
// It is held by exactly one App and references it back through app for the
// cross-cutting pieces it needs (the compiled handler, DI container, server
// config, logger). Keeping these nine fields and the concurrency-sensitive
// drain logic in one type — rather than spread across the App struct — is the
// whole point of the split; the public Run/Shutdown/State/Addr/OnStart/
// OnShutdown methods on App stay as thin delegates onto this engine.
type lifecycleManager struct {
	// app is the owning application, used for compile, DI finalize/shutdown,
	// server config, and logging. Never nil for an App built by New.
	app *App

	// state tracks the lifecycle: building → starting → running → stopping → stopped.
	state atomic.Uint32

	// draining reports that graceful shutdown has begun. Set once at the start
	// of shutdown and read by the readiness handler, which then reports the
	// instance as unready so load balancers stop routing before the HTTP drain.
	draining atomic.Bool

	// serverMu protects server, ctx, cancel, and boundAddr.
	serverMu sync.Mutex

	// server holds the *http.Server created by serve.
	server *http.Server

	// ctx is the app-level context, created at Run() time. Cancelled at the
	// beginning of Shutdown(). Background services select on ctx.Done().
	ctx    context.Context
	cancel context.CancelFunc

	// boundAddr is the actual address from net.Listener.Addr(). Set after listen
	// succeeds, cleared on shutdown. Protected by serverMu.
	boundAddr net.Addr

	// onStart holds hooks called during startup after the port is bound (FIFO order).
	onStart []func(ctx context.Context) error

	// onShutdown holds hooks called during graceful shutdown (LIFO order).
	onShutdown []func(ctx context.Context) error
}

// currentState returns the current lifecycle state.
func (lm *lifecycleManager) currentState() appState {
	return appState(lm.state.Load())
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
func (lm *lifecycleManager) runSignal(run func(context.Context) error) error {
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
//	↘ pre-session failure (preflight/listen)              → building (may run again)
//	↘ session failure (OnStart hook / post-running serve) → drain → stopped (terminal)
//
// A pre-session failure rolls back to building because nothing has started; a
// session failure runs the full teardown and the App is terminal (ADR-006).
//
// Race safety: stateStarting prevents Shutdown from reading nil ctx/server.
// stateRunning is stored only after the listener is bound and OnStart hooks
// pass, so IsRunning() truly means "accepting connections".
func (lm *lifecycleManager) serve(
	ctx context.Context,
	label string,
	preflight func() error,
	listen func(*http.Server) (net.Listener, error),
	serveFn func(*http.Server, net.Listener) error,
) error {
	app := lm.app
	app.handlerOnce.Do(app.compile)

	// Implicit DI finalize (idempotent).
	if err := Finalize(app); err != nil {
		return fmt.Errorf("credo: %s: DI finalize: %w", label, err)
	}

	// Phase 1: claim the start slot. An App is single-use — once it has shut
	// down it cannot run again; callers must create a fresh App.
	if !lm.state.CompareAndSwap(uint32(stateBuilding), uint32(stateStarting)) {
		switch lm.currentState() {
		case stateStopping, stateStopped:
			return fmt.Errorf("credo: %s: app cannot be run after shutdown; create a new App", label)
		default:
			return fmt.Errorf("credo: %s: server already in state %q", label, lm.currentState())
		}
	}

	// Phase 2: preflight checks that must fail before stateRunning (e.g. TLS
	// key-pair load), rolling back from stateStarting like a listen error.
	if preflight != nil {
		if err := preflight(); err != nil {
			lm.state.CompareAndSwap(uint32(stateStarting), uint32(stateBuilding))
			return fmt.Errorf("credo: %s: %w", label, err)
		}
	}

	// Phase 3: build the server and publish ctx/cancel/server under serverMu
	// while Shutdown cannot proceed (stateStarting blocks it).
	appCtx, appCancel := context.WithCancel(context.Background())
	srv := buildServer(app.serverCfg, app)
	lm.serverMu.Lock()
	lm.ctx = appCtx
	lm.cancel = appCancel
	lm.server = srv
	lm.serverMu.Unlock()

	// cleanup rolls back the ctx and server fields for a pre-session failure (a
	// listen error): nothing has started, so there is nothing to drain and the
	// App stays in building, free to run again. Session failures (OnStart/serve)
	// instead run the full drain — see Phase 5 and Phase 6.
	cleanup := func() {
		appCancel()
		lm.serverMu.Lock()
		lm.ctx, lm.cancel, lm.server, lm.boundAddr = nil, nil, nil, nil
		lm.serverMu.Unlock()
	}

	// Phase 4: obtain the listener. Fail fast before stateRunning so a listen
	// error rolls back from stateStarting and Shutdown is never given a
	// partially-initialised server.
	l, err := listen(srv)
	if err != nil {
		cleanup()
		lm.state.CompareAndSwap(uint32(stateStarting), uint32(stateBuilding))
		return err
	}
	defer l.Close() // safety net; Serve/ServeTLS close l themselves on return

	lm.serverMu.Lock()
	lm.boundAddr = l.Addr()
	lm.serverMu.Unlock()

	// Phase 5: startup hooks (FIFO), before stateRunning to avoid racing
	// Shutdown. Hooks receive the app context, cancelled when shutdown begins.
	//
	// A hook failure here is a session failure, not a pre-session one: an
	// earlier hook may have produced externally visible side effects (started
	// workers, acquired a migration lock, opened a subscription). So we run the
	// full teardown chain — the same one a graceful shutdown runs — and the App
	// becomes terminally stopped (ADR-006), rather than rolling back to building.
	// State is stateStarting, so a concurrent Shutdown (which requires
	// stateRunning) cannot race this drain; we store stateStopping and drain
	// directly instead of going through initiateShutdown's CAS.
	for i, fn := range lm.onStart {
		startErr := fn(lm.ctx)
		if startErr == nil {
			continue
		}
		lm.state.Store(uint32(stateStopping))
		// Serve never started, so drain's srv.Shutdown cannot close the bound
		// listener. Close it now — before the possibly slow DI/OnShutdown
		// teardown — so the port is released promptly, matching the graceful path
		// where srv.Shutdown closes the listener before those steps. The deferred
		// l.Close() then no-ops.
		l.Close()
		drainCtx, drainCancel := context.WithTimeout(context.Background(), lm.shutdownTimeout())
		teardownErr := lm.drain(drainCtx)
		drainCancel()
		err := fmt.Errorf("credo: %s: OnStart hook [%d]: %w", label, i, startErr)
		if teardownErr != nil {
			err = errors.Join(err, teardownErr)
		}
		return err
	}

	// Phase 6: open the Shutdown gate. IsRunning() is now true.
	lm.state.Store(uint32(stateRunning))
	app.logger.Info("credo: server started", "label", label, "addr", l.Addr().String())

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- serveFn(srv, l) }()

	select {
	case serveErr := <-serveErrCh:
		// Serve returned without us triggering shutdown. ErrServerClosed means
		// a programmatic Shutdown (from another goroutine) owns the drain and
		// the state transition — report graceful.
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		// A real serve failure on a running app is a terminal session failure
		// (ADR-006): run the same teardown as a graceful shutdown, then report
		// the serve error. initiateShutdown's running → stopping CAS claims
		// ownership against a racing programmatic Shutdown; if it lost the race
		// (errShutdownNotRunning), that caller owns the drain and we report only
		// the serve error.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), lm.shutdownTimeout())
		teardownErr := lm.initiateShutdown(drainCtx)
		drainCancel()
		if teardownErr != nil && !errors.Is(teardownErr, errShutdownNotRunning) {
			return errors.Join(serveErr, teardownErr)
		}
		return serveErr
	case <-ctx.Done():
		// Caller cancelled (or a signal, via runSignal). We own the drain. The
		// drain context drops the trigger's cancellation but keeps its values,
		// bounded by the configured shutdown timeout.
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lm.shutdownTimeout())
		defer cancel()
		shutErr := lm.initiateShutdown(drainCtx)
		<-serveErrCh // Serve unwinds once initiateShutdown closes the server.
		if errors.Is(shutErr, errShutdownNotRunning) {
			// A programmatic Shutdown raced us and owns the result.
			return nil
		}
		return shutErr
	}
}

// initiateShutdown is the CAS-guarded entry to the shared drain sequence. The
// running → stopping CAS is the sole source of truth for shutdown-once:
// concurrent callers (a cancelled context, a serve failure, and a programmatic
// Shutdown racing each other) cannot run the sequence twice. The loser receives
// errShutdownNotRunning. The startup-failure path does not go through here — it
// is in stateStarting, where no Shutdown can race it, so it calls drain directly.
func (lm *lifecycleManager) initiateShutdown(ctx context.Context) error {
	if !lm.state.CompareAndSwap(uint32(stateRunning), uint32(stateStopping)) {
		return errShutdownNotRunning
	}
	return lm.drain(ctx)
}

// drain runs the teardown chain shared by every shutdown path — graceful
// Shutdown, context cancellation, a runtime serve failure, and a failed startup:
// mark unready, cancel the app context, drain the HTTP server, tear down DI
// singletons (reverse order), run OnShutdown hooks (LIFO), release the
// server-session references, and store stateStopped.
//
// The caller must have already moved the state out of the live states
// (running/starting) so drain runs exactly once: initiateShutdown does this via
// its running → stopping CAS; the startup-failure path stores stateStopping
// directly (a non-running app cannot be reached by Shutdown).
//
// OnShutdown hooks run on *every* teardown, including a failed startup. They are
// the session teardown point, not an OnStart mirror, so they must be idempotent
// and must not assume any particular OnStart hook completed (ADR-006).
func (lm *lifecycleManager) drain(ctx context.Context) error {
	// Phase 0: stop reporting ready so load balancers drain this instance
	// before it stops accepting connections. Liveness stays up — the process
	// is alive, just no longer taking new work.
	lm.draining.Store(true)

	var errs []error

	// Read cancel and server under the same lock that serve() wrote them.
	lm.serverMu.Lock()
	cancelFn := lm.cancel
	srv := lm.server
	lm.serverMu.Unlock()

	// 1. Cancel the app context — signals background services, and the context
	// handed to OnStart hooks, to begin stopping.
	if cancelFn != nil {
		cancelFn()
	}

	// 2. Drain in-flight HTTP requests (stop accepting, wait for handlers). On a
	// failed-startup teardown Serve was never called, so srv has no registered
	// listener and this is a near no-op; serve()'s deferred l.Close() closes the
	// bound listener.
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("credo: server drain: %w", err))
		}
	}

	// 3. Tear down infrastructure — reverse-order DI singleton cleanup.
	if err := lm.app.container.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}

	// 4. User shutdown hooks (LIFO) — ctx carries the drain deadline.
	for i := len(lm.onShutdown) - 1; i >= 0; i-- {
		if err := lm.onShutdown[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}

	// Release the server-session references, mirroring the pre-session cleanup
	// in serve(). The App is single-use, so nothing reads these after stopped;
	// dropping them lets the closed server and cancelled context be collected.
	lm.serverMu.Lock()
	lm.ctx, lm.cancel, lm.server, lm.boundAddr = nil, nil, nil, nil
	lm.serverMu.Unlock()

	lm.state.Store(uint32(stateStopped))
	return errors.Join(errs...)
}

// shutdownTimeout returns the configured graceful-shutdown budget, falling
// back to the default if unset (e.g. a zero-value App in a test).
func (lm *lifecycleManager) shutdownTimeout() time.Duration {
	if lm.app.serverCfg.ShutdownTimeout > 0 {
		return lm.app.serverCfg.ShutdownTimeout
	}
	return defaultShutdownTimeout
}

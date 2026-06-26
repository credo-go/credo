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
	// the lifecycle context and *http.Server are written. Shutdown cannot operate
	// in this state, closing the race window between the CAS and the serverMu writes.
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
// the bound *http.Server and lifecycle context, the start/shutdown hooks, and the
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

	// redirectServer holds the optional HTTP→HTTPS redirect server created by
	// serve when WithHTTPRedirect is set. nil when no redirect listener runs.
	// Protected by serverMu; drained alongside server.
	redirectServer *http.Server

	// ctx is the lifecycle context, created at Run() time. Cancelled at the
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

// resolveTLSConfig returns the effective *tls.Config to serve with, or nil for
// plaintext. Precedence: WithTLSConfig > WithTLSFiles / server.tls.* > plaintext.
// All TLS validation lives here so it runs at preflight (Phase 2 of serve) and a
// failure rolls the lifecycle back to building, exactly like a listen error.
func (app *App) resolveTLSConfig() (*tls.Config, error) {
	// Highest precedence: a caller-supplied *tls.Config (WithTLSConfig). Because
	// the option records that it was set, an explicit nil is a fail-fast misuse
	// error rather than a silent fall through to the lower-precedence sources.
	if app.tlsConfigSet {
		if app.tlsConfig == nil {
			return nil, errors.New("WithTLSConfig given a nil *tls.Config")
		}
		// Clone it so we validate and serve a snapshot — the caller's live
		// pointer is never bound to the runtime server, and later caller
		// mutations do not affect serving.
		cfg := app.tlsConfig.Clone()
		// Mirror net/http (*Server).ServeTLS's configHasCert check
		// (Go 1.26 net/http/server.go): any of these three is a valid source.
		if len(cfg.Certificates) == 0 && cfg.GetCertificate == nil && cfg.GetConfigForClient == nil {
			return nil, errors.New("WithTLSConfig has no certificate source (set Certificates, GetCertificate, or GetConfigForClient)")
		}
		return cfg, nil
	}

	// File-based TLS, from WithTLSFiles or the server.tls.* config keys (already
	// merged in New, WithTLSFiles winning). Both paths required, or neither.
	cf, kf := app.serverCfg.TLS.CertFile, app.serverCfg.TLS.KeyFile

	// WithTLSFiles was called explicitly: empty paths are a misuse error, not a
	// silent fall through to plaintext — symmetric with the cert-source check
	// above and the partial-pair guard below.
	if app.tlsFilesSet && (cf == "" || kf == "") {
		return nil, fmt.Errorf("WithTLSFiles given an empty cert or key path (cert=%q key=%q)", cf, kf)
	}

	switch {
	case cf == "" && kf == "":
		return nil, nil // plaintext
	case cf == "" || kf == "":
		return nil, fmt.Errorf("TLS requires both cert_file and key_file (cert=%q key=%q)", cf, kf)
	}
	cert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// serveFuncs builds the (preflight, serveFn) pair shared by Run and RunContext.
// Preflight resolves the TLS config once — before stateRunning, so an invalid
// certificate fails fast and rolls back to building. serveFn then serves HTTPS
// with that resolved config when TLS is configured, or plaintext otherwise. The
// key pair is loaded exactly once: ServeTLS reuses srv.TLSConfig rather than
// reading the files again.
func (app *App) serveFuncs() (func() error, func(*http.Server, net.Listener) error) {
	var cfg *tls.Config
	preflight := func() error {
		c, err := app.resolveTLSConfig()
		if err != nil {
			return err
		}
		if c == nil && app.httpRedirectAddr != "" {
			return errors.New("WithHTTPRedirect requires TLS (set WithTLSFiles, WithTLSConfig, or server.tls.*)")
		}
		cfg = c
		return nil
	}
	serveFn := func(srv *http.Server, l net.Listener) error {
		if cfg != nil {
			srv.TLSConfig = cfg
			return srv.ServeTLS(l, "", "")
		}
		return srv.Serve(l)
	}
	return preflight, serveFn
}

// httpRedirectHandler returns a handler that permanently redirects every request
// to its HTTPS equivalent. It reuses the request host (its port stripped) and
// appends httpsPort unless that is empty or 443. GET and HEAD get 301; other
// methods get 308 so the method and body survive the redirect. Used by the
// optional WithHTTPRedirect listener.
func httpRedirectHandler(httpsPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		target := "https://" + host
		if httpsPort != "" && httpsPort != "443" {
			target += ":" + httpsPort
		}
		target += r.URL.RequestURI()

		code := http.StatusMovedPermanently // 301 for GET/HEAD
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			code = http.StatusPermanentRedirect // 308 preserves method and body
		}
		http.Redirect(w, r, target, code) //nolint:gosec // HTTP-to-HTTPS redirect intentionally preserves the request host.
	})
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
	redirectAddr string,
) error {
	app := lm.app
	app.handlerOnce.Do(app.compile)

	// Implicit DI finalize (idempotent).
	if err := app.Finalize(); err != nil {
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

	// Phase 4b: optional HTTP→HTTPS redirect listener (WithHTTPRedirect). Bind
	// fail-fast like the main listener — a pre-session failure rolls back to
	// building. The redirect target reuses the main listener's (HTTPS) port.
	// ServeContext passes an empty redirectAddr, so it stays redirect-exempt.
	//
	// redirectErrCh stays nil when no redirect listener runs; a nil channel never
	// fires in the Phase 6 select. When the listener does run, a runtime Serve
	// failure (anything but the ErrServerClosed of a graceful drain) is reported
	// here and handled like a main serve failure: the operator opted into the
	// redirect, so its silent loss must not leave the app reporting healthy.
	var redirectErrCh chan error
	if redirectAddr != "" {
		rl, rerr := net.Listen("tcp", redirectAddr)
		if rerr != nil {
			cleanup()
			l.Close()
			lm.state.CompareAndSwap(uint32(stateStarting), uint32(stateBuilding))
			return fmt.Errorf("credo: %s: HTTP redirect listen on %q: %w", label, redirectAddr, rerr)
		}
		_, httpsPort, _ := net.SplitHostPort(l.Addr().String())
		rsrv := &http.Server{
			Handler:           httpRedirectHandler(httpsPort),
			ReadHeaderTimeout: app.serverCfg.ReadHeaderTimeout,
		}
		lm.serverMu.Lock()
		lm.redirectServer = rsrv
		lm.serverMu.Unlock()
		redirectErrCh = make(chan error, 1)
		go func() {
			if rserveErr := rsrv.Serve(rl); rserveErr != nil && !errors.Is(rserveErr, http.ErrServerClosed) {
				redirectErrCh <- rserveErr
			}
		}()
	}

	// Phase 5: startup hooks (FIFO), before stateRunning to avoid racing
	// Shutdown. Hooks receive the lifecycle context, cancelled when shutdown begins.
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
	case redirectErr := <-redirectErrCh:
		// The redirect listener died at runtime. Handle it like a main serve
		// failure (ADR-006 terminal session failure): drain — which stops the
		// main server — and report the redirect error. The running → stopping
		// CAS in initiateShutdown guards against a racing programmatic Shutdown;
		// draining serveErrCh lets the main serve goroutine unwind.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), lm.shutdownTimeout())
		teardownErr := lm.initiateShutdown(drainCtx)
		drainCancel()
		<-serveErrCh
		redirectFail := fmt.Errorf("credo: %s: HTTP redirect server failed: %w", label, redirectErr)
		if teardownErr != nil && !errors.Is(teardownErr, errShutdownNotRunning) {
			return errors.Join(redirectFail, teardownErr)
		}
		return redirectFail
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
// mark unready, cancel the lifecycle context, drain the HTTP server, tear down DI
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

	// Read cancel and servers under the same lock that serve() wrote them.
	lm.serverMu.Lock()
	cancelFn := lm.cancel
	srv := lm.server
	redirectSrv := lm.redirectServer
	lm.serverMu.Unlock()

	// 1. Cancel the lifecycle context — signals background services, and the context
	// handed to OnStart hooks, to begin stopping.
	if cancelFn != nil {
		cancelFn()
	}

	// 2. Drain the HTTP servers (stop accepting, wait for in-flight handlers).
	// Stop the redirect listener first so that, during the main server's drain
	// window, no client is redirected to an HTTPS server that has just stopped
	// accepting connections; a redirect response is instant, so its Shutdown
	// returns near-immediately. On a failed-startup teardown Serve was never
	// called, so the main srv has no registered listener and its Shutdown is a
	// near no-op — serve()'s deferred l.Close() closes the bound listener.
	if redirectSrv != nil {
		if err := redirectSrv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("credo: HTTP redirect drain: %w", err))
		}
	}
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
	lm.ctx, lm.cancel, lm.server, lm.redirectServer, lm.boundAddr = nil, nil, nil, nil, nil
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

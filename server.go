package credo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// ServeHTTP implements http.Handler. The first call prepares the App —
// Finalize, compile, publish — and stores the result; a preparation error is
// a developer error and panics on every request rather than being retried.
// Requests are admitted by lifecycle state: a stopped App, or a stopping App
// that was never prepared, receives the default 503 envelope without touching
// DI or any configured callback, while an already-prepared handler keeps
// serving during the managed drain. Direct ServeHTTP never claims the managed
// server's start slot; an external http.Server stays its owner's job to drain
// before [App.Shutdown].
//
// An App with something to start — a component with Start or Ready, a start
// hook, a constructor handed to [App.Manage], a store registration or
// [App.UseI18n] catalogs — refuses to serve until the
// start phase has succeeded: before [App.Start] (or while it runs) every
// request panics with a message naming App.Start and testutil.Start, and
// after a failed or interrupted App.Start the stopped App answers with the
// 503 envelope. Under Run, RunContext and ServeContext the listener accepts
// only after the start phase, so the gate never fires; a child App mounted
// into a parent is handed to the parent with Manage for the same reason.
func (app *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	state := app.lifecycle.currentState()
	if state == stateStopped {
		app.rejectUnavailable(w, r, state)
		return
	}
	p := app.prep.Load()
	if p == nil {
		if state >= stateStopping {
			app.rejectUnavailable(w, r, state)
			return
		}
		if p = app.prepare(); p == nil {
			app.rejectUnavailable(w, r, app.lifecycle.currentState())
			return
		}
	}
	if p.err != nil {
		panic(p.err)
	}
	if p.needsStart && !app.lifecycle.started.Load() {
		if state = app.lifecycle.currentState(); state >= stateStopping {
			app.rejectUnavailable(w, r, state)
			return
		}
		panic(errNotStarted)
	}
	// http.NoBody (every bodyless request the stdlib server delivers) has
	// nothing to limit; skipping the wrap saves an allocation per request.
	if app.serverCfg.MaxBodyBytes > 0 && r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, app.serverCfg.MaxBodyBytes)
	}
	c := app.ctxPool.get()
	c.reset(w, r)
	// The executor owns everything from here: feature stages, the chain,
	// error and panic handling, finalization and the Context's release.
	app.execute(c, p.handler)
}

// Run binds the listener, runs the start phase — each component's Start in
// dependency order, then the start hooks — serves, and blocks until an
// interrupt (Ctrl+C) or SIGTERM is received, then drains in tiers under the
// deadline set by [WithShutdownTimeout]. A signal during the start phase
// interrupts it: nothing further starts, what was built is rolled back, and
// Run returns nil after a clean rollback. A second signal force-kills the
// process. Returns nil on graceful shutdown, and a *LifecycleError when the
// start fails or a drain or rollback does not complete.
//
// On Unix, Run also handles SIGHUP: each signal triggers [App.Reload] under
// the [WithReloadTimeout] budget (systemctl reload, logrotate postrotate).
// Reloads run one at a time, signals that arrive during a reload coalesce
// into at most one follow-up, and a failed reload is logged but never stops
// the server. There is no SIGHUP on Windows; there, and under [App.RunContext]
// on every platform, the programmatic [App.Reload] is the only trigger.
//
// Run serves HTTPS automatically when TLS is configured via [WithTLSFiles],
// [WithTLSConfig], or the server.tls.* config keys; otherwise it serves
// plaintext. A misconfigured certificate (missing file, mismatched pair, or a
// WithTLSConfig with no certificate source) fails fast before the server
// accepts connections, rolling the lifecycle back so the App can run again.
//
// Run is the safe default for a process whose lifetime is the server's. For
// explicit lifecycle control — tests, embedding, or caller-driven
// cancellation — use [App.RunContext].
func (app *App) Run() error {
	lm := app.lifecycle
	preflight, serveFn := app.serveFuncs()
	return lm.runSignal(func(ctx context.Context) error {
		return lm.serve(ctx, "Run", preflight, tcpListen, serveFn, app.httpRedirectAddr)
	})
}

// RunContext starts the HTTP server and blocks until ctx is cancelled, the
// server stops, or a programmatic [App.Shutdown]. Unlike [App.Run] it installs
// no signal handler; cancellation is entirely the caller's. On ctx
// cancellation the drain keeps ctx's values but drops its cancellation and
// applies the [WithShutdownTimeout] deadline. Returns nil on graceful
// shutdown.
//
// Like [App.Run], RunContext serves HTTPS when TLS is configured (via
// [WithTLSFiles], [WithTLSConfig], or server.tls.*) and plaintext otherwise,
// with the same fail-fast certificate validation.
//
// Cancelling ctx during the start phase interrupts it: the running Start's or
// start hook's context is cancelled, nothing further starts, what was built
// is rolled back, the listener never accepts, and RunContext returns nil
// after a clean rollback.
func (app *App) RunContext(ctx context.Context) error {
	preflight, serveFn := app.serveFuncs()
	return app.lifecycle.serve(ctx, "RunContext", preflight, tcpListen, serveFn, app.httpRedirectAddr)
}

// ServeContext serves on a caller-provided listener, sharing the same
// lifecycle as [App.RunContext]. It is the escape hatch for listeners the
// framework does not create itself — Unix sockets, a preconfigured test
// listener, or an externally managed listener. It supplies the listener only;
// the server itself is still the one the framework builds, so protocol-level
// settings such as H2C come from [WithHTTPServer]:
//
//	credo.WithHTTPServer(func(s *http.Server) {
//		s.Protocols = new(http.Protocols)
//		s.Protocols.SetHTTP1(true)
//		s.Protocols.SetUnencryptedHTTP2(true)
//	})
//
// ServeContext takes ownership of l: it is closed when the server stops,
// matching net/http.Server.Serve semantics. Returns nil on graceful shutdown.
//
// ServeContext serves l exactly as given and is TLS-exempt: TLS configured via
// [WithTLSFiles] or [WithTLSConfig] does not apply here, nor does the
// [WithHTTPRedirect] listener. For HTTPS on a custom listener, wrap it yourself
// — e.g. tls.NewListener(l, cfg).
func (app *App) ServeContext(ctx context.Context, l net.Listener) error {
	if l == nil {
		return errors.New("credo: ServeContext: nil listener")
	}
	return app.lifecycle.serve(ctx, "ServeContext", nil,
		func(*http.Server) (net.Listener, error) { return l, nil },
		plainServe, "",
	)
}

// Shutdown gracefully shuts down the App: it withdraws readiness; drains
// in-flight HTTP requests concurrently with the ingress tier (ingress stop
// hooks LIFO, then ingress components); waits for an in-flight reload; then
// stops the internal tier — internal stop hooks LIFO, then internal
// components in reverse dependency order. The caller's ctx carries the one
// deadline the steps share; [WithShutdownTimeout] does not replace it. A
// component or hook that has not returned at the deadline is abandoned and
// the components it depends on stay open. Returns the HTTP drain's error, the
// reload's and a *LifecycleError, joined, when a step fails or remains
// incomplete.
//
// Shutdown is also accepted on an App that was never run: bootstrap teardown
// closes route and DI registration, runs the same drain with no managed
// server, and tears down every singleton that exists — including values
// registered by a composition root whose [App.Finalize] never ran or failed.
// This is the cleanup path for tests and for Apps served through an external
// http.Server; that server's admission and drain remain its owner's job and
// must complete before Shutdown. The App is single-use: the terminal state is
// stopped even when cleanup was incomplete.
//
// Shutdown on a starting App interrupts the start phase and waits, within
// ctx, for the rollback; it returns the rollback's lifecycle error, nil when
// the rollback was clean. Shutdown returns an error when the App is already
// stopping or stopped; a start that reaches running (or rolls back to
// building) while Shutdown is deciding is claimed by the same call rather
// than refused with a stale state.
func (app *App) Shutdown(ctx context.Context) error {
	lm := app.lifecycle
	for {
		err := lm.initiateShutdown(ctx)
		if !errors.Is(err, errShutdownNotRunning) {
			return err
		}
		if claimed, bootstrapErr := lm.initiateBootstrapShutdown(ctx); claimed {
			return bootstrapErr
		}
		// Both claims lost. The state read here decides the outcome: a live
		// state means a transition landed between the two attempts (starting
		// became running, or a start rolled back to building), so claim
		// again; starting is interrupted; anything else is a genuine refusal.
		lm.serverMu.Lock()
		state, run := lm.currentState(), lm.run
		lm.serverMu.Unlock()
		switch state {
		case stateRunning, stateBuilding:
			continue
		case stateStarting:
			if retry, err := lm.interruptStart(ctx, run); !retry {
				return err
			}
			continue
		}
		return fmt.Errorf("credo: Shutdown: server in state %q, expected %q, %q or %q",
			state, stateBuilding, stateStarting, stateRunning)
	}
}

// errNotStarted is the ServeHTTP panic of an App with something to start that
// has not been started.
var errNotStarted = errors.New("credo: ServeHTTP: the App has start work (components, start hooks, " +
	"store registrations or UseI18n) and has not been started; call App.Start before serving it " +
	"through ServeHTTP (testutil.Start in tests), serve it with Run, RunContext or ServeContext, or, " +
	"for a mounted child App, hand it to the parent with parent.Manage(child)")

// interruptStart interrupts the start phase run on behalf of Shutdown and
// waits, within ctx, for its rollback. retry is true when the start phase
// ended before the interrupt landed — it reached running, or rolled back to
// building — so the caller claims the new state.
func (lm *lifecycleManager) interruptStart(ctx context.Context, run *startRun) (retry bool, err error) {
	if !run.interrupt(ctx, nil, errShutdownWhileStarting) {
		select {
		case <-run.done:
			return true, nil
		case <-ctx.Done():
			return false, fmt.Errorf("credo: Shutdown: the start phase is still ending: %w", ctx.Err())
		}
	}
	select {
	case <-run.done:
		return false, run.err
	case <-ctx.Done():
		return false, fmt.Errorf("credo: Shutdown: the start phase is still rolling back: %w", ctx.Err())
	}
}
